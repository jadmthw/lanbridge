// Package lan speaks Minecraft: Java Edition's "Open to LAN" discovery protocol.
// Every 1.5 s the hosting game sends "[MOTD]<text>[/MOTD][AD]<port>[/AD]" to
// 224.0.2.60:4445, and clients list the sender's IP address plus that port.
package lan

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lanbridge/internal/logx"
)

const (
	GroupIP = "224.0.2.60"
	Port    = 4445
	// Minecraft re-announces every 1.5 s; after this long a world is considered closed.
	expireAfter = 7 * time.Second
)

// GroupAddr is where Minecraft announces LAN worlds.
var GroupAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 2, 60), Port: Port}

var tokens = strings.NewReplacer("[MOTD]", "", "[/MOTD]", "", "[AD]", "", "[/AD]", "")

// Sanitize makes a world name safe to put in an announcement.
func Sanitize(motd string) string {
	motd = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, motd)
	for {
		s := tokens.Replace(motd)
		if s == motd {
			break
		}
		motd = s
	}
	motd = strings.TrimSpace(motd)
	if utf8.RuneCountInString(motd) > 100 {
		motd = string([]rune(motd)[:100])
	}
	if motd == "" {
		motd = "Minecraft world"
	}
	return motd
}

// Format builds an announcement for a world listening on port.
func Format(motd string, port int) []byte {
	return []byte("[MOTD]" + Sanitize(motd) + "[/MOTD][AD]" + strconv.Itoa(port) + "[/AD]")
}

// Parse reads an announcement.
func Parse(b []byte) (motd string, port int, ok bool) {
	s := string(b)
	i := strings.Index(s, "[MOTD]")
	if i < 0 {
		return "", 0, false
	}
	rest := s[i+len("[MOTD]"):]
	j := strings.Index(rest, "[/MOTD]")
	if j < 0 {
		return "", 0, false
	}
	motd = rest[:j]
	rest = rest[j+len("[/MOTD]"):]
	k := strings.Index(rest, "[AD]")
	if k < 0 {
		return "", 0, false
	}
	rest = rest[k+len("[AD]"):]
	l := strings.Index(rest, "[/AD]")
	if l < 0 {
		return "", 0, false
	}
	ad := strings.TrimSpace(rest[:l])
	if c := strings.LastIndexByte(ad, ':'); c >= 0 {
		ad = ad[c+1:]
	}
	p, err := strconv.Atoi(ad)
	if err != nil || p <= 0 || p > 65535 {
		return "", 0, false
	}
	return motd, p, true
}

// World is a LAN world seen on the network.
type World struct {
	Key      string    `json:"key"`
	MOTD     string    `json:"motd"`
	Addr     string    `json:"addr"`   // where to connect
	Port     int       `json:"port"`   // the game's port
	Source   string    `json:"source"` // who announced it
	Local    bool      `json:"local"`  // hosted on this computer
	LastSeen time.Time `json:"lastSeen"`
}

// Detector listens for LAN world announcements.
type Detector struct {
	log       *logx.Logger
	conns     []*net.UDPConn
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	mu        sync.Mutex
	worlds    map[string]*World
	local     map[netip.Addr]bool
	localTime time.Time
}

// StartDetector starts listening on every multicast-capable interface.
func StartDetector(log *logx.Logger) (*Detector, error) {
	d := &Detector{log: log, closed: make(chan struct{}), worlds: map[string]*World{}}
	var lastErr error
	if c, err := net.ListenMulticastUDP("udp4", nil, GroupAddr); err == nil {
		d.conns = append(d.conns, c)
	} else {
		lastErr = err
	}
	ifaces, _ := net.Interfaces()
	for i := range ifaces {
		ifi := ifaces[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || !hasIPv4(&ifi) {
			continue
		}
		c, err := net.ListenMulticastUDP("udp4", &ifi, GroupAddr)
		if err != nil {
			lastErr = err
			continue
		}
		d.conns = append(d.conns, c)
	}
	if len(d.conns) == 0 {
		return nil, fmt.Errorf("can't listen for LAN worlds on UDP port %d (%v)", Port, lastErr)
	}
	for _, c := range d.conns {
		d.wg.Add(1)
		go d.readLoop(c)
	}
	return d, nil
}

func hasIPv4(ifi *net.Interface) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return true
		}
	}
	return false
}

func (d *Detector) readLoop(c *net.UDPConn) {
	defer d.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-d.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if motd, port, ok := Parse(buf[:n]); ok {
			d.saw(src, motd, port)
		}
	}
}

func (d *Detector) saw(src *net.UDPAddr, motd string, port int) {
	ip, ok := netip.AddrFromSlice(src.IP)
	if !ok {
		return
	}
	ip = ip.Unmap()
	d.mu.Lock()
	local := ip.IsLoopback() || d.isLocal(ip)
	dial := ip
	if local {
		dial = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	key := netip.AddrPortFrom(ip, uint16(port)).String()
	w, existed := d.worlds[key]
	if !existed {
		w = &World{Key: key, Port: port}
		d.worlds[key] = w
	}
	w.MOTD = motd
	w.Source = ip.String()
	w.Local = local
	w.Addr = netip.AddrPortFrom(dial, uint16(port)).String()
	w.LastSeen = time.Now()
	d.mu.Unlock()
	if !existed {
		where := "on this computer"
		if !local {
			where = "at " + ip.String()
		}
		d.log.Infof("found LAN world %q %s (port %d)", motd, where, port)
	}
}

// isLocal must be called with d.mu held.
func (d *Detector) isLocal(ip netip.Addr) bool {
	if d.local == nil || time.Since(d.localTime) > 30*time.Second {
		d.local = map[netip.Addr]bool{}
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					if x, ok := netip.AddrFromSlice(ipn.IP); ok {
						d.local[x.Unmap()] = true
					}
				}
			}
		}
		d.localTime = time.Now()
	}
	return d.local[ip]
}

// Worlds returns the worlds announced recently, ones on this computer first.
func (d *Detector) Worlds() []World {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	out := make([]World, 0, len(d.worlds))
	for k, w := range d.worlds {
		if now.Sub(w.LastSeen) > expireAfter {
			delete(d.worlds, k)
			d.log.Infof("LAN world %q closed", w.MOTD)
			continue
		}
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Local != out[j].Local {
			return out[i].Local
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Close stops listening.
func (d *Detector) Close() {
	d.closeOnce.Do(func() {
		close(d.closed)
		for _, c := range d.conns {
			c.Close()
		}
		d.wg.Wait()
	})
}

// Announcer advertises a world so it shows up in Minecraft's LAN list.
type Announcer struct {
	conn      *net.UDPConn
	port      int
	targets   []*net.UDPAddr
	stop      chan struct{}
	done      chan struct{}
	kick      chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	motd   string
	active bool
}

// StartAnnouncer advertises a world at the given local TCP port. With lanWide
// false only Minecraft on this computer sees it (unicast to 127.0.0.1:4445);
// with lanWide true it is multicast to the whole local network like a real LAN
// world. targets overrides where announcements go (for tests).
func StartAnnouncer(port int, lanWide bool, targets []*net.UDPAddr) (*Announcer, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		if lanWide {
			targets = []*net.UDPAddr{GroupAddr}
		} else {
			targets = []*net.UDPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: Port}}
		}
	}
	a := &Announcer{conn: conn, port: port, targets: targets, stop: make(chan struct{}), done: make(chan struct{}), kick: make(chan struct{}, 1)}
	go a.loop()
	return a, nil
}

// Set changes the advertised name, and whether to advertise at all.
func (a *Announcer) Set(motd string, active bool) {
	a.mu.Lock()
	changed := a.motd != motd || a.active != active
	a.motd, a.active = motd, active
	a.mu.Unlock()
	if changed {
		select {
		case a.kick <- struct{}{}:
		default:
		}
	}
}

// Active reports whether the world is currently being advertised.
func (a *Announcer) Active() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active
}

func (a *Announcer) loop() {
	defer close(a.done)
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	for {
		a.mu.Lock()
		motd, active := a.motd, a.active
		a.mu.Unlock()
		if active {
			msg := Format(motd, a.port)
			for _, dst := range a.targets {
				_, _ = a.conn.WriteToUDP(msg, dst)
			}
		}
		select {
		case <-a.stop:
			return
		case <-t.C:
		case <-a.kick:
		}
	}
}

// Close stops advertising.
func (a *Announcer) Close() {
	a.closeOnce.Do(func() {
		close(a.stop)
		<-a.done
		a.conn.Close()
	})
}
