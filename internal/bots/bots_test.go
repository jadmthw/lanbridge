package bots

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lanbridge/internal/ai"
	"lanbridge/internal/bridge"
	"lanbridge/internal/logx"
)

func TestParseReply(t *testing.T) {
	cases := []struct {
		in      string
		say     string
		actions []string
	}{
		{`{"say":"On it!","actions":["follow Max"]}`, "On it!", []string{"follow Max"}},
		{"```json\n{\"say\": \"ok\", \"actions\": []}\n```", "ok", nil},
		{`Sure! {"say":"coming","actions":[{"do":"collect","block":"oak_log","count":4}]}`, "coming", []string{"collect oak_log 4"}},
		{`just some words`, "just some words", nil},
	}
	for _, c := range cases {
		r := parseReply(c.in)
		if r.Say != c.say || strings.Join(r.Actions, "|") != strings.Join(c.actions, "|") {
			t.Errorf("parseReply(%q) = %+v", c.in, r)
		}
	}
}

func TestParseAction(t *testing.T) {
	players := []string{"Max", "Sam"}
	for in, want := range map[string]string{
		"follow max":              "following Max",
		"follow":                  "following Sam", // defaults to whoever asked
		"collect oak_log 8":       "collecting oak log (0/8)",
		"mine logs 3":             "collecting logs (0/3)",
		"collect iron ore 2":      "collecting iron ore (0/2)",
		"attack zombies":          "fighting zombie",
		"give Max cobblestone 16": "bringing cobblestone to Max",
		"goto 10 64 -20":          "walking to 10 64 -20",
		"come Max":                "coming to Max",
		"eat":                     "eating",
	} {
		tk, err := parseAction(in, "Sam", players)
		if err != nil || tk.desc() != want {
			t.Errorf("parseAction(%q) = %v, %v; want %q", in, tk, err, want)
		}
	}
	if _, err := parseAction("summon a dragon", "Sam", players); err == nil {
		t.Error("expected an error for an unknown action")
	}
}

func TestBlockPattern(t *testing.T) {
	pat, _ := blockPattern("wood")
	for _, ok := range []string{"oak_log", "cherry_log", "pale_oak_log"} {
		if !strings.Contains(pat, strings.TrimSuffix(ok, "_log")) {
			t.Errorf("wood pattern %s misses %s", pat, ok)
		}
	}
	if p, _ := blockPattern("diamonds"); p != "^(deepslate_)?diamond_ore$" {
		t.Errorf("diamonds -> %s", p)
	}
	if p, _ := blockPattern("minecraft:Oak Log"); p != "^oak_log$" {
		t.Errorf("oak log -> %s", p)
	}
}

func TestChatLines(t *testing.T) {
	got := chatLines("  §4Hello\nthere\u0007 " + strings.Repeat("word ", 80))
	if len(got) != 2 || strings.ContainsAny(got[0], "§\n\u0007") || len([]rune(got[0])) > 220 {
		t.Fatalf("chatLines = %q", got)
	}
}

// fakeWorld stands in for Minecraft + Carpet: it answers bridge commands and
// records everything the AI players do.
type fakeWorld struct {
	m    *Manager
	mu   sync.Mutex
	sent []string
	bots map[string]bool
}

func (w *fakeWorld) record(s string) {
	w.mu.Lock()
	w.sent = append(w.sent, s)
	w.mu.Unlock()
}

func (w *fakeWorld) has(prefix string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.sent {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func (w *fakeWorld) Send(op string, args map[string]any) {
	switch op {
	case "act":
		w.record(fmt.Sprintf("act %s %s", args["name"], args["action"]))
	case "say":
		w.record(fmt.Sprintf("say <%s> %s", args["name"], args["text"]))
	case "tell":
		w.record(fmt.Sprintf("tell %v %s", args["to"], args["text"]))
	case "tp":
		w.record(fmt.Sprintf("tp %s %s", args["name"], args["to"]))
	}
}

func (w *fakeWorld) Call(ctx context.Context, op string, args map[string]any, out any) error {
	var data any = map[string]any{"ok": true}
	switch op {
	case "spawn":
		name := args["name"].(string)
		w.mu.Lock()
		w.bots[name] = true
		w.mu.Unlock()
		w.record("spawn " + name)
		go w.m.handle(bridge.Event{Type: "join", Player: name, Fake: true})
	case "remove":
		name := args["name"].(string)
		w.record("remove " + name)
		go w.m.handle(bridge.Event{Type: "leave", Player: name, Fake: true})
	case "state":
		data = map[string]any{"online": true, "x": 0.5, "y": 64, "z": 0.5, "health": 20, "food": 20, "dim": "overworld",
			"inv": map[string]any{"oak_log": 5}, "players": []any{map[string]any{"name": "Max", "x": 10.5, "y": 64, "z": 0.5, "dim": "overworld", "host": true}},
			"near": []any{}, "daytime": 1000}
	}
	b, _ := json.Marshal(data)
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func TestEndToEnd(t *testing.T) {
	var calls int
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		mu.Unlock()
		if !strings.Contains(string(body), "can you follow me") {
			t.Errorf("prompt is missing the player's message: %s", body)
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"{\"say\":\"Sure thing, Max!\",\"actions\":[\"follow Max\"]}"}],"usage":{"input_tokens":100,"output_tokens":20}}`)
	}))
	defer api.Close()
	old := ai.BaseURLs[ai.Anthropic]
	ai.BaseURLs[ai.Anthropic] = api.URL
	defer func() { ai.BaseURLs[ai.Anthropic] = old }()

	settings := DefaultSettings()
	settings.Providers = map[ai.Kind]ai.Config{ai.Anthropic: {Kind: ai.Anthropic, Auth: ai.AuthAPIKey, APIKey: "k", Model: "claude-test"}}
	m := &Manager{log: logx.Discard(), settings: func() Settings { return settings }, bots: map[string]*Bot{}, sticky: map[string]sticky{}, tests: map[ai.Kind]string{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	defer m.cancel()
	w := &fakeWorld{m: m, bots: map[string]bool{}}
	m.attach(w, bridge.Hello{World: "Test", Players: []string{"Max", "Sam"}})

	// Friends can't add AI players when only the host may.
	m.onChat("Sam", false, "!ai spawn claude")
	waitFor(t, func() bool { return w.has("tell Sam [LANBridge] Only the host") }, "host-only refusal")

	m.onChat("Max", true, "!ai spawn claude")
	waitFor(t, func() bool { return m.bot("Claude_1") != nil && m.bot("Claude_1").status().Online }, "Claude_1 to join")

	m.onChat("Max", true, "hey claude can you follow me")
	waitFor(t, func() bool { return w.has("say <Claude_1> Sure thing, Max!") }, "the reply in chat")
	waitFor(t, func() bool { return w.has("act Claude_1 move forward") && w.has("act Claude_1 look at 10.50") }, "walking toward Max")
	if s := m.bot("Claude_1").status(); s.Task != "following Max" || s.TokensIn != 100 {
		t.Fatalf("status %+v", s)
	}
	// Chat between people that doesn't mention the AI isn't sent to the model.
	m.mu.Lock()
	m.sticky = map[string]sticky{}
	m.mu.Unlock()
	m.onChat("Sam", false, "brb getting food")
	time.Sleep(900 * time.Millisecond)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 model call, got %d", n)
	}
	if err := m.Remove("claude_1"); err != nil || m.bot("Claude_1") != nil || !w.has("remove Claude_1") {
		t.Fatalf("remove: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
