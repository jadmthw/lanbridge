// Package invite encodes everything a friend needs to reach a host into one
// copy-pasteable code: a random secret, the host's direct addresses and an
// optional relay.
package invite

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"
)

// Prefix starts every invite code.
const Prefix = "LB1-"

const (
	version = 1
	tRelay  = 1
	tAddr4  = 2
	tAddr6  = 3
	tName   = 4
)

// Invite is a decoded invite code.
type Invite struct {
	Secret [16]byte
	Relay  string
	Direct []netip.AddrPort
	Name   string
}

// ErrInvalid means the text isn't an invite code.
var ErrInvalid = errors.New("that doesn't look like a LANBridge invite code (they start with " + Prefix + ")")

// ErrDamaged means the code was cut off or mistyped.
var ErrDamaged = errors.New("that invite code is incomplete or damaged; copy the whole thing again")

// NewSecret makes a fresh random secret.
func NewSecret() ([16]byte, error) {
	var s [16]byte
	_, err := rand.Read(s[:])
	return s, err
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// RoomID is the name the host registers under at a relay. It is derived from
// the secret, so the relay never sees the secret itself.
func (inv Invite) RoomID() string {
	h := sha256.Sum256(append([]byte("lanbridge/room/v1:"), inv.Secret[:]...))
	return strings.ToLower(b32.EncodeToString(h[:15]))
}

// Encode returns the invite as a code.
func (inv Invite) Encode() string {
	b := []byte{version}
	b = append(b, inv.Secret[:]...)
	if inv.Relay != "" && len(inv.Relay) <= 255 {
		b = append(b, tRelay, byte(len(inv.Relay)))
		b = append(b, inv.Relay...)
	}
	for _, ap := range inv.Direct {
		ip := ap.Addr().Unmap()
		if ip.Is4() {
			a := ip.As4()
			b = append(b, tAddr4, 6)
			b = append(b, a[:]...)
		} else {
			a := ip.As16()
			b = append(b, tAddr6, 18)
			b = append(b, a[:]...)
		}
		b = binary.BigEndian.AppendUint16(b, ap.Port())
	}
	if name := truncate(inv.Name, 40); name != "" {
		b = append(b, tName, byte(len(name)))
		b = append(b, name...)
	}
	sum := sha256.Sum256(b)
	b = append(b, sum[:3]...)
	return Prefix + base64.RawURLEncoding.EncodeToString(b)
}

// LooksLikeCode reports whether s is plausibly an invite code.
func LooksLikeCode(s string) bool {
	s = clean(s)
	return len(s) > len(Prefix) && strings.EqualFold(s[:len(Prefix)], Prefix)
}

// Decode parses an invite code. It tolerates surrounding spaces, quotes and
// line breaks picked up while copying through chat apps.
func Decode(s string) (Invite, error) {
	s = clean(s)
	if !LooksLikeCode(s) {
		return Invite{}, ErrInvalid
	}
	body := strings.NewReplacer("+", "-", "/", "_", "=", "").Replace(s[len(Prefix):])
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(b) < 1+16+3 {
		return Invite{}, ErrDamaged
	}
	payload, sum := b[:len(b)-3], b[len(b)-3:]
	h := sha256.Sum256(payload)
	if !bytes.Equal(h[:3], sum) {
		return Invite{}, ErrDamaged
	}
	if payload[0] != version {
		return Invite{}, fmt.Errorf("this invite needs a newer version of LANBridge (code version %d)", payload[0])
	}
	var inv Invite
	copy(inv.Secret[:], payload[1:17])
	p := payload[17:]
	for len(p) > 0 {
		if len(p) < 2 || len(p) < 2+int(p[1]) {
			return Invite{}, ErrDamaged
		}
		t, v := p[0], p[2:2+int(p[1])]
		p = p[2+int(p[1]):]
		switch {
		case t == tRelay:
			inv.Relay = string(v)
		case t == tAddr4 && len(v) == 6:
			ip := netip.AddrFrom4([4]byte(v[:4]))
			inv.Direct = append(inv.Direct, netip.AddrPortFrom(ip, binary.BigEndian.Uint16(v[4:])))
		case t == tAddr6 && len(v) == 18:
			ip := netip.AddrFrom16([16]byte(v[:16]))
			inv.Direct = append(inv.Direct, netip.AddrPortFrom(ip, binary.BigEndian.Uint16(v[16:])))
		case t == tName:
			inv.Name = string(v)
		}
		// unknown field types are skipped so newer codes still work
	}
	return inv, nil
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '`', '<', '>', '\u200b', '\ufeff':
			return -1
		}
		return r
	}, s)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
