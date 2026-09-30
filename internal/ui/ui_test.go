package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"lanbridge/internal/app"
	"lanbridge/internal/logx"
)

func TestAPIAuthAndHostGuard(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	a, err := app.New(logx.Discard())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(a, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := strings.SplitN(s.URL(), "/#", 2)[0]
	get := func(path, token, host string) *http.Response {
		req, _ := http.NewRequest("GET", base+path, nil)
		if token != "" {
			req.Header.Set("X-LANBridge-Token", token)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := get("/api/state", "", ""); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r := get("/api/state", s.token, "evil.example:80"); r.StatusCode != 403 {
		t.Fatalf("foreign Host header: %d", r.StatusCode)
	}
	r := get("/api/state", s.token, "")
	if r.StatusCode != 200 {
		t.Fatalf("state: %d", r.StatusCode)
	}
	var st app.State
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil || st.Mode != "idle" {
		t.Fatalf("state %+v %v", st, err)
	}
	page := get("/", "", "")
	body, _ := io.ReadAll(page.Body)
	if page.StatusCode != 200 || !strings.Contains(string(body), "LANBridge") || page.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("index: %d", page.StatusCode)
	}
}
