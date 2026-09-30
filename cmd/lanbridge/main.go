// Command lanbridge lets friends join a Minecraft: Java Edition "Open to LAN"
// world over the internet. Run it with no arguments for the control panel.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"lanbridge/internal/app"
	"lanbridge/internal/invite"
	"lanbridge/internal/logx"
	"lanbridge/internal/relay"
	"lanbridge/internal/selftest"
	"lanbridge/internal/tunnel"
	"lanbridge/internal/ui"
)

// Set at build time with -ldflags "-X main.version=… -X main.defaultRelay=…".
var (
	version      = "dev"
	defaultRelay = ""
)

func main() {
	app.Version = version
	if u, err := relay.NormalizeURL(defaultRelay); err == nil {
		app.DefaultRelay = u
	}
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	var err error
	switch {
	case cmd == "help" || cmd == "-h" || cmd == "--help":
		usage()
	case cmd == "version" || cmd == "--version":
		fmt.Println("LANBridge", version, runtime.GOOS+"/"+runtime.GOARCH)
	case cmd == "" || cmd == "ui" || strings.HasPrefix(cmd, "-"):
		if cmd == "ui" {
			args = args[1:]
		}
		err = runUI(args)
	case cmd == "host":
		err = runHost(args[1:])
	case cmd == "join":
		err = runJoin(args[1:])
	case cmd == "relay":
		err = runRelay(args[1:])
	case cmd == "check":
		err = runCheck(args[1:])
	case cmd == "selftest":
		err = selftest.Run(context.Background(), os.Stdout)
	case invite.LooksLikeCode(cmd):
		err = runJoin(args)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q.\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		pauseIfDoubleClicked()
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`LANBridge ` + version + ` - play Minecraft "Open to LAN" worlds with friends over the internet.

Usage:
  lanbridge                     open the control panel (easiest)
  lanbridge host [flags]        share your Open to LAN world from the terminal
  lanbridge join <code>         join a friend's world from the terminal
  lanbridge check <code>        test whether a host can be reached
  lanbridge relay [flags]       run a relay server for your friends
  lanbridge selftest            check that everything works on this computer
  lanbridge version

Run "lanbridge <command> -h" for a command's flags.
`)
}

// pauseIfDoubleClicked keeps the console open on Windows so errors can be read.
func pauseIfDoubleClicked() {
	if runtime.GOOS == "windows" && len(os.Args) == 1 {
		fmt.Print("\nPress Enter to close this window.")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newLogger(verbose bool) *logx.Logger {
	l := logx.New(os.Stdout, 300)
	l.SetVerbose(verbose)
	return l
}

func runUI(args []string) error {
	fs := flag.NewFlagSet("lanbridge", flag.ContinueOnError)
	port := fs.Int("port", ui.DefaultPort, "control panel port (on 127.0.0.1 only)")
	noBrowser := fs.Bool("no-browser", false, "don't open the control panel in a browser")
	verbose := fs.Bool("v", false, "show detailed logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *port == ui.DefaultPort && ui.FocusExisting(*port) {
		fmt.Println("LANBridge is already running, so its control panel was opened again.")
		time.Sleep(1500 * time.Millisecond)
		return nil
	}
	log := newLogger(*verbose)
	a, err := app.New(log)
	if err != nil {
		return err
	}
	srv, err := ui.Start(a, *port)
	if err != nil {
		return err
	}
	fmt.Printf("\n  LANBridge %s\n\n  Control panel: %s\n\n  Keep this window open while you play. Close it or press Ctrl+C to quit.\n\n", version, srv.URL())
	if !*noBrowser {
		if err := ui.OpenBrowser(srv.URL()); err != nil {
			fmt.Println("  Couldn't open a browser. Open the link above yourself.")
		}
	}
	ctx, stop := signalContext()
	defer stop()
	select {
	case <-ctx.Done():
	case <-srv.Done():
	}
	fmt.Println("Shutting down...")
	a.Shutdown()
	srv.Close()
	return nil
}

func runHost(args []string) error {
	fs := flag.NewFlagSet("host", flag.ContinueOnError)
	mcPort := fs.Int("mc-port", 0, "the port Minecraft shows after Open to LAN (default: detect automatically)")
	port := fs.Int("port", 0, "port friends connect to directly (default: from settings, 42525)")
	relayURL := fs.String("relay", "", "relay address, e.g. tcp://203.0.113.7:7777 or wss://relay.example.com")
	noRelay := fs.Bool("no-relay", false, "don't use a relay")
	token := fs.String("token", "", "relay access token")
	noUPnP := fs.Bool("no-upnp", false, "don't ask the router to open a port")
	forwarded := fs.Bool("forwarded", false, "you forwarded the port on your router yourself")
	verbose := fs.Bool("v", false, "show detailed logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	o := app.HostOptions{MCPort: *mcPort, Port: *port}
	if set["relay"] {
		o.RelayURL = relayURL
	}
	if *noRelay {
		none := ""
		o.RelayURL = &none
	}
	if set["token"] {
		o.RelayToken = token
	}
	if *noUPnP {
		off := false
		o.UPnP = &off
	}
	if set["forwarded"] {
		o.Forwarded = forwarded
	}
	a, err := app.New(newLogger(*verbose))
	if err != nil {
		return err
	}
	if err := a.StartHost(o); err != nil {
		return err
	}
	defer a.Shutdown()
	ctx, stop := signalContext()
	defer stop()
	h := a.Host()
	for {
		if _, ready := h.Invite(); ready {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
	st := h.Status()
	fmt.Printf("\nInvite code (send this to your friends):\n\n  %s\n\n", st.Code)
	for _, r := range st.Routes {
		fmt.Printf("  [%s] %s: %s\n", map[string]string{"ok": " ok ", "wait": "....", "bad": "FAIL", "off": "off "}[r.State], r.Title, r.Detail)
	}
	if st.Target == "" {
		fmt.Println("\nWaiting for a world: in Minecraft press Esc -> Open to LAN -> Start LAN World.")
	}
	fmt.Println("\nPress Ctrl+C to stop hosting.")
	<-ctx.Done()
	return nil
}

func runJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	localPort := fs.Int("local-port", 0, "local port Minecraft connects to (default 25565, or any free port)")
	shareLAN := fs.Bool("share-lan", false, "let other devices on your network join through this computer")
	verbose := fs.Bool("v", false, "show detailed logs")
	code := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		code, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if code == "" && fs.NArg() > 0 {
		code = fs.Arg(0)
	}
	if code == "" {
		return errors.New("usage: lanbridge join <invite code>")
	}
	a, err := app.New(newLogger(*verbose))
	if err != nil {
		return err
	}
	if err := a.StartJoin(code, app.JoinOptions{LocalPort: *localPort, ShareLAN: *shareLAN}); err != nil {
		return err
	}
	defer a.Shutdown()
	j := a.Joiner()
	fmt.Printf("\nIn Minecraft, open Multiplayer: the world appears under the LAN heading.\nOr choose Direct Connection and enter: %s\n\nPress Ctrl+C to leave.\n\n", j.LocalAddr())
	ctx, stop := signalContext()
	defer stop()
	<-ctx.Done()
	return nil
}

func runRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	def := ":" + relay.DefaultPort
	if p := os.Getenv("PORT"); p != "" {
		def = ":" + p
	}
	listen := fs.String("listen", def, "address to listen on ($PORT is used when set)")
	token := fs.String("token", os.Getenv("LANBRIDGE_RELAY_TOKEN"), "access token hosts must use (default $LANBRIDGE_RELAY_TOKEN)")
	trust := fs.Bool("trust-proxy", os.Getenv("LANBRIDGE_TRUST_PROXY") == "1", "running behind a reverse proxy; take client addresses from X-Forwarded-For")
	verbose := fs.Bool("v", false, "show detailed logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger(*verbose)
	s := relay.NewServer(log)
	s.Token, s.TrustProxy, s.Version = *token, *trust, version
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	access := "open to any host"
	if *token != "" {
		access = "hosts need the access token"
	}
	log.Infof("LANBridge relay %s listening on %s (%s)", version, ln.Addr(), access)
	ctx, stop := signalContext()
	defer stop()
	go func() { <-ctx.Done(); ln.Close() }()
	if err := s.Serve(ln); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func runCheck(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: lanbridge check <invite code>")
	}
	inv, err := invite.Decode(args[0])
	if err != nil {
		return err
	}
	name := inv.Name
	if name == "" {
		name = "the host"
	}
	fmt.Printf("Invite from %s: %d direct address(es)", name, len(inv.Direct))
	if inv.Relay != "" {
		fmt.Printf(", relay %s", relay.Host(inv.Relay))
	}
	fmt.Println()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, route, err := tunnel.Check(ctx, inv)
	if err != nil {
		return err
	}
	fmt.Printf("Reached %s %s.\n", name, route)
	if info.Online {
		fmt.Printf("World open: %q (%d playing)\n", info.MOTD, info.Players)
	} else {
		fmt.Println("No world is open to LAN right now.")
	}
	return nil
}
