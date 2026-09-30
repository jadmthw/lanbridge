package invite

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func sample(t *testing.T) Invite {
	t.Helper()
	sec, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	return Invite{
		Secret: sec,
		Relay:  "wss://relay.example.com/",
		Name:   "Max",
		Direct: []netip.AddrPort{netip.MustParseAddrPort("203.0.113.9:42525"), netip.MustParseAddrPort("[2001:db8::7]:42525"), netip.MustParseAddrPort("192.168.1.20:42525")},
	}
}

func TestRoundTrip(t *testing.T) {
	inv := sample(t)
	code := inv.Encode()
	if !strings.HasPrefix(code, Prefix) {
		t.Fatalf("code %q lacks prefix", code)
	}
	got, err := Decode(code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != inv.Secret || got.Relay != inv.Relay || got.Name != inv.Name || len(got.Direct) != 3 {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, inv)
	}
	for i := range inv.Direct {
		if got.Direct[i] != inv.Direct[i] {
			t.Fatalf("addr %d: %v != %v", i, got.Direct[i], inv.Direct[i])
		}
	}
	if got.RoomID() != inv.RoomID() || len(inv.RoomID()) != 24 {
		t.Fatalf("room id %q", inv.RoomID())
	}
}

func TestTolerantDecode(t *testing.T) {
	code := sample(t).Encode()
	mid := len(code) / 2
	messy := "  \"" + code[:mid] + "\n " + code[mid:] + "\"\r\n"
	if _, err := Decode(messy); err != nil {
		t.Fatalf("messy paste: %v", err)
	}
	if _, err := Decode(strings.ToLower(code[:4]) + code[4:]); err != nil {
		t.Fatalf("lowercase prefix: %v", err)
	}
}

func TestDamaged(t *testing.T) {
	code := sample(t).Encode()
	if _, err := Decode(code[:len(code)-3]); !errors.Is(err, ErrDamaged) {
		t.Fatalf("truncated: got %v", err)
	}
	b := []byte(code)
	if b[10] == 'A' {
		b[10] = 'B'
	} else {
		b[10] = 'A'
	}
	if _, err := Decode(string(b)); !errors.Is(err, ErrDamaged) {
		t.Fatalf("typo: got %v", err)
	}
	if _, err := Decode("hello"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("not a code: got %v", err)
	}
}

func TestNameIsTruncatedSafely(t *testing.T) {
	inv := sample(t)
	inv.Name = strings.Repeat("é", 30) // 60 bytes
	got, err := Decode(inv.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Name) > 40 || !strings.HasPrefix(inv.Name, got.Name) {
		t.Fatalf("name %q", got.Name)
	}
}
