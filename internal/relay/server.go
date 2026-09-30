package relay

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lanbridge/internal/logx"
	"lanbridge/internal/netx"
	"lanbridge/internal/ws"
)

const (
	maxRooms   = 20000
	maxPending = 32
	joinWait   = 15 * time.Second
)

// Server is a relay. Configure the exported fields before calling Serve.
type Server struct {
	Token      string // hosts must present this, if set
	TrustProxy bool   // take the client IP from X-Forwarded-For (behind a reverse proxy)
	Version    string
	Log        *logx.Logger

	mu      sync.Mutex
	rooms   map[string]*room
	probes  map[string]time.Time
	active  atomic.Int64
	total   atomic.Int64
	bytes   atomic.Int64
	started time.Time
}

type room struct {
	ctrl    *ctrl
	pending map[string]chan net.Conn
}

type ctrl struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *ctrl) send(line string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := io.WriteString(c.conn, line+"\n")
	return err
}

// NewServer returns a relay server.
func NewServer(log *logx.Logger) *Server {
	if log == nil {
		log = logx.Discard()
	}
	return &Server{Log: log, rooms: map[string]*room{}, probes: map[string]time.Time{}, started: time.Now(), Version: "dev"}
}

// Serve accepts connections until ln is closed.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReaderSize(c, 2048)
	head, err := br.Peek(4)
	if err != nil {
		c.Close()
		return
	}
	var conn net.Conn = &netx.BufferedConn{Conn: c, R: br}
	ip := netx.RemoteIP(c.RemoteAddr()).String()
	if string(head) == "GET " || string(head) == "HEAD" {
		req, err := http.ReadRequest(br)
		if err != nil {
			c.Close()
			return
		}
		if s.TrustProxy {
			if fwd := forwardedFor(req); fwd != "" {
				ip = fwd
			}
		}
		if !ws.IsUpgrade(req) {
			s.serveHTTP(c, req)
			return
		}
		wc, err := ws.Accept(c, br, req)
		if err != nil {
			c.Close()
			return
		}
		conn = wc
	}
	lr := bufio.NewReaderSize(conn, 1024)
	line, err := netx.ReadLine(lr, 512)
	if err != nil {
		conn.Close()
		return
	}
	conn = &netx.BufferedConn{Conn: conn, R: lr}
	f := strings.Fields(line)
	if len(f) < 3 || f[0] != Proto {
		s.reject(conn, "bad request (is this a LANBridge client?)")
		return
	}
	switch f[1] {
	case "HOST":
		s.host(conn, lr, f[2:], ip)
	case "JOIN":
		s.join(conn, f[2:])
	case "ACCEPT":
		s.accept(conn, f[2:])
	case "PROBE":
		s.probe(conn, f[2:], ip)
	default:
		s.reject(conn, "unknown command")
	}
}

func (s *Server) reject(conn net.Conn, msg string) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "ERR "+msg+"\n")
	conn.Close()
}

func (s *Server) host(conn net.Conn, lr *bufio.Reader, args []string, ip string) {
	id := args[0]
	if !validID(id) {
		s.reject(conn, "bad room id")
		return
	}
	if s.Token != "" {
		tok := ""
		if len(args) > 1 {
			tok = args[1]
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
			s.reject(conn, "this relay needs a valid access token")
			return
		}
	}
	c := &ctrl{conn: conn}
	s.mu.Lock()
	r := s.rooms[id]
	if r == nil {
		if len(s.rooms) >= maxRooms {
			s.mu.Unlock()
			s.reject(conn, "relay is full")
			return
		}
		r = &room{pending: map[string]chan net.Conn{}}
		s.rooms[id] = r
	}
	old := r.ctrl
	r.ctrl = c
	s.mu.Unlock()
	if old != nil {
		old.conn.Close() // the same host reconnected
	}
	if err := c.send("OK " + ip + " probe"); err != nil {
		s.dropCtrl(id, c)
		conn.Close()
		return
	}
	s.Log.Infof("host online (room %s)", id[:6])
	_ = conn.SetDeadline(time.Time{})
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if c.send("PING") != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		line, err := netx.ReadLine(lr, 128)
		if err != nil {
			break
		}
		if line == "PING" {
			_ = c.send("PONG")
		}
	}
	close(stop)
	conn.Close()
	s.dropCtrl(id, c)
	s.Log.Infof("host offline (room %s)", id[:6])
}

func (s *Server) dropCtrl(id string, c *ctrl) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rooms[id]; r != nil && r.ctrl == c {
		r.ctrl = nil
		if len(r.pending) == 0 {
			delete(s.rooms, id)
		}
	}
}

func (s *Server) join(conn net.Conn, args []string) {
	id := args[0]
	if !validID(id) {
		s.reject(conn, "bad room id")
		return
	}
	connID := randomHex(8)
	ch := make(chan net.Conn, 1)
	s.mu.Lock()
	r := s.rooms[id]
	if r == nil || r.ctrl == nil {
		s.mu.Unlock()
		s.reject(conn, "the host isn't connected to this relay right now")
		return
	}
	if len(r.pending) >= maxPending {
		s.mu.Unlock()
		s.reject(conn, "too many people joining at once; try again")
		return
	}
	r.pending[connID] = ch
	c := r.ctrl
	s.mu.Unlock()

	var hostConn net.Conn
	if c.send("CONN "+connID) == nil {
		t := time.NewTimer(joinWait)
		select {
		case hostConn = <-ch:
		case <-t.C:
		}
		t.Stop()
	}
	s.mu.Lock()
	_, still := r.pending[connID]
	delete(r.pending, connID)
	if r.ctrl == nil && len(r.pending) == 0 && s.rooms[id] == r {
		delete(s.rooms, id)
	}
	s.mu.Unlock()
	if !still && hostConn == nil {
		hostConn = <-ch // the host's ACCEPT won the race and is already in the channel
	}
	if hostConn == nil {
		s.reject(conn, "the host didn't answer")
		return
	}
	_ = conn.SetDeadline(time.Time{})
	_ = hostConn.SetDeadline(time.Time{})
	if _, err := io.WriteString(hostConn, "OK\n"); err != nil {
		hostConn.Close()
		conn.Close()
		return
	}
	if _, err := io.WriteString(conn, "OK\n"); err != nil {
		hostConn.Close()
		conn.Close()
		return
	}
	s.active.Add(1)
	s.total.Add(1)
	var up, down atomic.Int64
	netx.Pipe(conn, hostConn, netx.PipeOptions{AToB: &up, BToA: &down})
	s.active.Add(-1)
	s.bytes.Add(up.Load() + down.Load())
}

func (s *Server) accept(conn net.Conn, args []string) {
	if len(args) < 2 || !validID(args[0]) {
		s.reject(conn, "bad accept")
		return
	}
	s.mu.Lock()
	var ch chan net.Conn
	if r := s.rooms[args[0]]; r != nil {
		ch = r.pending[args[1]]
		delete(r.pending, args[1])
	}
	s.mu.Unlock()
	if ch == nil {
		s.reject(conn, "that join request expired")
		return
	}
	ch <- conn // the joining goroutine owns the connection now
}

// probe checks whether the caller's public address accepts connections on a
// port, so a host learns whether friends can reach it directly. It only ever
// dials the address the request came from.
func (s *Server) probe(conn net.Conn, args []string, ip string) {
	defer conn.Close()
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 || len(args) < 2 || !validID(args[1]) {
		s.reject(conn, "bad probe")
		return
	}
	nonce := args[1]
	s.mu.Lock()
	if last, ok := s.probes[ip]; ok && time.Since(last) < 3*time.Second {
		s.mu.Unlock()
		s.reject(conn, "slow down")
		return
	}
	s.probes[ip] = time.Now()
	if len(s.probes) > 5000 {
		for k, t := range s.probes {
			if time.Since(t) > time.Minute {
				delete(s.probes, k)
			}
		}
	}
	s.mu.Unlock()
	target := net.JoinHostPort(ip, strconv.Itoa(port))
	result := "unreachable"
	if pc, err := net.DialTimeout("tcp", target, 4*time.Second); err == nil {
		_ = pc.SetDeadline(time.Now().Add(4 * time.Second))
		if _, err := io.WriteString(pc, "LBPROBE "+nonce+"\n"); err == nil {
			if line, err := netx.ReadLine(bufio.NewReader(pc), 128); err == nil && line == "LBPROBE-OK "+nonce {
				result = "reachable"
			}
		}
		pc.Close()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "OK "+result+" "+target+"\n")
}

func (s *Server) serveHTTP(c net.Conn, req *http.Request) {
	defer c.Close()
	status, body := "200 OK", ""
	switch req.URL.Path {
	case "/healthz":
		body = "ok\n"
	case "/":
		s.mu.Lock()
		hosts := 0
		for _, r := range s.rooms {
			if r.ctrl != nil {
				hosts++
			}
		}
		s.mu.Unlock()
		body = fmt.Sprintf("LANBridge relay %s\nhosts online: %d\nactive tunnels: %d\ntunnels since start: %d\nuptime: %s\n",
			s.Version, hosts, s.active.Load(), s.total.Load(), time.Since(s.started).Round(time.Second))
	default:
		status, body = "404 Not Found", "not found\n"
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "HTTP/1.1 %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n", status, len(body))
	if req.Method != http.MethodHead {
		_, _ = io.WriteString(c, body)
	}
}

// forwardedFor returns the client address added by the nearest reverse proxy.
func forwardedFor(r *http.Request) string {
	for _, h := range []string{"Cf-Connecting-Ip", "X-Real-Ip"} {
		if ip, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(h))); err == nil {
			return ip.Unmap().String()
		}
	}
	if v := r.Header.Values("X-Forwarded-For"); len(v) > 0 {
		parts := strings.Split(v[len(v)-1], ",")
		if ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
			return ip.Unmap().String()
		}
	}
	return ""
}

func validID(s string) bool {
	if len(s) < 6 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
