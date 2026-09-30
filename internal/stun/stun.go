// Package stun finds this network's public IPv4 address with a STUN binding
// request (RFC 5389). LANBridge uses it when the host forwarded a port by hand.
package stun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// DefaultServers are public STUN servers.
var DefaultServers = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478", "stun1.l.google.com:19302"}

const magicCookie = 0x2112A442

// PublicIPv4 asks STUN servers, in order, for this network's public IPv4 address.
func PublicIPv4(ctx context.Context, servers []string) (netip.Addr, error) {
	if len(servers) == 0 {
		servers = DefaultServers
	}
	err := errors.New("no STUN servers configured")
	for _, s := range servers {
		var ip netip.Addr
		if ip, err = query(ctx, s); err == nil {
			return ip, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return netip.Addr{}, err
}

func query(ctx context.Context, server string) (netip.Addr, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp4", server)
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	var txid [12]byte
	if _, err := rand.Read(txid[:]); err != nil {
		return netip.Addr{}, err
	}
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001) // binding request
	binary.BigEndian.PutUint32(req[4:], magicCookie)
	copy(req[8:], txid[:])
	deadline := time.Now().Add(2 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = c.SetDeadline(deadline)
	if _, err := c.Write(req); err != nil {
		return netip.Addr{}, err
	}
	buf := make([]byte, 1024)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("no answer from %s", server)
		}
		if ip, ok := ParseBindingResponse(buf[:n], txid); ok {
			return ip, nil
		}
	}
}

// ParseBindingResponse extracts the mapped IPv4 address from a binding success response.
func ParseBindingResponse(b []byte, txid [12]byte) (netip.Addr, bool) {
	if len(b) < 20 || binary.BigEndian.Uint16(b[0:]) != 0x0101 || binary.BigEndian.Uint32(b[4:]) != magicCookie || !bytes.Equal(b[8:20], txid[:]) {
		return netip.Addr{}, false
	}
	length := int(binary.BigEndian.Uint16(b[2:]))
	if 20+length > len(b) {
		return netip.Addr{}, false
	}
	attrs := b[20 : 20+length]
	var mapped netip.Addr
	for len(attrs) >= 4 {
		t := binary.BigEndian.Uint16(attrs[0:])
		l := int(binary.BigEndian.Uint16(attrs[2:]))
		if 4+l > len(attrs) {
			break
		}
		v := attrs[4 : 4+l]
		switch {
		case t == 0x0020 && l >= 8 && v[1] == 0x01: // XOR-MAPPED-ADDRESS, IPv4
			var ip [4]byte
			binary.BigEndian.PutUint32(ip[:], binary.BigEndian.Uint32(v[4:8])^magicCookie)
			return netip.AddrFrom4(ip), true
		case t == 0x0001 && l >= 8 && v[1] == 0x01: // MAPPED-ADDRESS, IPv4
			mapped = netip.AddrFrom4([4]byte(v[4:8]))
		}
		next := 4 + l + (4-l%4)%4
		if next > len(attrs) {
			break
		}
		attrs = attrs[next:]
	}
	return mapped, mapped.IsValid()
}
