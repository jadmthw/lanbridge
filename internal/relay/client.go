// Package relay implements LANBridge's relay. A host keeps a control
// connection open; when a friend asks to join, the relay tells the host, the
// host opens a data connection, and the relay splices the two together.
// Everything that passes through is end-to-end encrypted between the friend
// and the host, so the relay can't read or alter game traffic.
package relay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"lanbridge/internal/netx"
	"lanbridge/internal/ws"
)

const (
	// Proto starts every command line.
	Proto = "LBR1"
	// DefaultPort is used for tcp:// relay addresses without a port.
	DefaultPort = "7777"
)

// Error is a refusal sent by the relay.
type Error struct{ Msg string }

func (e *Error) Error() string { return "relay: " + e.Msg }

// NormalizeURL turns user input such as "relay.example.com", "1.2.3.4:7777",
// "tcp://…", "ws://…", "wss://…" or "https://…" into a canonical relay URL.
func NormalizeURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "tcp://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("%q isn't a valid relay address", s)
	}
	switch strings.ToLower(u.Scheme) {
	case "tcp":
		port := u.Port()
		if port == "" {
			port = DefaultPort
		}
		return "tcp://" + net.JoinHostPort(u.Hostname(), port), nil
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("a relay address starts with tcp://, ws:// or wss:// (not %s://)", u.Scheme)
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String(), nil
}

// Host returns just the host name of a relay URL, for display.
func Host(relayURL string) string {
	if u, err := url.Parse(relayURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return relayURL
}

// Dial connects to a relay. network is "tcp", "tcp4" or "tcp6".
func Dial(ctx context.Context, network, relayURL string) (net.Conn, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "tcp":
		d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 20 * time.Second}
		return d.DialContext(ctx, network, u.Host)
	case "ws", "wss":
		return ws.Dial(ctx, network, relayURL)
	}
	return nil, fmt.Errorf("unsupported relay address %q", relayURL)
}

// Request sends one command line and waits for the reply. On "OK" it returns
// the connection, ready for use, and the rest of the reply line.
func Request(ctx context.Context, network, relayURL, line string, timeout time.Duration) (*netx.BufferedConn, string, error) {
	c, err := Dial(ctx, network, relayURL)
	if err != nil {
		return nil, "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	_ = c.SetDeadline(time.Now().Add(timeout))
	br := bufio.NewReaderSize(c, 1024)
	var resp string
	_, err = io.WriteString(c, line+"\n")
	if err == nil {
		resp, err = netx.ReadLine(br, 512)
	}
	if !stop() && err == nil {
		err = context.Cause(ctx)
	}
	if err != nil {
		c.Close()
		return nil, "", err
	}
	switch {
	case resp == "OK" || strings.HasPrefix(resp, "OK "):
	case strings.HasPrefix(resp, "ERR"):
		c.Close()
		return nil, "", &Error{Msg: strings.TrimSpace(strings.TrimPrefix(resp, "ERR"))}
	default:
		c.Close()
		return nil, "", fmt.Errorf("relay: unexpected reply %q", resp)
	}
	_ = c.SetDeadline(time.Time{})
	return &netx.BufferedConn{Conn: c, R: br}, strings.TrimSpace(strings.TrimPrefix(resp, "OK")), nil
}
