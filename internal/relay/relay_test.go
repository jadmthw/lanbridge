package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startRelay(t *testing.T, token string) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil)
	s.Token = token
	go s.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"relay.example.com":               "tcp://relay.example.com:7777",
		"1.2.3.4:9000":                    "tcp://1.2.3.4:9000",
		"https://relay.example.com/x?y=1": "wss://relay.example.com/x",
		"ws://10.0.0.1:8080":              "ws://10.0.0.1:8080",
		"  tcp://[2001:db8::1]  ":         "tcp://[2001:db8::1]:7777",
		"":                                "",
	}
	for in, want := range cases {
		got, err := NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeURL("ftp://x"); err == nil {
		t.Error("ftp should be rejected")
	}
}

func TestJoinWithoutHost(t *testing.T) {
	addr := startRelay(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := Request(ctx, "tcp", "tcp://"+addr, Proto+" JOIN abcdefghij", 5*time.Second)
	var re *Error
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "isn't connected") {
		t.Fatalf("got %v", err)
	}
}

func TestTokenRequired(t *testing.T) {
	addr := startRelay(t, "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := Request(ctx, "tcp", "tcp://"+addr, Proto+" HOST abcdefghij wrong", 5*time.Second); err == nil {
		t.Fatal("wrong token accepted")
	}
	c, rest, err := Request(ctx, "tcp", "tcp://"+addr, Proto+" HOST abcdefghij s3cret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.HasPrefix(rest, "127.0.0.1") {
		t.Fatalf("reply %q", rest)
	}
}

func TestHealthz(t *testing.T) {
	addr := startRelay(t, "")
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "ok\n" {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
}
