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

func (w *fakeWorld) log() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.sent...)
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
			"inv": map[string]any{"oak_log": 5}, "players": []any{
				map[string]any{"name": "Max", "x": 10.5, "y": 64, "z": 0.5, "yaw": 0, "dim": "overworld", "host": true},
				map[string]any{"name": "Sam", "x": 30.5, "y": 64, "z": 0.5, "yaw": 180, "dim": "overworld"}},
			"near": []any{}, "daytime": 1000}
	case "run":
		var results []any
		for _, c := range args["commands"].([]string) {
			w.record(fmt.Sprintf("run as_bot=%v %s", args["as_bot"], c))
			if strings.Contains(c, "BADBLOCK") {
				results = append(results, map[string]any{"ok": false, "error": "Unknown block type 'minecraft:badblock'"})
			} else {
				results = append(results, map[string]any{"ok": true})
			}
		}
		data = map[string]any{"ok": true, "results": results}
	case "terrain":
		w.record(fmt.Sprintf("terrain %v %v %v", args["x"], args["z"], args["dim"]))
		var cells []any
		for i := 0; i < 33*33; i++ {
			if i%33 > 28 {
				cells = append(cells, []any{62, "water"})
			} else {
				cells = append(cells, []any{63, "grass_block"})
			}
		}
		data = cells
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
	settings.ReplyAll = false
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

func TestCheckCommand(t *testing.T) {
	for _, ok := range []string{"time set day", "/give Max diamond 3", "execute as Max run tp @s ~ ~10 ~", "minecraft:weather clear"} {
		if _, err := checkCommand(ok); err != nil {
			t.Errorf("%q should be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{"op Max", "/stop", "minecraft:ban Sam", "execute as @a run op Max", "execute at Max run execute as @s run deop Max",
		"execute if function foo:bar run say hi", "script run print(1)", "player Bot kill", "carpet commandPlayer true", "say hi\nop Max"} {
		if _, err := checkCommand(bad); err == nil {
			t.Errorf("%q should be blocked", bad)
		}
	}
	if _, err := checkBuild("give Max diamond 1"); err == nil {
		t.Error("builds must only use fill/setblock/clone")
	}
	if _, err := checkBuild("/fill ~0 ~0 ~0 ~4 ~3 ~4 oak_planks hollow"); err != nil {
		t.Error(err)
	}
}

func TestBuildSpot(t *testing.T) {
	for yaw, want := range map[float64]string{0: "south", 180: "north", -180: "north", 90: "west", -90: "east", 270: "east", 359: "south"} {
		if got, _, _ := facing(yaw); got != want {
			t.Errorf("facing(%v) = %s, want %s", yaw, got, want)
		}
	}
	st := &State{X: 0.5, Y: 64, Z: 0.5, Dim: "overworld", Players: []PlayerPos{{Name: "Max", X: 10.5, Y: 70, Z: 0.5, Yaw: 180, Dim: "overworld"}}}
	if x, y, z, _, err := buildSpot("", "Max", st); err != nil || x != 10 || y != 70 || z != -4 {
		t.Fatalf("buildSpot = %d %d %d %v", x, y, z, err)
	}
	if x, y, z, _, _ := buildSpot("100, 65, -20", "Max", st); x != 100 || y != 65 || z != -20 {
		t.Fatalf("explicit spot = %d %d %d", x, y, z)
	}
}

func TestCommandsBuildsAndReplyAll(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b := string(body)
		mu.Lock()
		prompts = append(prompts, b)
		mu.Unlock()
		reply := `{"say":"hm?","actions":[],"commands":[],"build":"","build_at":""}`
		switch {
		case strings.Contains(b, "These commands failed"):
			reply = `{"title":"Cozy hut","commands":["setblock ~2 ~0 ~0 oak_door[facing=north,half=lower]"]}`
		case strings.Contains(b, "SITE SURVEY"):
			reply = `{"title":"Cozy hut","commands":["fill ~-2 ~0 ~-2 ~2 ~4 ~2 air","fill ~-2 ~0 ~-2 ~2 ~3 ~2 spruce_planks hollow","setblock ~0 ~0 ~-2 BADBLOCK","give Max tnt 64"]}`
		case strings.Contains(b, "build me a hut"):
			reply = `{"say":"On it!","actions":[],"commands":["time set day","op Max"],"build":"a cozy 5x5 spruce hut with a door and a lantern","build_at":""}`
		case strings.Contains(b, "give me diamonds"):
			reply = `{"say":"Here!","actions":[],"commands":["give Sam diamond 64"],"build":"","build_at":""}`
		}
		out, _ := json.Marshal(reply)
		io.WriteString(w, `{"content":[{"type":"text","text":`+string(out)+`}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer api.Close()
	old := ai.BaseURLs[ai.Anthropic]
	ai.BaseURLs[ai.Anthropic] = api.URL
	defer func() { ai.BaseURLs[ai.Anthropic] = old }()

	settings := DefaultSettings()
	settings.Providers = map[ai.Kind]ai.Config{ai.Anthropic: {Kind: ai.Anthropic, Auth: ai.AuthAPIKey, APIKey: "k", Model: "m"}}
	m := &Manager{log: logx.Discard(), settings: func() Settings { return settings }, bots: map[string]*Bot{}, sticky: map[string]sticky{}, tests: map[ai.Kind]string{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	defer m.cancel()
	w := &fakeWorld{m: m, bots: map[string]bool{}}
	m.attach(w, bridge.Hello{World: "Test", Players: []string{"Max", "Sam"}})
	if _, err := m.Spawn(SpawnRequest{Kind: ai.Anthropic}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		b := m.bot("Claude_1")
		if b == nil {
			return false
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.state != nil
	}, "first state")

	// The host asks for a build: the site is surveyed, designed, placed, and
	// the step the game rejected is sent back for one repair pass.
	m.onChat("Max", true, "claude build me a hut")
	waitFor(t, func() bool { return w.has("say <Claude_1> Built Cozy hut") || w.has("say <Claude_1> Done!") }, "the build")
	const spot = "run as_bot=false execute in minecraft:overworld positioned 10 64 4 run "
	for _, want := range []string{"terrain 10 4 overworld", spot + "fill ~-2 ~0 ~-2 ~2 ~3 ~2 spruce_planks hollow", spot + "setblock ~2 ~0 ~0 oak_door[facing=north,half=lower]"} {
		if !w.has(want) {
			t.Fatalf("missing %q in %v", want, w.log())
		}
	}
	if w.has(spot+"give") || w.has("run as_bot=true op Max") || !w.has("run as_bot=true time set day") {
		t.Fatalf("command filtering wrong: %v", w.log())
	}
	mu.Lock()
	var survey string
	for _, p := range prompts {
		if strings.Contains(p, "SITE SURVEY") && !strings.Contains(p, "These commands failed") {
			survey = p
		}
	}
	mu.Unlock()
	if !strings.Contains(survey, "cozy 5x5 spruce hut") || !strings.Contains(survey, "facing south") || !strings.Contains(survey, "wwww") {
		t.Fatalf("architect prompt is missing the request, facing or terrain: %.400s", survey)
	}

	// A guest who doesn't name the AI still gets an answer, but can't use commands.
	m.onChat("Sam", false, "can you give me diamonds")
	waitFor(t, func() bool { return w.has("say <Claude_1> Sorry Sam, I'm not allowed to run commands for you.") }, "the refusal")
	if w.has("run as_bot=true give Sam diamond 64") {
		t.Fatal("a guest got a command run")
	}
}

func TestOffensiveNames(t *testing.T) {
	for _, bad := range []string{"N1gga", "niiigga_bot", "NIGGER", "xx_fag_xx", "Kike", "coon_2", "Spic"} {
		if !offensiveName(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
	for _, ok := range []string{"sama", "Knight", "Raccoon", "Spicy_1", "Nigel", "Cocoon", "Pakistan_fan", "Claude_1", "GPT_2"} {
		if offensiveName(ok) {
			t.Errorf("%q shouldn't be refused", ok)
		}
	}
	if _, err := checkBuild("summon text_display ~ ~2 ~ {text:'\"12:00\"'}"); err != nil {
		t.Errorf("text_display should be allowed in builds: %v", err)
	}
	if _, err := checkBuild("summon tnt ~ ~ ~"); err == nil {
		t.Error("builds must not summon tnt")
	}
}

func TestBusyBuildingAndStop(t *testing.T) {
	var mu sync.Mutex
	var busyPrompt bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b := string(body)
		reply := `{"say":"hm","actions":[],"commands":[],"build":"","build_at":""}`
		switch {
		case strings.Contains(b, "SITE SURVEY"):
			<-r.Context().Done() // a slow design that gets cancelled
			return
		case strings.Contains(b, "stop the build"):
			reply = `{"say":"Okay!","actions":["stop"],"commands":[],"build":"","build_at":""}`
		case strings.Contains(b, "what are you doing"):
			mu.Lock()
			busyPrompt = strings.Contains(b, "YOU ARE BUSY BUILDING") && strings.Contains(b, "redstone timer")
			mu.Unlock()
			reply = `{"say":"Still designing that timer!","actions":[],"commands":[],"build":"","build_at":""}`
		case strings.Contains(b, "make a timer"):
			reply = `{"say":"On it!","actions":[],"commands":[],"build":"a redstone timer with a lamp display","build_at":""}`
		}
		out, _ := json.Marshal(reply)
		io.WriteString(w, `{"content":[{"type":"text","text":`+string(out)+`}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer api.Close()
	old := ai.BaseURLs[ai.Anthropic]
	ai.BaseURLs[ai.Anthropic] = api.URL
	defer func() { ai.BaseURLs[ai.Anthropic] = old }()

	settings := DefaultSettings()
	settings.Providers = map[ai.Kind]ai.Config{ai.Anthropic: {Kind: ai.Anthropic, Auth: ai.AuthAPIKey, APIKey: "k", Model: "m"}}
	m := &Manager{log: logx.Discard(), settings: func() Settings { return settings }, bots: map[string]*Bot{}, sticky: map[string]sticky{}, tests: map[ai.Kind]string{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	defer m.cancel()
	w := &fakeWorld{m: m, bots: map[string]bool{}}
	m.attach(w, bridge.Hello{World: "Test", Players: []string{"Max", "Sam"}})
	if _, err := m.Spawn(SpawnRequest{Kind: ai.Anthropic}); err != nil {
		t.Fatal(err)
	}
	b := m.bot("Claude_1")
	waitFor(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.state != nil }, "first state")

	m.onChat("Max", true, "claude make a timer")
	waitFor(t, func() bool { return b.status().Task != "" && strings.Contains(b.status().Task, "designing") }, "designing")
	m.onChat("Max", true, "claude what are you doing")
	waitFor(t, func() bool { return w.has("say <Claude_1> Still designing that timer!") }, "the busy answer")
	mu.Lock()
	ok := busyPrompt
	mu.Unlock()
	if !ok {
		t.Fatal("the chat prompt didn't say the AI player is busy building")
	}
	m.onChat("Max", true, "claude stop the build")
	waitFor(t, func() bool { return w.has("say <Claude_1> Okay, I stopped the build.") }, "the build to stop")
	waitFor(t, func() bool { return b.status().Task == "" }, "the task to clear")
}
