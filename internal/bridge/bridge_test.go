package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"lanbridge/internal/logx"
)

// fakeApp plays the part of lanbridge.sc: heartbeat, commands in, events out.
func fakeApp(t *testing.T, dir string, stop <-chan struct{}) {
	os.MkdirAll(filepath.Join(dir, "in"), 0o755)
	os.MkdirAll(filepath.Join(dir, "out"), 0o755)
	write := func(name string, v any) {
		b, _ := json.Marshal(v)
		os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	write("hello.json", map[string]any{"v": 1, "session": "s1", "world": "Test World", "players": []string{"Max"}})
	seq := 0
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			write("hello.json", map[string]any{"v": 1, "session": "s1", "world": "Test World", "players": []string{"Max"}})
			files, _ := filepath.Glob(filepath.Join(dir, "in", "*.json"))
			sort.Strings(files)
			var out []map[string]any
			for _, f := range files {
				b, _ := os.ReadFile(f)
				os.Remove(f)
				var batch struct {
					Cmds []map[string]any `json:"cmds"`
				}
				if err := json.Unmarshal(b, &batch); err != nil {
					t.Errorf("bad command file: %v", err)
					continue
				}
				for _, c := range batch.Cmds {
					if id, ok := c["id"]; ok {
						out = append(out, map[string]any{"type": "result", "id": id, "data": map[string]any{"echo": c["op"]}})
					}
					if c["op"] == "say" {
						out = append(out, map[string]any{"type": "chat", "player": "Max", "host": 1, "message": "heard " + c["text"].(string)})
					}
				}
			}
			if len(out) > 0 {
				seq++
				write(filepath.Join("out", "e_"+string(rune('a'+seq%26))+time.Now().Format("150405.000000")+".json"), map[string]any{"events": out})
			}
		}
	}()
}

func TestClientRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "saves", "w", "scripts", AppName+".data")
	stop := make(chan struct{})
	defer close(stop)
	fakeApp(t, dir, stop)
	c, err := Open(dir, logx.Discard())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.World != "Test World" {
		t.Fatalf("world %q", c.World)
	}
	var res struct {
		Echo string `json:"echo"`
	}
	if err := c.Call(context.Background(), "state", map[string]any{"name": "Bot"}, &res); err != nil || res.Echo != "state" {
		t.Fatalf("call: %v %+v", err, res)
	}
	c.Send("say", map[string]any{"name": "Bot", "text": "hello"})
	select {
	case ev := <-c.Events():
		if ev.Type != "chat" || ev.Message != "heard hello" || !bool(ev.Host) {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
	}
}

func TestStaleWorld(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.json"), []byte(`{"v":1,"world":"x"}`), 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(filepath.Join(dir, "hello.json"), old, old)
	if _, err := Open(dir, logx.Discard()); err == nil {
		t.Fatal("expected a stale-world error")
	}
}

func TestActiveAndInstall(t *testing.T) {
	game := t.TempDir()
	os.MkdirAll(filepath.Join(game, "saves", "Old", "scripts", AppName+".data"), 0o755)
	os.MkdirAll(filepath.Join(game, "mods"), 0o755)
	os.WriteFile(filepath.Join(game, "mods", "fabric-carpet-26.3-1.4.200.jar"), nil, 0o644)
	live := filepath.Join(game, "saves", "Live", "scripts", AppName+".data")
	os.MkdirAll(live, 0o755)
	os.WriteFile(filepath.Join(live, "hello.json"), []byte(`{"v":1,"world":"Live"}`), 0o644)
	dir, h, ok := Active([]string{game})
	if !ok || dir != live || h.World != "Live" {
		t.Fatalf("Active = %q %+v %v", dir, h, ok)
	}
	inst := Instances([]string{game})
	found := false
	for _, i := range inst {
		if i.Dir == filepath.Clean(game) {
			found = i.HasCarpet && !i.Installed
		}
	}
	if !found {
		t.Fatalf("instance not detected properly: %+v", inst)
	}
	p, err := Install(game)
	if err != nil || !strings.HasSuffix(filepath.ToSlash(p), "config/carpet/scripts/lanbridge.sc") {
		t.Fatalf("install: %v %s", err, p)
	}
}

// TestScriptStructure is a static check of lanbridge.sc, since Scarpet can't
// run outside Minecraft: brackets balance, every top-level statement is one
// function definition or global, and every lb_ function used is defined.
func TestScriptStructure(t *testing.T) {
	src := string(Script)
	var stack []rune
	var stmts []string
	var cur strings.Builder
	inStr, inComment := false, false
	rs := []rune(src)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case inComment:
			if r == '\n' {
				inComment = false
				cur.WriteRune(r)
			}
			continue
		case inStr:
			cur.WriteRune(r)
			if r == '\\' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			} else if r == '\'' {
				inStr = false
			}
			continue
		case r == '/' && i+1 < len(rs) && rs[i+1] == '/':
			inComment = true
			continue
		case r == '\'':
			inStr = true
		case r == '(' || r == '[' || r == '{':
			stack = append(stack, r)
		case r == ')' || r == ']' || r == '}':
			want := map[rune]rune{')': '(', ']': '[', '}': '{'}[r]
			if len(stack) == 0 || stack[len(stack)-1] != want {
				t.Fatalf("unbalanced %q near: %s", r, tail(cur.String()))
			}
			stack = stack[:len(stack)-1]
		case r == ';' && len(stack) == 0:
			stmts = append(stmts, cur.String())
			cur.Reset()
			continue
		case r == ';':
			rest := strings.TrimLeft(string(rs[i+1:]), " \t\r\n")
			if strings.HasPrefix(rest, ")") || strings.HasPrefix(rest, ",") {
				t.Fatalf("stray semicolon before %q near: %s", rest[:1], tail(cur.String()))
			}
		}
		cur.WriteRune(r)
	}
	if inStr || len(stack) != 0 {
		t.Fatalf("unterminated string or bracket (%d open)", len(stack))
	}
	if strings.TrimSpace(cur.String()) != "" {
		stmts = append(stmts, cur.String())
	}
	def := regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s*\([a-z_, ]*\)\s*->`)
	glob := regexp.MustCompile(`^\s*global_[a-z_]+\s*=`)
	defined := map[string]bool{}
	for _, s := range stmts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if m := def.FindStringSubmatch(s); m != nil {
			if strings.Count(s, "->") < 1 {
				t.Fatalf("bad definition: %s", tail(s))
			}
			defined[m[1]] = true
			continue
		}
		if !glob.MatchString(s) {
			t.Fatalf("unexpected top-level statement: %s", tail(s))
		}
	}
	for _, m := range regexp.MustCompile(`\b(lb_[a-z_]+)\s*\(`).FindAllStringSubmatch(src, -1) {
		if !defined[m[1]] {
			t.Fatalf("%s is called but never defined", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`schedule\(\s*\d+\s*,\s*'([a-z_]+)'`).FindAllStringSubmatch(src, -1) {
		if !defined[m[1]] {
			t.Fatalf("scheduled %s is never defined", m[1])
		}
	}
	for _, ev := range []string{"__on_player_message", "__on_player_connects", "__on_player_disconnects", "__on_player_dies", "__on_start", "__config"} {
		if !defined[ev] {
			t.Fatalf("missing %s", ev)
		}
	}
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		return "…" + s[len(s)-160:]
	}
	return s
}
