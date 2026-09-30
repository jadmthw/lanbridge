package tunnel

import "strings"

// Minecraft's first packets are unencrypted: a Handshake whose "intent" is
// 1 (server-list ping), 2 (login) or 3 (transfer), then for logins a Login
// Start that carries the player's name. The host peeks at them only to show
// who is playing. The bytes are always forwarded unchanged.

const (
	intentUnknown  = 0
	intentStatus   = 1
	intentLogin    = 2
	intentTransfer = 3
)

type mcSniffer struct {
	buf  []byte
	done bool
	cb   func(intent int, name string)
}

// feed takes the next client bytes and reports whether it has seen enough.
func (s *mcSniffer) feed(p []byte) bool {
	if s.done {
		return true
	}
	s.buf = append(s.buf, p...)
	intent, name, complete, bad := parseMCStart(s.buf)
	switch {
	case bad || (!complete && len(s.buf) > 4096):
		s.finish(intentUnknown, "")
	case complete:
		s.finish(intent, name)
	}
	return s.done
}

func (s *mcSniffer) finish(intent int, name string) {
	s.done = true
	s.buf = nil
	if s.cb != nil {
		s.cb(intent, cleanName(name))
	}
}

func cleanName(n string) string {
	if len(n) == 0 || len(n) > 16 {
		return ""
	}
	for _, r := range n {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return strings.Clone(n)
}

type mcReader struct {
	b   []byte
	bad bool
}

// varint returns ok=false with r.bad unset when more data is needed.
func (r *mcReader) varint() (int, bool) {
	var v uint32
	for i := 0; i < 5; i++ {
		if len(r.b) == 0 {
			return 0, false
		}
		c := r.b[0]
		r.b = r.b[1:]
		v |= uint32(c&0x7F) << (7 * i)
		if c&0x80 == 0 {
			return int(int32(v)), true
		}
	}
	r.bad = true
	return 0, false
}

func (r *mcReader) take(n int) ([]byte, bool) {
	if n < 0 {
		r.bad = true
		return nil, false
	}
	if len(r.b) < n {
		return nil, false
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out, true
}

func (r *mcReader) str(max int) (string, bool) {
	n, ok := r.varint()
	if !ok {
		return "", false
	}
	if n < 0 || n > max {
		r.bad = true
		return "", false
	}
	b, ok := r.take(n)
	return string(b), ok
}

func (r *mcReader) packet() (*mcReader, bool) {
	n, ok := r.varint()
	if !ok {
		return nil, false
	}
	if n <= 0 || n > 2048 {
		r.bad = true
		return nil, false
	}
	b, ok := r.take(n)
	if !ok {
		return nil, false
	}
	return &mcReader{b: b}, true
}

// parseMCStart parses the Handshake and, for logins, the Login Start packet.
func parseMCStart(buf []byte) (intent int, name string, complete, bad bool) {
	r := &mcReader{b: buf}
	hs, ok := r.packet()
	if !ok {
		return 0, "", false, r.bad
	}
	if id, ok := hs.varint(); !ok || id != 0 {
		return 0, "", false, true
	}
	if _, ok := hs.varint(); !ok { // protocol version
		return 0, "", false, true
	}
	if _, ok := hs.str(1024); !ok { // server address
		return 0, "", false, true
	}
	if _, ok := hs.take(2); !ok { // port
		return 0, "", false, true
	}
	next, ok := hs.varint()
	if !ok {
		return 0, "", false, true
	}
	if next != intentLogin && next != intentTransfer {
		return next, "", true, false
	}
	ls, ok := r.packet()
	if !ok {
		return next, "", r.bad, false
	}
	if id, ok := ls.varint(); !ok || id != 0 {
		return next, "", true, false
	}
	nm, _ := ls.str(64)
	return next, nm, true, false
}
