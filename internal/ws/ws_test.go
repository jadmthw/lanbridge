package ws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestEcho(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					c.Close()
					return
				}
				wc, err := Accept(c, br, req)
				if err != nil {
					return
				}
				defer wc.Close()
				_, _ = io.Copy(wc, wc)
			}()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, "tcp", "ws://"+ln.Addr().String()+"/relay")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := make([]byte, 300_000) // several frames, including 64-bit lengths
	_, _ = rand.Read(payload)
	go func() { _, _ = c.Write(payload) }()
	got := make([]byte, len(payload))
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("echo mismatch")
	}
}

func TestPingIsAnswered(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	srv := newConn(a, bufio.NewReader(a), false)
	go func() {
		// A client frame: masked ping with payload "hi", then a masked 1-byte binary frame.
		key := []byte{1, 2, 3, 4}
		ping := []byte{0x89, 0x82, key[0], key[1], key[2], key[3], 'h' ^ 1, 'i' ^ 2}
		data := []byte{0x82, 0x81, key[0], key[1], key[2], key[3], 'x' ^ 1}
		_, _ = b.Write(append(ping, data...))
	}()
	pong := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4)
		_, _ = io.ReadFull(b, buf)
		pong <- buf
	}()
	p := make([]byte, 1)
	if _, err := io.ReadFull(srv, p); err != nil || p[0] != 'x' {
		t.Fatalf("read %q, %v", p, err)
	}
	got := <-pong
	if !bytes.Equal(got, []byte{0x8A, 0x02, 'h', 'i'}) {
		t.Fatalf("pong = %x", got)
	}
}
