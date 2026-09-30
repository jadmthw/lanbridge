package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"lanbridge/internal/invite"
	"lanbridge/internal/lan"
	"lanbridge/internal/logx"
	"lanbridge/internal/netx"
	"lanbridge/internal/relay"
)

// JoinConfig configures a joiner.
type JoinConfig struct {
	Invite     invite.Invite
	LocalPort  int  // 0: 25565, or any free port if taken; >0: that port if free; <0: any free port
	ShareLAN   bool // let other devices on this network join through this computer
	NoAnnounce bool
	AnnounceTo []*net.UDPAddr // override where LAN announcements go (tests)
	Log        *logx.Logger
}

type route struct {
	direct netip.AddrPort
	relay  bool
}

func (r route) String() string {
	if r.relay {
		return "relay"
	}
	return r.direct.String()
}

func (r route) describe(relayURL string) string {
	if r.relay {
		return "through the relay (" + relay.Host(relayURL) + ")"
	}
	switch netx.Describe(r.direct.Addr()) {
	case "same network":
		return "directly on your network"
	case "this computer":
		return "directly on this computer"
	case "IPv6":
		return "directly over IPv6"
	}
	return "directly over the internet"
}

// Joiner makes a friend's world show up as a LAN world on this computer.
type Joiner struct {
	cfg      JoinConfig
	log      *logx.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	lns      []net.Listener
	port     int
	ann      *lan.Announcer
	conns    netx.ConnSet
	room     string
	wg       sync.WaitGroup
	stopOnce sync.Once

	mu        sync.Mutex
	info      HostInfo
	checked   bool
	connected bool
	lastErr   string
	route     *route
	rtt       time.Duration
	sessions  int

	bytesUp   atomic.Int64
	bytesDown atomic.Int64
}

// StartJoin opens the local port Minecraft connects to and starts watching the host.
func StartJoin(parent context.Context, cfg JoinConfig) (*Joiner, error) {
	if cfg.Log == nil {
		cfg.Log = logx.Discard()
	}
	if len(cfg.Invite.Direct) == 0 && cfg.Invite.Relay == "" {
		return nil, errors.New("this invite doesn't include any way to reach the host")
	}
	if cfg.Invite.Relay != "" {
		u, err := relay.NormalizeURL(cfg.Invite.Relay)
		if err != nil {
			return nil, err
		}
		cfg.Invite.Relay = u
	}
	lns, port, err := listenLocal(cfg.LocalPort, cfg.ShareLAN)
	if err != nil {
		return nil, fmt.Errorf("can't open a local port for Minecraft: %w", err)
	}
	if want := wantPort(cfg.LocalPort); want > 0 && port != want {
		cfg.Log.Warnf("port %d is busy, so Minecraft will use port %d instead", want, port)
	}
	ctx, cancel := context.WithCancel(parent)
	j := &Joiner{cfg: cfg, log: cfg.Log, ctx: ctx, cancel: cancel, lns: lns, port: port, room: cfg.Invite.RoomID()}
	if !cfg.NoAnnounce {
		if ann, err := lan.StartAnnouncer(port, cfg.ShareLAN, cfg.AnnounceTo); err != nil {
			j.log.Warnf("can't add the world to Minecraft's LAN list (%v); use Direct Connection to %s instead", err, j.LocalAddr())
		} else {
			j.ann = ann
		}
	}
	for _, ln := range lns {
		j.wg.Add(1)
		go j.acceptLoop(ln)
	}
	j.wg.Add(1)
	go j.monitor()
	return j, nil
}

func wantPort(p int) int {
	switch {
	case p == 0:
		return 25565
	case p < 0:
		return 0
	}
	return p
}

func listenLocal(p int, shareLAN bool) ([]net.Listener, int, error) {
	try := func(port int) ([]net.Listener, int, error) {
		if shareLAN {
			ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
			if err != nil {
				return nil, 0, err
			}
			return []net.Listener{ln}, ln.Addr().(*net.TCPAddr).Port, nil
		}
		ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil, 0, err
		}
		got := ln.Addr().(*net.TCPAddr).Port
		lns := []net.Listener{ln}
		if ln6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", got)); err == nil {
			lns = append(lns, ln6) // "localhost" may resolve to ::1
		}
		return lns, got, nil
	}
	if want := wantPort(p); want > 0 {
		if lns, port, err := try(want); err == nil {
			return lns, port, nil
		}
	}
	return try(0)
}

// LocalAddr is what to type into Minecraft's Direct Connection box.
func (j *Joiner) LocalAddr() string {
	if j.port == 25565 {
		return "localhost"
	}
	return fmt.Sprintf("localhost:%d", j.port)
}

// Port is the local port Minecraft connects to.
func (j *Joiner) Port() int { return j.port }

// Stop closes everything.
func (j *Joiner) Stop() {
	j.stopOnce.Do(func() {
		j.cancel()
		for _, ln := range j.lns {
			ln.Close()
		}
		if j.ann != nil {
			j.ann.Close()
		}
		j.conns.CloseAll()
		done := make(chan struct{})
		go func() { j.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		j.log.Infof("left the game session")
	})
}

func (j *Joiner) monitor() {
	defer j.wg.Done()
	for {
		wait := 10 * time.Second
		if !j.check() {
			wait = 4 * time.Second
		}
		select {
		case <-j.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (j *Joiner) check() bool {
	ctx, cancel := context.WithTimeout(j.ctx, 20*time.Second)
	defer cancel()
	tc, _, r, rtt, err := j.dial(ctx, StreamInfo)
	if err != nil {
		if j.ctx.Err() == nil {
			j.setDown(err)
		}
		return false
	}
	defer tc.Close()
	_ = tc.SetReadDeadline(time.Now().Add(10 * time.Second))
	var info HostInfo
	if err := readMsg(tc, &info); err != nil {
		if j.ctx.Err() == nil {
			j.setDown(fmt.Errorf("the host didn't send its status: %w", err))
		}
		return false
	}
	j.setUp(info, r, rtt)
	return true
}

func hostLabel(name string) string {
	if name == "" {
		return "the host"
	}
	return name
}

func (j *Joiner) setUp(info HostInfo, r route, rtt time.Duration) {
	j.mu.Lock()
	was, wasOnline, prev := j.connected, j.info.Online, j.route
	j.info, j.checked, j.connected, j.lastErr, j.rtt = info, true, true, "", rtt
	j.route = &r
	j.mu.Unlock()
	if j.ann != nil {
		j.ann.Set(info.MOTD, info.Online)
	}
	if !was || prev == nil || *prev != r {
		j.log.Infof("connected to %s %s (%d ms)", hostLabel(info.Name), r.describe(j.cfg.Invite.Relay), rtt.Milliseconds())
	}
	switch {
	case info.Online && (!was || !wasOnline):
		j.log.Infof("%q is open: find it under Multiplayer (LAN), or use Direct Connection: %s", info.MOTD, j.LocalAddr())
	case !info.Online && (!was || wasOnline):
		j.log.Infof("%s hasn't opened a world to LAN yet", hostLabel(info.Name))
	}
}

func (j *Joiner) setDown(err error) {
	msg := err.Error()
	j.mu.Lock()
	changed := j.connected || j.lastErr != msg
	j.checked, j.connected, j.lastErr = true, false, msg
	j.mu.Unlock()
	if j.ann != nil {
		j.ann.Set("", false)
	}
	if changed {
		j.log.Warnf("can't reach %s: %s", hostLabel(j.cfg.Invite.Name), msg)
	}
}

func (j *Joiner) routes() []route {
	var rs []route
	for _, d := range j.cfg.Invite.Direct {
		rs = append(rs, route{direct: d})
	}
	if j.cfg.Invite.Relay != "" {
		rs = append(rs, route{relay: true})
	}
	return rs
}

type dialResult struct {
	tc     *tls.Conn
	status byte
	r      route
	rtt    time.Duration
	err    error
}

// dial tries every route at once (direct ones first, the relay a moment
// later, the last route that worked slightly ahead) and keeps the first one
// that completes the secure handshake.
func (j *Joiner) dial(ctx context.Context, stream byte) (*tls.Conn, byte, route, time.Duration, error) {
	all := j.routes()
	j.mu.Lock()
	var pref *route
	if j.route != nil {
		p := *j.route
		pref = &p
	}
	j.mu.Unlock()
	hasDirect := len(j.cfg.Invite.Direct) > 0
	ctx, cancel := context.WithCancel(ctx)
	results := make(chan dialResult, len(all))
	for _, r := range all {
		var delay time.Duration
		switch {
		case pref != nil && *pref == r:
		case pref != nil:
			delay = 1200 * time.Millisecond
		case r.relay && hasDirect:
			delay = 600 * time.Millisecond
		}
		go func(r route, delay time.Duration) {
			if delay > 0 {
				t := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					t.Stop()
					results <- dialResult{r: r, err: context.Canceled}
					return
				case <-t.C:
				}
			}
			raw, err := j.dialRaw(ctx, r)
			if err != nil {
				results <- dialResult{r: r, err: err}
				return
			}
			tc, status, rtt, err := clientHandshake(ctx, raw, j.cfg.Invite.Secret[:], stream)
			if err != nil {
				raw.Close()
				results <- dialResult{r: r, err: err}
				return
			}
			results <- dialResult{tc: tc, status: status, r: r, rtt: rtt}
		}(r, delay)
	}
	var failed []dialResult
	for i := range all {
		res := <-results
		if res.err == nil {
			cancel()
			if left := len(all) - i - 1; left > 0 {
				go func() {
					for k := 0; k < left; k++ {
						if x := <-results; x.tc != nil {
							x.tc.Close()
						}
					}
				}()
			}
			return res.tc, res.status, res.r, res.rtt, nil
		}
		failed = append(failed, res)
	}
	cancel()
	return nil, 0, route{}, 0, j.summarize(failed)
}

func (j *Joiner) summarize(failed []dialResult) error {
	var relayErr error
	for _, f := range failed {
		if errors.Is(f.err, ErrRejected) {
			return ErrRejected
		}
		if f.r.relay {
			relayErr = f.err
		}
		j.log.Debugf("route %s failed: %v", f.r, f.err)
	}
	var re *relay.Error
	switch {
	case errors.As(relayErr, &re):
		return errors.New(re.Msg)
	case j.cfg.Invite.Relay == "":
		return errors.New("no direct route to the host worked, and their invite has no relay")
	case relayErr != nil:
		return fmt.Errorf("direct routes failed and the relay did too (%v)", relayErr)
	}
	return errors.New("couldn't reach the host")
}

func (j *Joiner) dialRaw(ctx context.Context, r route) (net.Conn, error) {
	if r.relay {
		c, _, err := relay.Request(ctx, "tcp", j.cfg.Invite.Relay, fmt.Sprintf("%s JOIN %s", relay.Proto, j.room), 20*time.Second)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	d := net.Dialer{Timeout: 6 * time.Second, KeepAlive: 20 * time.Second}
	return d.DialContext(ctx, "tcp", r.direct.String())
}

func (j *Joiner) acceptLoop(ln net.Listener) {
	defer j.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			if j.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go j.handleLocal(c)
	}
}

func (j *Joiner) handleLocal(local net.Conn) {
	if !j.conns.Add(local) {
		return
	}
	defer j.conns.Remove(local)
	defer local.Close()
	ctx, cancel := context.WithTimeout(j.ctx, 20*time.Second)
	tc, status, r, rtt, err := j.dial(ctx, StreamGame)
	cancel()
	if err != nil {
		if j.ctx.Err() == nil {
			j.setDown(err)
		}
		return
	}
	if !j.conns.Add(tc) {
		return
	}
	defer j.conns.Remove(tc)
	if status != StatusOK {
		tc.Close()
		j.log.Warnf("Minecraft tried to connect, but %s has no world open right now", hostLabel(j.cfg.Invite.Name))
		return
	}
	j.mu.Lock()
	j.sessions++
	j.route, j.rtt = &r, rtt
	j.mu.Unlock()
	netx.Pipe(local, tc, netx.PipeOptions{AToB: &j.bytesUp, BToA: &j.bytesDown})
	j.mu.Lock()
	j.sessions--
	j.mu.Unlock()
}

// JoinStatus is a snapshot for the control panel.
type JoinStatus struct {
	HostName    string `json:"hostName"`
	MOTD        string `json:"motd"`
	WorldOnline bool   `json:"worldOnline"`
	HostPlayers int    `json:"hostPlayers"`
	HostVersion string `json:"hostVersion,omitempty"`
	Checked     bool   `json:"checked"`
	Connected   bool   `json:"connected"`
	Route       string `json:"route,omitempty"`
	PingMs      int64  `json:"pingMs"`
	Error       string `json:"error,omitempty"`
	LocalAddr   string `json:"localAddr"`
	LocalPort   int    `json:"localPort"`
	Announcing  bool   `json:"announcing"`
	ShareLAN    bool   `json:"shareLan"`
	Sessions    int    `json:"sessions"`
	BytesUp     int64  `json:"bytesUp"`
	BytesDown   int64  `json:"bytesDown"`
}

// Status returns a snapshot for the control panel.
func (j *Joiner) Status() JoinStatus {
	j.mu.Lock()
	st := JoinStatus{HostName: j.info.Name, MOTD: j.info.MOTD, WorldOnline: j.info.Online, HostPlayers: j.info.Players,
		HostVersion: j.info.Version, Checked: j.checked, Connected: j.connected, PingMs: j.rtt.Milliseconds(),
		Error: j.lastErr, Sessions: j.sessions}
	if j.route != nil && j.connected {
		st.Route = j.route.describe(j.cfg.Invite.Relay)
	}
	j.mu.Unlock()
	if st.HostName == "" {
		st.HostName = j.cfg.Invite.Name
	}
	st.LocalAddr, st.LocalPort, st.ShareLAN = j.LocalAddr(), j.port, j.cfg.ShareLAN
	if j.ann != nil {
		st.Announcing = j.ann.Active()
	}
	st.BytesUp, st.BytesDown = j.bytesUp.Load(), j.bytesDown.Load()
	return st
}

// Check connects to the host once and reports its status and the route used.
func Check(ctx context.Context, inv invite.Invite) (HostInfo, string, error) {
	if inv.Relay != "" {
		u, err := relay.NormalizeURL(inv.Relay)
		if err != nil {
			return HostInfo{}, "", err
		}
		inv.Relay = u
	}
	j := &Joiner{cfg: JoinConfig{Invite: inv}, log: logx.Discard(), room: inv.RoomID()}
	tc, _, r, _, err := j.dial(ctx, StreamInfo)
	if err != nil {
		return HostInfo{}, "", err
	}
	defer tc.Close()
	_ = tc.SetReadDeadline(time.Now().Add(10 * time.Second))
	var info HostInfo
	if err := readMsg(tc, &info); err != nil {
		return HostInfo{}, "", err
	}
	return info, r.describe(inv.Relay), nil
}
