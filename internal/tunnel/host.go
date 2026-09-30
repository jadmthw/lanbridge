package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"lanbridge/internal/invite"
	"lanbridge/internal/lan"
	"lanbridge/internal/logx"
	"lanbridge/internal/netx"
	"lanbridge/internal/relay"
	"lanbridge/internal/stun"
	"lanbridge/internal/upnp"
)

// Version is reported to friends; set by the app.
var Version = "dev"

// DefaultHostPort is where a host listens for direct connections.
const DefaultHostPort = 42525

// HostConfig configures a host.
type HostConfig struct {
	Secret        [16]byte
	Name          string // shown to friends
	ListenAddr    string // default ":42525"
	UPnP          bool   // ask the router to forward the port
	ManualForward bool   // the user forwarded the port on their router by hand
	RelayURL      string
	RelayToken    string
	Target        string // a fixed Minecraft address; empty = auto-detect Open to LAN worlds
	NoDetect      bool   // don't listen for LAN announcements (tests)
	// Advertise, if set, replaces automatic discovery of the direct
	// addresses put in the invite (tests, unusual setups).
	Advertise func(port int) []netip.AddrPort
	Log       *logx.Logger
}

// Host shares a Minecraft world with friends.
type Host struct {
	cfg      HostConfig
	log      *logx.Logger
	tlsConf  *tls.Config
	ctx      context.Context
	cancel   context.CancelFunc
	ln       net.Listener
	port     int
	det      *lan.Detector
	detErr   string
	conns    netx.ConnSet
	wg       sync.WaitGroup
	room     string
	started  time.Time
	stopOnce sync.Once

	mu       sync.Mutex
	ready    bool
	selected string
	manual   string
	up       upnpInfo
	rel      relayInfo
	reach    string // "unknown", "yes", "no" or "n/a"
	publicIP netip.Addr
	players  map[*player]struct{}

	connCount atomic.Int64
	bytesUp   atomic.Int64
	bytesDown atomic.Int64
}

type upnpInfo struct {
	Status       string // off, searching, mapped, unavailable, blocked, failed
	Detail       string
	ExternalIP   string
	ExternalPort int
}

type relayInfo struct {
	Status string // off, connecting, online, error
	Error  string
}

type player struct {
	name   string
	intent int
	via    string
	since  time.Time
	up     atomic.Int64
	down   atomic.Int64
}

// StartHost starts listening for friends.
func StartHost(parent context.Context, cfg HostConfig) (*Host, error) {
	if cfg.Log == nil {
		cfg.Log = logx.Discard()
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", DefaultHostPort)
	}
	if cfg.RelayURL != "" {
		u, err := relay.NormalizeURL(cfg.RelayURL)
		if err != nil {
			return nil, err
		}
		cfg.RelayURL = u
	}
	tlsConf, err := newServerTLS()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		host, _, _ := net.SplitHostPort(cfg.ListenAddr)
		ln2, err2 := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err2 != nil {
			return nil, fmt.Errorf("can't open a port for friends to connect to: %w", err)
		}
		cfg.Log.Warnf("port %s is in use, so friends will connect on port %d instead", strings.TrimPrefix(cfg.ListenAddr, ":"), ln2.Addr().(*net.TCPAddr).Port)
		ln = ln2
	}
	ctx, cancel := context.WithCancel(parent)
	h := &Host{
		cfg: cfg, log: cfg.Log, tlsConf: tlsConf, ctx: ctx, cancel: cancel, ln: ln,
		port:    ln.Addr().(*net.TCPAddr).Port,
		room:    invite.Invite{Secret: cfg.Secret}.RoomID(),
		started: time.Now(), manual: cfg.Target, reach: "unknown",
		players: map[*player]struct{}{},
		up:      upnpInfo{Status: "off"},
		rel:     relayInfo{Status: "off"},
	}
	if !cfg.NoDetect {
		if det, err := lan.StartDetector(h.log); err != nil {
			h.detErr = err.Error()
			h.log.Warnf("%v; you can still type in the world's port", err)
		} else {
			h.det = det
		}
	}
	h.wg.Add(1)
	go h.acceptLoop()

	setup := make(chan struct{}, 2)
	pending := 0
	if cfg.UPnP {
		h.up.Status = "searching"
		pending++
		h.wg.Add(1)
		go h.runUPnP(setup)
	}
	if cfg.RelayURL != "" {
		h.rel.Status = "connecting"
		pending++
		h.wg.Add(1)
		go h.runRelay(setup)
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		timeout := time.NewTimer(9 * time.Second)
		defer timeout.Stop()
	wait:
		for i := 0; i < pending; i++ {
			select {
			case <-setup:
			case <-timeout.C:
				break wait
			case <-ctx.Done():
				return
			}
		}
		h.finishSetup()
	}()
	h.log.Infof("hosting: friends connect to port %d", h.port)
	return h, nil
}

// Port is the TCP port friends connect to directly.
func (h *Host) Port() int { return h.port }

// Stop disconnects everyone and cleans up (including the router port).
func (h *Host) Stop() {
	h.stopOnce.Do(func() {
		h.cancel()
		h.ln.Close()
		if h.det != nil {
			h.det.Close()
		}
		h.conns.CloseAll()
		done := make(chan struct{})
		go func() { h.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
		h.log.Infof("stopped hosting")
	})
}

func (h *Host) finishSetup() {
	h.mu.Lock()
	needIP := !h.publicIP.IsValid() && (h.cfg.ManualForward || (h.up.Status == "mapped" && h.up.ExternalIP == ""))
	h.mu.Unlock()
	if needIP {
		ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		ip, err := stun.PublicIPv4(ctx, nil)
		cancel()
		if err == nil {
			h.notePublicIP(ip)
		} else {
			h.log.Warnf("couldn't look up your public IP address: %v", err)
		}
	}
	h.checkReachable()
	h.mu.Lock()
	h.ready = true
	h.mu.Unlock()
	h.log.Infof("invite code ready")
}

func (h *Host) notePublicIP(ip netip.Addr) {
	ip = ip.Unmap()
	if !ip.Is4() || !netx.IsPublic(ip) {
		return
	}
	h.mu.Lock()
	if !h.publicIP.IsValid() {
		h.publicIP = ip
	}
	h.mu.Unlock()
}

// externalLocked is the public address friends can use, if there is one.
func (h *Host) externalLocked() netip.AddrPort {
	if h.up.Status == "mapped" {
		ip, err := netip.ParseAddr(h.up.ExternalIP)
		if err != nil {
			ip = h.publicIP
		}
		if netx.IsPublic(ip) {
			return netip.AddrPortFrom(ip, uint16(h.up.ExternalPort))
		}
	}
	if h.cfg.ManualForward && h.publicIP.IsValid() {
		return netip.AddrPortFrom(h.publicIP, uint16(h.port))
	}
	return netip.AddrPort{}
}

// checkReachable asks the relay to connect back to our public address.
func (h *Host) checkReachable() {
	h.mu.Lock()
	ext := h.externalLocked()
	online := h.rel.Status == "online"
	h.mu.Unlock()
	switch {
	case !ext.IsValid():
		h.setReach("n/a")
		return
	case !online:
		h.setReach("unknown")
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	c, rest, err := relay.Request(ctx, "tcp4", h.cfg.RelayURL, fmt.Sprintf("%s PROBE %d %s", relay.Proto, ext.Port(), randomHex(8)), 15*time.Second)
	if err != nil {
		h.log.Debugf("reachability check failed: %v", err)
		h.setReach("unknown")
		return
	}
	c.Close()
	if strings.HasPrefix(rest, "reachable") {
		h.setReach("yes")
		h.log.Infof("direct connections work: port %d is open to the internet", ext.Port())
	} else {
		h.setReach("no")
		h.log.Infof("port %d isn't reachable from the internet, so friends will use the relay", ext.Port())
	}
}

func (h *Host) setReach(v string) {
	h.mu.Lock()
	h.reach = v
	h.mu.Unlock()
}

func (h *Host) setUPnP(v upnpInfo) {
	h.mu.Lock()
	h.up = v
	h.mu.Unlock()
}

func (h *Host) setRelay(v relayInfo) {
	h.mu.Lock()
	h.rel = v
	h.mu.Unlock()
}

func (h *Host) runUPnP(setup chan<- struct{}) {
	defer h.wg.Done()
	var once sync.Once
	signal := func() { once.Do(func() { setup <- struct{}{} }) }
	defer signal()
	const desc = "LANBridge (Minecraft)"
	ctx, cancel := context.WithTimeout(h.ctx, 12*time.Second)
	cl, err := upnp.Discover(ctx, 3*time.Second)
	if err != nil {
		cancel()
		h.setUPnP(upnpInfo{Status: "unavailable", Detail: "No router answered the request to open a port. That's normal on school, work, dorm and many mesh networks."})
		h.log.Infof("UPnP: %v", err)
		return
	}
	extIP, ipErr := cl.ExternalIP(ctx)
	if ipErr == nil && !netx.IsPublic(extIP) {
		cancel()
		h.setUPnP(upnpInfo{Status: "blocked", ExternalIP: extIP.String(),
			Detail: "Your router sits behind another router or your provider's shared address (" + extIP.String() + "), so people on the internet can't reach it directly."})
		h.log.Infof("UPnP: the router's internet address %s is private (double NAT or CGNAT)", extIP)
		return
	}
	ext, lease, err := cl.Map(ctx, h.port, desc)
	cancel()
	if err != nil {
		h.setUPnP(upnpInfo{Status: "failed", Detail: "Your router refused to open a port: " + err.Error()})
		h.log.Infof("UPnP: couldn't open a port: %v", err)
		return
	}
	info := upnpInfo{Status: "mapped", ExternalPort: ext}
	if ipErr == nil {
		info.ExternalIP = extIP.String()
	}
	h.setUPnP(info)
	h.log.Infof("UPnP: your router now forwards port %d to this computer", ext)
	signal()
	t := time.NewTicker(20 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			if err := cl.Unmap(ctx, ext); err == nil {
				h.log.Infof("UPnP: closed port %d on your router", ext)
			}
			cancel()
			return
		case <-t.C:
			if lease > 0 {
				ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
				if err := cl.Renew(ctx, ext, h.port, lease, desc); err != nil {
					h.log.Warnf("UPnP: renewing the port failed: %v", err)
				}
				cancel()
			}
		}
	}
}

func (h *Host) runRelay(setup chan<- struct{}) {
	defer h.wg.Done()
	var once sync.Once
	signal := func() { once.Do(func() { setup <- struct{}{} }) }
	defer signal()
	backoff := time.Second
	for h.ctx.Err() == nil {
		start := time.Now()
		err := h.relaySession(signal)
		if h.ctx.Err() != nil {
			return
		}
		msg := explainRelayErr(err, h.cfg.RelayURL)
		h.setRelay(relayInfo{Status: "error", Error: msg})
		h.log.Warnf("relay: %s (trying again in %s)", msg, backoff)
		signal()
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (h *Host) relaySession(online func()) error {
	tok := h.cfg.RelayToken
	if tok == "" {
		tok = "-"
	}
	c, rest, err := relay.Request(h.ctx, "tcp", h.cfg.RelayURL, fmt.Sprintf("%s HOST %s %s", relay.Proto, h.room, tok), 20*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	if f := strings.Fields(rest); len(f) > 0 {
		if ip, err := netip.ParseAddr(f[0]); err == nil {
			h.notePublicIP(ip)
		}
	}
	h.setRelay(relayInfo{Status: "online"})
	h.log.Infof("relay: connected to %s", relay.Host(h.cfg.RelayURL))
	online()
	var wmu sync.Mutex
	send := func(s string) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := io.WriteString(c, s+"\n")
		return err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-h.ctx.Done():
				c.Close()
				return
			case <-t.C:
				if send("PING") != nil {
					c.Close()
					return
				}
			}
		}
	}()
	for {
		_ = c.SetReadDeadline(time.Now().Add(70 * time.Second))
		line, err := netx.ReadLine(c.R, 256)
		if err != nil {
			if h.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("lost the connection: %w", err)
		}
		switch {
		case line == "PING":
			_ = send("PONG")
		case strings.HasPrefix(line, "CONN "):
			go h.acceptRelay(strings.TrimSpace(line[5:]))
		}
	}
}

func (h *Host) acceptRelay(id string) {
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	c, _, err := relay.Request(ctx, "tcp", h.cfg.RelayURL, fmt.Sprintf("%s ACCEPT %s %s", relay.Proto, h.room, id), 15*time.Second)
	cancel()
	if err != nil {
		h.log.Debugf("relay: couldn't pick up a connection: %v", err)
		return
	}
	h.serveConn(c, "relay")
}

func explainRelayErr(err error, relayURL string) string {
	var re *relay.Error
	var dnsErr *net.DNSError
	switch {
	case err == nil:
		return "disconnected"
	case errors.As(err, &re):
		return re.Msg
	case errors.As(err, &dnsErr):
		return "can't find " + relay.Host(relayURL) + " (check the relay address)"
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused"):
		return relay.Host(relayURL) + " refused the connection (is the relay running?)"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.EOF):
		return "no answer from " + relay.Host(relayURL)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "no answer from " + relay.Host(relayURL)
	}
	return err.Error()
}

func (h *Host) acceptLoop() {
	defer h.wg.Done()
	for {
		c, err := h.ln.Accept()
		if err != nil {
			if h.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go h.handleDirect(c)
	}
}

func (h *Host) handleDirect(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(12 * time.Second))
	br := bufio.NewReaderSize(c, 4096)
	first, err := br.Peek(1)
	if err != nil {
		c.Close()
		return
	}
	if first[0] == 'L' { // reachability probe from a relay
		defer c.Close()
		line, err := netx.ReadLine(br, 128)
		if err == nil && strings.HasPrefix(line, "LBPROBE ") {
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, _ = io.WriteString(c, "LBPROBE-OK "+strings.TrimPrefix(line, "LBPROBE ")+"\n")
		}
		return
	}
	via := "direct"
	if ip := netx.RemoteIP(c.RemoteAddr()); ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		via = "same network"
	}
	h.serveConn(&netx.BufferedConn{Conn: c, R: br}, via)
}

func (h *Host) serveConn(raw net.Conn, via string) {
	if !h.conns.Add(raw) {
		return
	}
	defer h.conns.Remove(raw)
	defer raw.Close()
	tc, stream, respond, err := serverHandshake(raw, h.tlsConf, h.cfg.Secret[:])
	if err != nil {
		h.log.Debugf("turned away a %s connection: %v", via, err)
		return
	}
	switch stream {
	case StreamInfo:
		if respond(StatusOK) != nil {
			return
		}
		_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
		if writeMsg(tc, h.info()) == nil {
			_ = tc.CloseWrite()
			_, _ = io.Copy(io.Discard, tc) // wait for the friend to finish reading
		}
	case StreamGame:
		h.serveGame(tc, respond, via)
	default:
		_ = respond(StatusError)
	}
}

func (h *Host) serveGame(tc *tls.Conn, respond func(byte) error, via string) {
	target := h.Target()
	if target == "" {
		_ = respond(StatusOffline)
		h.log.Warnf("a friend tried to join, but no world is open to LAN right now")
		return
	}
	mc, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_ = respond(StatusOffline)
		h.log.Warnf("a friend tried to join, but Minecraft at %s didn't answer: %v", target, err)
		return
	}
	if !h.conns.Add(mc) {
		return
	}
	defer h.conns.Remove(mc)
	if respond(StatusOK) != nil {
		mc.Close()
		return
	}
	p := &player{via: via, since: time.Now()}
	h.mu.Lock()
	h.players[p] = struct{}{}
	h.mu.Unlock()
	h.connCount.Add(1)
	sn := &mcSniffer{cb: func(intent int, name string) { h.identify(p, intent, name) }}
	netx.Pipe(tc, mc, netx.PipeOptions{AToB: &p.down, BToA: &p.up, Sniff: sn.feed})
	h.mu.Lock()
	delete(h.players, p)
	playing := p.intent == intentLogin || p.intent == intentTransfer
	label := playerLabel(p)
	h.mu.Unlock()
	h.bytesUp.Add(p.up.Load())
	h.bytesDown.Add(p.down.Load())
	if playing {
		h.log.Infof("%s left", label)
	}
}

func (h *Host) identify(p *player, intent int, name string) {
	h.mu.Lock()
	p.intent, p.name = intent, name
	label := playerLabel(p)
	h.mu.Unlock()
	if intent == intentLogin || intent == intentTransfer {
		h.log.Infof("%s is joining (%s)", label, p.via)
	}
}

func playerLabel(p *player) string {
	if p.name != "" {
		return p.name
	}
	return "a friend"
}

// Target is the Minecraft address friends are forwarded to ("" = none).
func (h *Host) Target() string {
	h.mu.Lock()
	manual, selected := h.manual, h.selected
	h.mu.Unlock()
	if manual != "" {
		return manual
	}
	if w := h.world(selected); w != nil {
		return w.Addr
	}
	return ""
}

// world picks the world to share: the one chosen by the user, else one on this computer.
func (h *Host) world(selected string) *lan.World {
	if h.det == nil {
		return nil
	}
	ws := h.det.Worlds()
	for i := range ws {
		if selected != "" && ws[i].Key == selected {
			return &ws[i]
		}
	}
	for i := range ws {
		if ws[i].Local {
			return &ws[i]
		}
	}
	return nil
}

// SelectWorld shares a specific detected world ("" = automatic).
func (h *Host) SelectWorld(key string) {
	h.mu.Lock()
	h.selected, h.manual = key, ""
	h.mu.Unlock()
}

// SetManualTarget shares whatever listens at addr, e.g. "127.0.0.1:54321".
func (h *Host) SetManualTarget(addr string) {
	h.mu.Lock()
	h.manual = addr
	h.mu.Unlock()
	h.log.Infof("sharing the game at %s", addr)
}

func (h *Host) info() HostInfo {
	h.mu.Lock()
	manual, selected := h.manual, h.selected
	n := 0
	for p := range h.players {
		if p.intent == intentLogin || p.intent == intentTransfer {
			n++
		}
	}
	h.mu.Unlock()
	info := HostInfo{Name: h.cfg.Name, Version: Version, Players: n}
	if manual != "" {
		info.Online, info.MOTD = true, defaultMOTD(h.cfg.Name)
	} else if w := h.world(selected); w != nil {
		info.Online, info.MOTD = true, w.MOTD
	}
	return info
}

func defaultMOTD(name string) string {
	if name == "" {
		return "Minecraft world"
	}
	return name + "'s world"
}

// candidates are the direct addresses written into the invite.
func (h *Host) candidates() []netip.AddrPort {
	if h.cfg.Advertise != nil {
		return h.cfg.Advertise(h.port)
	}
	h.mu.Lock()
	ext := h.externalLocked()
	reach := h.reach
	h.mu.Unlock()
	var out []netip.AddrPort
	add := func(ap netip.AddrPort) {
		for _, x := range out {
			if x == ap {
				return
			}
		}
		out = append(out, ap)
	}
	if ext.IsValid() && reach != "no" {
		add(ext)
	}
	lan4, pub4, v6 := netx.LocalAddrs()
	p := uint16(h.port)
	for i, ip := range pub4 {
		if i < 1 {
			add(netip.AddrPortFrom(ip, p))
		}
	}
	for i, ip := range v6 {
		if i < 2 {
			add(netip.AddrPortFrom(ip, p))
		}
	}
	for i, ip := range lan4 {
		if i < 2 {
			add(netip.AddrPortFrom(ip, p))
		}
	}
	return out
}

// Invite returns the current invite and whether setup has finished.
func (h *Host) Invite() (invite.Invite, bool) {
	inv := invite.Invite{Secret: h.cfg.Secret, Relay: h.cfg.RelayURL, Name: h.cfg.Name, Direct: h.candidates()}
	h.mu.Lock()
	defer h.mu.Unlock()
	return inv, h.ready
}

// RouteView describes one way friends can reach the host.
type RouteView struct {
	State  string `json:"state"` // ok, wait, bad, off
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// PlayerView describes a connected friend.
type PlayerView struct {
	Name  string    `json:"name"`
	Via   string    `json:"via"`
	Since time.Time `json:"since"`
	Up    int64     `json:"up"`
	Down  int64     `json:"down"`
}

// HostStatus is a snapshot for the control panel.
type HostStatus struct {
	Ready       bool         `json:"ready"`
	Code        string       `json:"code,omitempty"`
	Name        string       `json:"name"`
	Port        int          `json:"port"`
	Worlds      []lan.World  `json:"worlds"`
	Selected    string       `json:"selected"`
	Target      string       `json:"target"`
	Manual      string       `json:"manual,omitempty"`
	DetectError string       `json:"detectError,omitempty"`
	Routes      []RouteView  `json:"routes"`
	Relay       string       `json:"relay"`
	Players     []PlayerView `json:"players"`
	Connections int64        `json:"connections"`
	BytesUp     int64        `json:"bytesUp"`
	BytesDown   int64        `json:"bytesDown"`
	Since       time.Time    `json:"since"`
}

// Status returns a snapshot for the control panel.
func (h *Host) Status() HostStatus {
	inv, ready := h.Invite()
	worlds := []lan.World{}
	if h.det != nil {
		worlds = h.det.Worlds()
	}
	st := HostStatus{Ready: ready, Name: h.cfg.Name, Port: h.port, Worlds: worlds, DetectError: h.detErr,
		Connections: h.connCount.Load(), Since: h.started, Players: []PlayerView{}}
	up, down := h.bytesUp.Load(), h.bytesDown.Load()
	h.mu.Lock()
	st.Selected, st.Manual, st.Relay = h.selected, h.manual, h.rel.Status
	for p := range h.players {
		up += p.up.Load()
		down += p.down.Load()
		if p.intent == intentStatus || (p.intent == intentUnknown && time.Since(p.since) < 3*time.Second) {
			continue
		}
		st.Players = append(st.Players, PlayerView{Name: playerLabel(p), Via: p.via, Since: p.since, Up: p.up.Load(), Down: p.down.Load()})
	}
	st.Routes = h.routesLocked()
	h.mu.Unlock()
	sort.Slice(st.Players, func(i, j int) bool { return st.Players[i].Since.Before(st.Players[j].Since) })
	st.BytesUp, st.BytesDown = up, down
	st.Target = h.Target()
	if ready {
		st.Code = inv.Encode()
	}
	return st
}

func (h *Host) routesLocked() []RouteView {
	var out []RouteView
	out = append(out, RouteView{"ok", "Same Wi-Fi or network", "Friends on your network can always join."})
	d := RouteView{Title: "Direct over the internet"}
	ext := h.externalLocked()
	switch {
	case h.cfg.ManualForward && h.up.Status != "mapped":
		if ext.IsValid() {
			d.State, d.Detail = "ok", fmt.Sprintf("Using the port you forwarded: %s.", ext)
		} else {
			d.State, d.Detail = "wait", fmt.Sprintf("Using the port you forwarded (%d). Looking up your public address…", h.port)
		}
	case h.up.Status == "mapped":
		where := fmt.Sprintf("port %d", h.up.ExternalPort)
		if ext.IsValid() {
			where = ext.String()
		}
		d.State, d.Detail = "ok", "Your router opened "+where+"."
	case h.up.Status == "searching":
		d.State, d.Detail = "wait", "Asking your router to open a port…"
	case h.up.Status == "off":
		d.State, d.Detail = "off", "Opening a port on your router is turned off in Settings."
	default:
		d.State, d.Detail = "bad", h.up.Detail
	}
	if d.State == "ok" {
		switch h.reach {
		case "yes":
			d.Detail += " Checked: it's reachable from the internet."
		case "no":
			d.State = "bad"
			d.Detail += " But it isn't reachable from the internet (a firewall or a second router may be in the way)."
		}
	}
	out = append(out, d)
	r := RouteView{Title: "Relay"}
	host := relay.Host(h.cfg.RelayURL)
	switch h.rel.Status {
	case "online":
		r.State, r.Detail = "ok", "Connected to "+host+". Friends can join from any network."
	case "connecting":
		r.State, r.Detail = "wait", "Connecting to "+host+"…"
	case "error":
		r.State, r.Detail = "bad", h.rel.Error+". Retrying."
	default:
		r.State, r.Detail = "off", "No relay set up. Friends on other networks can only join if the direct route works. Add a relay in Settings."
	}
	return append(out, r)
}
