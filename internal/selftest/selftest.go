// Package selftest runs LANBridge end to end on this computer: LAN
// announcements, a direct tunnel, relayed tunnels over TCP and WebSocket, and
// refusal of a wrong invite. It needs no internet access.
package selftest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"lanbridge/internal/invite"
	"lanbridge/internal/lan"
	"lanbridge/internal/logx"
	"lanbridge/internal/relay"
	"lanbridge/internal/tunnel"
)

// Run executes every check and prints one line per check.
func Run(ctx context.Context, out io.Writer) error {
	checks := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"LAN world announcement", checkLAN},
		{"direct tunnel", func(ctx context.Context) error { return checkTunnel(ctx, "direct") }},
		{"relay tunnel (TCP)", func(ctx context.Context) error { return checkTunnel(ctx, "tcp") }},
		{"relay tunnel (WebSocket)", func(ctx context.Context) error { return checkTunnel(ctx, "ws") }},
		{"wrong invite is refused", checkReject},
	}
	failed := 0
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		start := time.Now()
		err := c.fn(cctx)
		cancel()
		if err != nil {
			failed++
			fmt.Fprintf(out, "[FAIL] %s: %v\n", c.name, err)
			continue
		}
		fmt.Fprintf(out, "[ ok ] %s (%d ms)\n", c.name, time.Since(start).Milliseconds())
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(checks))
	}
	fmt.Fprintln(out, "All checks passed.")
	return nil
}

func checkLAN(ctx context.Context) error {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	defer pc.Close()
	ann, err := lan.StartAnnouncer(25599, false, []*net.UDPAddr{pc.LocalAddr().(*net.UDPAddr)})
	if err != nil {
		return err
	}
	defer ann.Close()
	ann.Set("Self-test world", true)
	_ = pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	n, _, err := pc.ReadFromUDP(buf)
	if err != nil {
		return fmt.Errorf("no announcement arrived: %w", err)
	}
	motd, port, ok := lan.Parse(buf[:n])
	if !ok || motd != "Self-test world" || port != 25599 {
		return fmt.Errorf("unexpected announcement %q", buf[:n])
	}
	return nil
}

func startEcho() (net.Listener, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln, nil
}

func loopback(port int) []netip.AddrPort {
	return []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))}
}

func startHost(ctx context.Context, mode, target string) (*tunnel.Host, func(), error) {
	cleanup := func() {}
	cfg := tunnel.HostConfig{Name: "selftest", ListenAddr: "127.0.0.1:0", Target: target, NoDetect: true, Log: logx.Discard()}
	var err error
	if cfg.Secret, err = invite.NewSecret(); err != nil {
		return nil, cleanup, err
	}
	if mode == "direct" {
		cfg.Advertise = loopback
	} else {
		cfg.Advertise = func(int) []netip.AddrPort { return nil }
		rl, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, cleanup, err
		}
		go relay.NewServer(nil).Serve(rl)
		cleanup = func() { rl.Close() }
		cfg.RelayURL = "tcp://" + rl.Addr().String()
		if mode == "ws" {
			cfg.RelayURL = "ws://" + rl.Addr().String() + "/"
		}
	}
	h, err := tunnel.StartHost(ctx, cfg)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return h, func() { h.Stop(); cleanup() }, nil
}

func waitReady(ctx context.Context, h *tunnel.Host) (invite.Invite, error) {
	for {
		inv, ready := h.Invite()
		st := h.Status()
		if ready && (st.Relay == "off" || st.Relay == "online") {
			return inv, nil
		}
		select {
		case <-ctx.Done():
			return invite.Invite{}, fmt.Errorf("the host never became ready (relay: %s)", st.Relay)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func checkTunnel(ctx context.Context, mode string) error {
	echo, err := startEcho()
	if err != nil {
		return err
	}
	defer echo.Close()
	h, stop, err := startHost(ctx, mode, echo.Addr().String())
	if err != nil {
		return err
	}
	defer stop()
	inv, err := waitReady(ctx, h)
	if err != nil {
		return err
	}
	inv, err = invite.Decode(inv.Encode()) // go through a real code
	if err != nil {
		return err
	}
	info, _, err := tunnel.Check(ctx, inv)
	if err != nil {
		return fmt.Errorf("status check: %w", err)
	}
	if !info.Online || info.Name != "selftest" {
		return fmt.Errorf("unexpected host status %+v", info)
	}
	j, err := tunnel.StartJoin(ctx, tunnel.JoinConfig{Invite: inv, LocalPort: -1, NoAnnounce: true, Log: logx.Discard()})
	if err != nil {
		return err
	}
	defer j.Stop()
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func(size int) { errs <- roundTrip(fmt.Sprintf("127.0.0.1:%d", j.Port()), size) }((i + 1) * 96 * 1024)
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			return err
		}
	}
	return nil
}

func roundTrip(addr string, size int) error {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	werr := make(chan error, 1)
	go func() { _, err := c.Write(payload); werr <- err }()
	got := make([]byte, size)
	if _, err := io.ReadFull(c, got); err != nil {
		return fmt.Errorf("reading the echo: %w", err)
	}
	if err := <-werr; err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		return errors.New("data came back corrupted")
	}
	return nil
}

func checkReject(ctx context.Context) error {
	echo, err := startEcho()
	if err != nil {
		return err
	}
	defer echo.Close()
	h, stop, err := startHost(ctx, "direct", echo.Addr().String())
	if err != nil {
		return err
	}
	defer stop()
	inv, err := waitReady(ctx, h)
	if err != nil {
		return err
	}
	inv.Secret[0] ^= 0xFF
	if _, _, err := tunnel.Check(ctx, inv); !errors.Is(err, tunnel.ErrRejected) {
		return fmt.Errorf("expected the host to refuse, got: %v", err)
	}
	return nil
}
