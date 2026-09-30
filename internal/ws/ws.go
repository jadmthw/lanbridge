// Package ws is a small RFC 6455 WebSocket implementation that exposes a
// connection as a plain byte stream (a net.Conn). It lets a relay run behind
// HTTPS-only hosting and pass through networks that only allow web traffic.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	guid        = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	opCont      = 0x0
	opText      = 0x1
	opBinary    = 0x2
	opClose     = 0x8
	opPing      = 0x9
	opPong      = 0xA
	maxFrame    = 32 << 10 // size of outgoing data frames
	maxIncoming = 16 << 20 // refuse absurdly large frames
)

// UserAgent is sent by Dial.
var UserAgent = "lanbridge"

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(h[:])
}

func hasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// IsUpgrade reports whether r asks to switch to WebSocket.
func IsUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && hasToken(r.Header, "Connection", "upgrade") && hasToken(r.Header, "Upgrade", "websocket")
}

// Accept completes the server side of a handshake for a request that was read from br.
func Accept(c net.Conn, br *bufio.Reader, r *http.Request) (net.Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !IsUpgrade(r) || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return nil, errors.New("websocket: bad handshake")
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := io.WriteString(c, resp); err != nil {
		return nil, err
	}
	return newConn(c, br, false), nil
}

// Dial opens a ws:// or wss:// URL. network is "tcp", "tcp4" or "tcp6".
func Dial(ctx context.Context, network, rawURL string) (net.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	secure := false
	switch u.Scheme {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if secure {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 20 * time.Second}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if secure {
		tc := tls.Client(c, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, err
		}
		c = tc
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	conn, err := clientHandshake(c, u)
	if !stop() && err == nil {
		err = context.Cause(ctx)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return conn, nil
}

func clientHandshake(c net.Conn, u *url.URL) (net.Conn, error) {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key +
		"\r\nSec-WebSocket-Version: 13\r\nUser-Agent: " + UserAgent + "\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(c, 4096)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("websocket handshake: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("websocket handshake: server answered %q", resp.Status)
	}
	if !hasToken(resp.Header, "Upgrade", "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		return nil, errors.New("websocket handshake: invalid response")
	}
	_ = c.SetDeadline(time.Time{})
	return newConn(c, br, true), nil
}

// Conn is a WebSocket connection carrying a byte stream in binary frames.
type Conn struct {
	c      net.Conn
	br     *bufio.Reader
	client bool

	wmu       sync.Mutex
	closeOnce sync.Once
	closeSent atomic.Bool

	// read state; a Conn supports one reader at a time
	remaining int64
	masked    bool
	mask      [4]byte
	maskPos   int
	eof       bool
}

func newConn(c net.Conn, br *bufio.Reader, client bool) *Conn {
	return &Conn{c: c, br: br, client: client}
}

func (w *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for w.remaining == 0 {
		if w.eof {
			return 0, io.EOF
		}
		if err := w.nextFrame(); err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > w.remaining {
		p = p[:w.remaining]
	}
	n, err := w.br.Read(p)
	if n > 0 {
		if w.masked {
			for i := 0; i < n; i++ {
				p[i] ^= w.mask[(w.maskPos+i)&3]
			}
			w.maskPos = (w.maskPos + n) & 3
		}
		w.remaining -= int64(n)
	}
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (w *Conn) nextFrame() error {
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.br, h[:]); err != nil {
			return err
		}
		op := h[0] & 0x0F
		masked := h[1]&0x80 != 0
		n := int64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return unexpected(err)
			}
			n = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return unexpected(err)
			}
			u := binary.BigEndian.Uint64(b[:])
			if u > maxIncoming {
				return errors.New("websocket: frame too large")
			}
			n = int64(u)
		}
		var key [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, key[:]); err != nil {
				return unexpected(err)
			}
		}
		switch op {
		case opCont, opText, opBinary:
			if n == 0 {
				continue
			}
			w.remaining, w.masked, w.mask, w.maskPos = n, masked, key, 0
			return nil
		case opClose, opPing, opPong:
			if n > 125 {
				return errors.New("websocket: control frame too large")
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(w.br, payload); err != nil {
				return unexpected(err)
			}
			if masked {
				for i := range payload {
					payload[i] ^= key[i&3]
				}
			}
			switch op {
			case opPing:
				w.wmu.Lock()
				err := w.writeFrame(opPong, payload)
				w.wmu.Unlock()
				if err != nil {
					return err
				}
			case opClose:
				w.eof = true
				w.sendClose()
				return io.EOF
			}
		default:
			return fmt.Errorf("websocket: unexpected opcode %d", op)
		}
	}
}

// writeFrame must be called with wmu held.
func (w *Conn) writeFrame(op byte, payload []byte) error {
	n := len(payload)
	buf := make([]byte, 0, 14+n)
	buf = append(buf, 0x80|op)
	var maskBit byte
	if w.client {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		buf = append(buf, maskBit|byte(n))
	case n <= 0xFFFF:
		buf = append(buf, maskBit|126, byte(n>>8), byte(n))
	default:
		buf = append(buf, maskBit|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	if w.client {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		buf = append(buf, key[:]...)
		start := len(buf)
		buf = append(buf, payload...)
		for i := 0; i < n; i++ {
			buf[start+i] ^= key[i&3]
		}
	} else {
		buf = append(buf, payload...)
	}
	_, err := w.c.Write(buf)
	return err
}

func (w *Conn) Write(p []byte) (int, error) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closeSent.Load() {
		return 0, net.ErrClosed
	}
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxFrame {
			chunk = chunk[:maxFrame]
		}
		if err := w.writeFrame(opBinary, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

func (w *Conn) sendClose() {
	if w.closeSent.Swap(true) {
		return
	}
	if w.wmu.TryLock() {
		_ = w.c.SetWriteDeadline(time.Now().Add(time.Second))
		_ = w.writeFrame(opClose, []byte{0x03, 0xE8}) // 1000: normal closure
		w.wmu.Unlock()
	}
}

// Close sends a close frame (best effort) and closes the connection.
func (w *Conn) Close() error {
	var err error
	w.closeOnce.Do(func() {
		_ = w.c.SetWriteDeadline(time.Now().Add(time.Second))
		w.sendClose()
		err = w.c.Close()
	})
	return err
}

func (w *Conn) LocalAddr() net.Addr                { return w.c.LocalAddr() }
func (w *Conn) RemoteAddr() net.Addr               { return w.c.RemoteAddr() }
func (w *Conn) SetDeadline(t time.Time) error      { return w.c.SetDeadline(t) }
func (w *Conn) SetReadDeadline(t time.Time) error  { return w.c.SetReadDeadline(t) }
func (w *Conn) SetWriteDeadline(t time.Time) error { return w.c.SetWriteDeadline(t) }
