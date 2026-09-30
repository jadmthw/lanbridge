// Package netx holds small networking helpers shared across LANBridge.
package netx

import (
	"bufio"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BufferedConn is a net.Conn whose reads go through R, so bytes that were
// buffered while reading a header line aren't lost.
type BufferedConn struct {
	net.Conn
	R *bufio.Reader
}

func (c *BufferedConn) Read(p []byte) (int, error) { return c.R.Read(p) }

// CloseWrite half-closes the connection when the underlying conn supports it.
func (c *BufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// ErrLineTooLong is returned by ReadLine for oversized lines.
var ErrLineTooLong = errors.New("line too long")

// ReadLine reads one '\n'-terminated line of at most max bytes, without the line ending.
func ReadLine(r *bufio.Reader, max int) (string, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max {
			return "", ErrLineTooLong
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// PipeOptions configures Pipe.
type PipeOptions struct {
	AToB, BToA *atomic.Int64     // byte counters, optional
	Sniff      func([]byte) bool // sees a→b data until it returns true, optional
}

// Pipe copies data both ways between a and b. When one direction ends it
// half-closes the other side and gives the remaining direction a few seconds
// to finish, then closes both connections.
func Pipe(a, b net.Conn, opt PipeOptions) {
	done := make(chan struct{}, 2)
	go copyHalf(b, a, opt.AToB, opt.Sniff, done)
	go copyHalf(a, b, opt.BToA, nil, done)
	<-done
	grace := time.Now().Add(5 * time.Second)
	_ = a.SetDeadline(grace)
	_ = b.SetDeadline(grace)
	<-done
	a.Close()
	b.Close()
}

func copyHalf(dst, src net.Conn, ctr *atomic.Int64, sniff func([]byte) bool, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if sniff != nil && sniff(buf[:n]) {
				sniff = nil
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			if ctr != nil {
				ctr.Add(int64(n))
			}
		}
		if err != nil {
			break
		}
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// ConnSet tracks live connections so they can all be closed at shutdown.
type ConnSet struct {
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Add registers c. It returns false (and closes c) if the set is already closed.
func (s *ConnSet) Add(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return false
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[c] = struct{}{}
	return true
}

// Remove forgets c.
func (s *ConnSet) Remove(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// CloseAll closes every tracked connection and refuses new ones.
func (s *ConnSet) CloseAll() {
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

// IsPublic reports whether ip is a globally routable unicast address.
func IsPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if ip.Is4() {
		b := ip.As4()
		switch {
		case b[0] == 100 && b[1]&0xC0 == 64: // 100.64.0.0/10, carrier-grade NAT
			return false
		case b[0] == 0 || b[0] >= 240:
			return false
		case b[0] == 192 && b[1] == 0 && (b[2] == 0 || b[2] == 2):
			return false
		case b[0] == 198 && (b[1] == 18 || b[1] == 19):
			return false
		case b[0] == 198 && b[1] == 51 && b[2] == 100, b[0] == 203 && b[1] == 0 && b[2] == 113:
			return false
		}
		return true
	}
	b := ip.As16()
	if b[0]&0xE0 != 0x20 { // global unicast is 2000::/3
		return false
	}
	return !(b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x0d && b[3] == 0xb8) // 2001:db8::/32
}

// Describe says, in plain words, what kind of route an address is.
func Describe(ip netip.Addr) string {
	ip = ip.Unmap()
	switch {
	case ip.IsLoopback():
		return "this computer"
	case ip.Is4() && (ip.IsPrivate() || ip.IsLinkLocalUnicast()):
		return "same network"
	case ip.Is6():
		return "IPv6"
	default:
		return "internet"
	}
}

// LocalAddrs lists this computer's usable addresses: private IPv4 (LAN), public
// IPv4 assigned straight to an interface, and global IPv6. The address used for
// the default route comes first in each list.
func LocalAddrs() (lan4, pub4, v6 []netip.Addr) {
	prim4 := primary("udp4", "8.8.8.8:53")
	prim6 := primary("udp6", "[2001:4860:4860::8888]:53")
	seen := map[netip.Addr]bool{}
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || virtualIface(ifi.Name) {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if seen[ip] {
				continue
			}
			seen[ip] = true
			switch {
			case ip.Is4() && ip.IsPrivate():
				lan4 = append(lan4, ip)
			case ip.Is4() && IsPublic(ip):
				pub4 = append(pub4, ip)
			case ip.Is6() && IsPublic(ip):
				v6 = append(v6, ip)
			}
		}
	}
	return putFirst(lan4, prim4), putFirst(pub4, prim4), putFirst(v6, prim6)
}

func primary(network, probe string) netip.Addr {
	c, err := net.Dial(network, probe) // a UDP "dial" sends nothing; it just picks a route
	if err != nil {
		return netip.Addr{}
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		ip, _ := netip.AddrFromSlice(ua.IP)
		return ip.Unmap()
	}
	return netip.Addr{}
}

func putFirst(list []netip.Addr, first netip.Addr) []netip.Addr {
	for i, ip := range list {
		if ip == first && i > 0 {
			list[0], list[i] = list[i], list[0]
			break
		}
	}
	return list
}

func virtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "vethernet", "zt", "tailscale", "utun", "awdl", "llw", "anpi", "bridge", "gif", "stf", "tun", "tap", "wg"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	for _, s := range []string{"virtualbox", "vmware", "hyper-v", "loopback", "wsl", "vpn"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// RemoteIP returns the IP part of a network address.
func RemoteIP(a net.Addr) netip.Addr {
	if a == nil {
		return netip.Addr{}
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return netip.Addr{}
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}
