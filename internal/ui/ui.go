// Package ui serves LANBridge's control panel: a local web page, opened in
// the default browser, that only this computer can reach.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"lanbridge/internal/ai"
	"lanbridge/internal/app"
	"lanbridge/internal/bots"
)

//go:embed index.html
var indexHTML []byte

//go:embed ai.html
var aiHTML []byte

// DefaultPort is where the control panel listens when it can.
const DefaultPort = 47474

// Server is the running control panel.
type Server struct {
	app      *app.App
	token    string
	port     int
	srv      *http.Server
	done     chan struct{}
	doneOnce sync.Once

	focusMu   sync.Mutex
	lastFocus time.Time
}

// Start serves the control panel on 127.0.0.1:port, or a random port if that's taken.
func Start(a *app.App, port int) (*Server, error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if ln, err = net.Listen("tcp4", "127.0.0.1:0"); err != nil {
			return nil, err
		}
	}
	tok := make([]byte, 18)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	s := &Server{app: a, token: base64.RawURLEncoding.EncodeToString(tok), port: ln.Addr().(*net.TCPAddr).Port, done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/ping", s.ping)
	mux.HandleFunc("/api/focus", s.focus)
	mux.HandleFunc("/api/state", s.authed(false, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.app.State()) }))
	mux.HandleFunc("/api/host/start", s.authed(true, s.hostStart))
	mux.HandleFunc("/api/host/stop", s.authed(true, func(w http.ResponseWriter, r *http.Request) { s.app.StopHost(); ok(w) }))
	mux.HandleFunc("/api/host/select", s.authed(true, s.hostSelect))
	mux.HandleFunc("/api/host/newcode", s.authed(true, func(w http.ResponseWriter, r *http.Request) { reply(w, s.app.NewCode()) }))
	mux.HandleFunc("/api/join/start", s.authed(true, s.joinStart))
	mux.HandleFunc("/api/join/stop", s.authed(true, func(w http.ResponseWriter, r *http.Request) { s.app.StopJoin(); ok(w) }))
	mux.HandleFunc("/api/settings", s.authed(true, s.settings))
	mux.HandleFunc("/api/quit", s.authed(true, s.quit))
	mux.HandleFunc("/ai", s.aiPage)
	mux.HandleFunc("/api/ai/state", s.authed(false, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.app.AIStatus()) }))
	mux.HandleFunc("/api/ai/provider", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var u app.ProviderUpdate
		if decode(w, r, &u) {
			reply(w, s.app.AISetProvider(u))
		}
	}))
	mux.HandleFunc("/api/ai/test", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Kind ai.Kind `json:"kind"`
		}
		if !decode(w, r, &b) {
			return
		}
		msg, err := s.app.AITest(r.Context(), b.Kind)
		if err != nil {
			reply(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
	}))
	mux.HandleFunc("/api/ai/models", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Kind ai.Kind `json:"kind"`
		}
		if !decode(w, r, &b) {
			return
		}
		models, err := s.app.AIModels(r.Context(), b.Kind)
		if err != nil {
			reply(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
	}))
	mux.HandleFunc("/api/ai/install", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Dir string `json:"dir"`
		}
		if !decode(w, r, &b) {
			return
		}
		path, err := s.app.AIInstall(b.Dir)
		if err != nil {
			reply(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path})
	}))
	mux.HandleFunc("/api/ai/folder", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Dir string `json:"dir"`
		}
		if decode(w, r, &b) {
			reply(w, s.app.AIAddFolder(b.Dir))
		}
	}))
	mux.HandleFunc("/api/ai/spawn", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var req bots.SpawnRequest
		if !decode(w, r, &req) {
			return
		}
		name, err := s.app.AISpawn(req)
		if err != nil {
			reply(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
	}))
	mux.HandleFunc("/api/ai/remove", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name string `json:"name"`
		}
		if decode(w, r, &b) {
			reply(w, s.app.AIRemove(b.Name))
		}
	}))
	mux.HandleFunc("/api/ai/stop", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name string `json:"name"`
		}
		if decode(w, r, &b) {
			reply(w, s.app.AIStop(b.Name))
		}
	}))
	mux.HandleFunc("/api/ai/options", s.authed(true, func(w http.ResponseWriter, r *http.Request) {
		var o app.AIOptions
		if decode(w, r, &o) {
			reply(w, s.app.AISetOptions(o))
		}
	}))
	s.srv = &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// URL opens the control panel, including its access token.
func (s *Server) URL() string { return fmt.Sprintf("http://127.0.0.1:%d/#%s", s.port, s.token) }

// Done is closed when the user presses Quit.
func (s *Server) Done() <-chan struct{} { return s.done }

// Close stops serving.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}

// guard rejects requests that don't come to our own address (DNS rebinding).
func (s *Server) guard(next http.Handler) http.Handler {
	want1, want2 := fmt.Sprintf("127.0.0.1:%d", s.port), fmt.Sprintf("localhost:%d", s.port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != want1 && r.Host != want2 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authed(post bool, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-LANBridge-Token")), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "This page's access link is out of date. Reopen LANBridge."})
			return
		}
		if post && r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
			return
		}
		fn(w, r)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	_, _ = w.Write(indexHTML)
}

func (s *Server) aiPage(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	_, _ = w.Write(aiHTML)
}

// ping lets a second copy of LANBridge find this one.
func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"app": "lanbridge", "version": app.Version})
}

// focus asks this copy to open its window. The custom header means web pages
// can't trigger it (browsers would need a CORS preflight we never grant).
func (s *Server) focus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-LANBridge-Focus") != "1" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.focusMu.Lock()
	open := time.Since(s.lastFocus) > 3*time.Second
	if open {
		s.lastFocus = time.Now()
	}
	s.focusMu.Unlock()
	if open {
		_ = OpenBrowser(s.URL())
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": "lanbridge"})
}

func (s *Server) hostStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MCPort int `json:"mcPort"`
	}
	if !decode(w, r, &body) {
		return
	}
	reply(w, s.app.StartHost(app.HostOptions{MCPort: body.MCPort}))
}

func (s *Server) hostSelect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key        string `json:"key"`
		ManualPort int    `json:"manualPort"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.ManualPort != 0 {
		reply(w, s.app.SetManualPort(body.ManualPort))
		return
	}
	reply(w, s.app.SelectWorld(body.Key))
}

func (s *Server) joinStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	reply(w, s.app.StartJoin(body.Code, app.JoinOptions{}))
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	var u app.SettingsUpdate
	if !decode(w, r, &u) {
		return
	}
	reply(w, s.app.UpdateSettings(u))
}

func (s *Server) quit(w http.ResponseWriter, r *http.Request) {
	ok(w)
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.doneOnce.Do(func() { close(s.done) })
	}()
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err == nil && len(body) > 0 {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ok(w)
}

func ok(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// FocusExisting asks an already-running LANBridge on port to show its window.
func FocusExisting(port int) bool {
	c := http.Client{Timeout: 1500 * time.Millisecond}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/focus", port), nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-LANBridge-Focus", "1")
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		App string `json:"app"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v)
	return resp.StatusCode == http.StatusOK && v.App == "lanbridge"
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
