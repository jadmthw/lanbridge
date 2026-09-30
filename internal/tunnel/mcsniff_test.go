package tunnel

import "testing"

func varint(v int) []byte {
	var out []byte
	u := uint32(v)
	for {
		b := byte(u & 0x7F)
		u >>= 7
		if u != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

func packet(body ...[]byte) []byte {
	var b []byte
	for _, x := range body {
		b = append(b, x...)
	}
	return append(varint(len(b)), b...)
}

func mcString(s string) []byte { return append(varint(len(s)), s...) }

func handshake(intent int) []byte {
	return packet(varint(0), varint(775), mcString("localhost"), []byte{0x63, 0xDD}, varint(intent))
}

func TestSniffLogin(t *testing.T) {
	stream := append(handshake(2), packet(varint(0), mcString("Steve_42"), make([]byte, 16))...)
	var gotIntent int
	var gotName string
	s := &mcSniffer{cb: func(i int, n string) { gotIntent, gotName = i, n }}
	// Feed one byte at a time, like a slow network.
	done := false
	for i := range stream {
		if s.feed(stream[i : i+1]) {
			done = true
			break
		}
	}
	if !done || gotIntent != intentLogin || gotName != "Steve_42" {
		t.Fatalf("done=%v intent=%d name=%q", done, gotIntent, gotName)
	}
}

func TestSniffStatusAndGarbage(t *testing.T) {
	var intent int
	s := &mcSniffer{cb: func(i int, _ string) { intent = i }}
	if !s.feed(handshake(1)) || intent != intentStatus {
		t.Fatalf("status ping: intent %d", intent)
	}
	g := &mcSniffer{cb: func(i int, _ string) { intent = i }}
	if !g.feed([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) || intent != intentUnknown {
		t.Fatal("garbage should end sniffing with an unknown intent")
	}
	if cleanName("bad name!") != "" || cleanName("Alex") != "Alex" {
		t.Fatal("cleanName")
	}
}
