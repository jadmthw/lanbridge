package stun

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestParseBindingResponse(t *testing.T) {
	var txid [12]byte
	copy(txid[:], "abcdefghijkl")
	ip := netip.MustParseAddr("198.51.100.23").As4()
	attr := make([]byte, 12)
	binary.BigEndian.PutUint16(attr[0:], 0x0020)
	binary.BigEndian.PutUint16(attr[2:], 8)
	attr[5] = 0x01
	binary.BigEndian.PutUint16(attr[6:], 4242^uint16(magicCookie>>16))
	binary.BigEndian.PutUint32(attr[8:], binary.BigEndian.Uint32(ip[:])^magicCookie)
	msg := make([]byte, 20)
	binary.BigEndian.PutUint16(msg[0:], 0x0101)
	binary.BigEndian.PutUint16(msg[2:], uint16(len(attr)))
	binary.BigEndian.PutUint32(msg[4:], magicCookie)
	copy(msg[8:], txid[:])
	msg = append(msg, attr...)
	got, ok := ParseBindingResponse(msg, txid)
	if !ok || got != netip.MustParseAddr("198.51.100.23") {
		t.Fatalf("got %v %v", got, ok)
	}
	var other [12]byte
	if _, ok := ParseBindingResponse(msg, other); ok {
		t.Fatal("accepted a response for another transaction")
	}
}
