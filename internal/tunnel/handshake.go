// Package tunnel carries Minecraft connections between a host and friends.
//
// Every connection is TLS 1.3. The host's certificate is throwaway; instead,
// both sides prove they hold the invite secret with an HMAC over the TLS
// session's exported keying material (RFC 8446 §7.5). A man-in-the-middle
// (including the relay) ends up with two different TLS sessions and can't
// produce either proof, so traffic is private and tamper-proof end to end.
package tunnel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"
)

// Stream types a joiner can ask for.
const (
	StreamInfo byte = 1 // host status
	StreamGame byte = 2 // a Minecraft connection
)

// Status bytes the host answers with.
const (
	StatusOK       byte = 0
	StatusOffline  byte = 1 // no world open right now
	StatusError    byte = 2
	statusRejected byte = 0xFF
)

const (
	alpn     = "lanbridge/1"
	ekmLabel = "EXPORTER-lanbridge-auth-v1"
)

var magic = []byte("LBT1")

var (
	// ErrRejected means the host didn't accept the invite secret.
	ErrRejected = errors.New("the host didn't accept this invite code; it may be old, so ask for a new one")
	// ErrNotHost means whatever answered couldn't prove it's the host.
	ErrNotHost = errors.New("something answered, but it isn't the host from this invite")
)

func newServerTLS() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "lanbridge"},
		DNSNames:     []string{"lanbridge"},
		NotBefore:    time.Now().Add(-24 * time.Hour),
		NotAfter:     time.Now().Add(20 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{alpn},
	}, nil
}

func clientTLS() *tls.Config {
	// Certificate checks are replaced by the secret-based proof below.
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, NextProtos: []string{alpn}, ServerName: "lanbridge"}
}

func authTag(secret []byte, role string, stream byte, ekm []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("lanbridge-v1/" + role + "/"))
	m.Write([]byte{stream})
	m.Write(ekm)
	return m.Sum(nil)
}

// clientHandshake secures raw as the joining side and authenticates both
// ways. It returns the host's status byte and the round-trip time of the
// authentication exchange.
func clientHandshake(ctx context.Context, raw net.Conn, secret []byte, stream byte) (*tls.Conn, byte, time.Duration, error) {
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	_ = raw.SetDeadline(time.Now().Add(12 * time.Second))
	tc := tls.Client(raw, clientTLS())
	if err := tc.Handshake(); err != nil {
		return nil, 0, 0, fmt.Errorf("secure handshake failed: %w", err)
	}
	cs := tc.ConnectionState()
	ekm, err := cs.ExportKeyingMaterial(ekmLabel, nil, 32)
	if err != nil {
		return nil, 0, 0, err
	}
	msg := append(append(append([]byte{}, magic...), stream), authTag(secret, "client", stream, ekm)...)
	start := time.Now()
	if _, err := tc.Write(msg); err != nil {
		return nil, 0, 0, err
	}
	resp := make([]byte, 33)
	if _, err := io.ReadFull(tc, resp); err != nil {
		return nil, 0, 0, fmt.Errorf("host closed the connection during setup: %w", err)
	}
	rtt := time.Since(start)
	if resp[32] == statusRejected && bytes.Equal(resp[:32], make([]byte, 32)) {
		return nil, 0, 0, ErrRejected
	}
	if !hmac.Equal(resp[:32], authTag(secret, "host", stream, ekm)) {
		return nil, 0, 0, ErrNotHost
	}
	if !stop() {
		return nil, 0, 0, context.Cause(ctx)
	}
	_ = raw.SetDeadline(time.Time{})
	return tc, resp[32], rtt, nil
}

// serverHandshake secures raw as the host and checks the joiner's proof. On
// success the caller must answer with respond(status) before using the conn.
func serverHandshake(raw net.Conn, conf *tls.Config, secret []byte) (*tls.Conn, byte, func(byte) error, error) {
	_ = raw.SetDeadline(time.Now().Add(12 * time.Second))
	tc := tls.Server(raw, conf)
	if err := tc.Handshake(); err != nil {
		return nil, 0, nil, err
	}
	cs := tc.ConnectionState()
	ekm, err := cs.ExportKeyingMaterial(ekmLabel, nil, 32)
	if err != nil {
		return nil, 0, nil, err
	}
	req := make([]byte, len(magic)+1+32)
	if _, err := io.ReadFull(tc, req); err != nil {
		return nil, 0, nil, err
	}
	if !bytes.Equal(req[:len(magic)], magic) {
		return nil, 0, nil, errors.New("not a LANBridge client")
	}
	stream := req[len(magic)]
	if !hmac.Equal(req[len(magic)+1:], authTag(secret, "client", stream, ekm)) {
		_, _ = tc.Write(append(make([]byte, 32), statusRejected))
		return nil, 0, nil, errors.New("wrong invite code")
	}
	respond := func(status byte) error {
		_, err := tc.Write(append(authTag(secret, "host", stream, ekm), status))
		_ = raw.SetDeadline(time.Time{})
		return err
	}
	return tc, stream, respond, nil
}

// HostInfo is what a host reports about itself.
type HostInfo struct {
	Name    string `json:"name"`
	MOTD    string `json:"motd"`
	Online  bool   `json:"online"`
	Players int    `json:"players"`
	Version string `json:"version"`
}

func writeMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > 60000 {
		return errors.New("message too large")
	}
	_, err = w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...))
	return err
}

func readMsg(r io.Reader, v any) error {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	b := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const digits = "0123456789abcdef"
	out := make([]byte, 2*n)
	for i, x := range b {
		out[2*i], out[2*i+1] = digits[x>>4], digits[x&15]
	}
	return string(out)
}
