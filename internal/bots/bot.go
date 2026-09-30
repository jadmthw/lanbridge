package bots

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"lanbridge/internal/ai"
)

type line struct {
	at   time.Time
	from string
	text string
	host bool
}

// Bot is one AI player.
type Bot struct {
	m        *Manager
	Name     string
	Kind     ai.Kind
	Persona  string
	Mode     string
	provider ai.Provider
	created  time.Time
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	joined   chan struct{}
	joinOnce sync.Once

	mu        sync.Mutex
	online    bool
	removing  bool
	state     *State
	task      task
	queue     []task
	history   []line
	inbox     []line
	thinking  bool
	lastSaid  string
	calls     int
	tokensIn  int
	tokensOut int
	lastErr   string
	callTimes []time.Time
	walking   bool
	sprinting bool
	building  bool
	stage     string
	buildDesc string
	buildFor  string
	buildAt   time.Time
	cancelB   context.CancelFunc
	buildLog  []string
	lastLook  time.Time
	limitNote time.Time
}

func newBot(m *Manager, name string, kind ai.Kind, persona, mode string, p ai.Provider) *Bot {
	ctx, cancel := context.WithCancel(m.ctx)
	return &Bot{m: m, Name: name, Kind: kind, Persona: strings.TrimSpace(persona), Mode: mode, provider: p,
		created: time.Now(), ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), joined: make(chan struct{})}
}

func (b *Bot) start() {
	go b.mind()
	go b.body()
}

func (b *Bot) stop() { b.cancel() }

func (b *Bot) markJoined() {
	b.mu.Lock()
	b.online = true
	b.mu.Unlock()
	b.joinOnce.Do(func() { close(b.joined) })
}

// ---- body: the Carpet fake player ----

func (b *Bot) name() string { return b.Name }

func (b *Bot) act(a string) {
	b.m.send("act", map[string]any{"name": b.Name, "action": a})
}

func (b *Bot) lookAt(x, y, z float64) {
	b.act(fmt.Sprintf("look at %.2f %.2f %.2f", x, y, z))
}

func (b *Bot) walk(on bool) {
	b.mu.Lock()
	changed := b.walking != on
	b.walking = on
	b.mu.Unlock()
	if changed {
		if on {
			b.act("move forward")
		} else {
			b.act("move")
		}
	}
}

func (b *Bot) sprint(on bool) {
	b.mu.Lock()
	changed := b.sprinting != on
	b.sprinting = on
	b.mu.Unlock()
	if changed {
		if on {
			b.act("sprint")
		} else {
			b.act("unsprint")
		}
	}
}

func (b *Bot) jump() { b.act("jump") }

func (b *Bot) stopAll() {
	b.mu.Lock()
	b.walking, b.sprinting = false, false
	b.mu.Unlock()
	b.act("stop")
	b.act("unsprint")
}

func (b *Bot) tpTo(player string) {
	b.m.send("tp", map[string]any{"name": b.Name, "to": player})
}

func (b *Bot) call(ctx context.Context, op string, args map[string]any, out any) error {
	return b.m.call(ctx, op, args, out)
}

// say posts a chat message as this AI player.
func (b *Bot) say(text string) {
	if b.ctx.Err() != nil {
		return // removed from the world; stay quiet
	}
	for _, part := range chatLines(text) {
		b.m.send("say", map[string]any{"name": b.Name, "text": part})
		b.m.heard(b.Name, part, true)
		b.mu.Lock()
		b.lastSaid = part
		b.mu.Unlock()
	}
}

// chatLines cleans model output into at most two chat-sized lines.
func chatLines(s string) []string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '§' || r == '\u200b':
			return -1
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(strings.Trim(s, `"' `)), " ")
	var out []string
	for s != "" && len(out) < 2 {
		r := []rune(s)
		if len(r) <= 220 {
			out = append(out, s)
			break
		}
		cut := 220
		for i := 220; i > 120; i-- {
			if r[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		s = strings.TrimSpace(string(r[cut:]))
	}
	return out
}

// note adds a line only this AI player sees in its chat memory.
func (b *Bot) note(text string) {
	b.mu.Lock()
	b.history = append(b.history, line{at: time.Now(), from: b.Name, text: text})
	if len(b.history) > 24 {
		b.history = b.history[len(b.history)-24:]
	}
	b.mu.Unlock()
}

func (b *Bot) setTask(t task) {
	b.mu.Lock()
	b.task, b.queue = t, nil
	b.mu.Unlock()
	b.stopAll()
}

func (b *Bot) enqueue(t task) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.task == nil {
		b.task = t
		return
	}
	if len(b.queue) < 5 {
		b.queue = append(b.queue, t)
	}
}

// StopTask cancels whatever the bot is doing, including a build.
func (b *Bot) StopTask() {
	b.mu.Lock()
	b.task, b.queue = nil, nil
	cancel := b.cancelB
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	b.stopAll()
}

func (b *Bot) body() {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		b.mu.Lock()
		online := b.online
		b.mu.Unlock()
		if !online {
			continue
		}
		st := &State{}
		if err := b.m.call(b.ctx, "state", map[string]any{"name": b.Name, "radius": 20}, st); err != nil {
			continue
		}
		st.at = time.Now()
		if !st.Online {
			continue // it may be respawning; the manager handles leave events
		}
		b.mu.Lock()
		b.state = st
		tk := b.task
		b.mu.Unlock()
		if tk == nil {
			b.idle(st)
			continue
		}
		if done, msg := tk.step(b.ctx, b, st); done {
			b.mu.Lock()
			if b.task == tk {
				b.task = nil
				if len(b.queue) > 0 {
					b.task, b.queue = b.queue[0], b.queue[1:]
				}
			}
			b.mu.Unlock()
			b.walk(false)
			b.sprint(false)
			if msg != "" {
				b.say(msg)
			}
		}
	}
}

// idle keeps an unbusy AI player looking alive, and fed.
func (b *Bot) idle(st *State) {
	if st.Food <= 6 && st.Mode != "creative" {
		for _, f := range foods {
			if st.count(f) > 0 {
				b.enqueue(&eatTask{})
				return
			}
		}
	}
	if time.Since(b.lastLook) < 3*time.Second {
		return
	}
	b.lastLook = time.Now()
	var best *PlayerPos
	bestD := 12.0
	for i := range st.Players {
		p := &st.Players[i]
		if d := st.dist(p.X, p.Y, p.Z); p.Dim == st.Dim && d < bestD {
			best, bestD = p, d
		}
	}
	if best != nil {
		b.lookAt(best.X, best.Y+1.62, best.Z)
	}
}

// ---- mind: the model ----

func (b *Bot) hear(l line, addressed bool) {
	b.mu.Lock()
	b.history = append(b.history, l)
	if len(b.history) > 24 {
		b.history = b.history[len(b.history)-24:]
	}
	if addressed {
		b.inbox = append(b.inbox, l)
		if len(b.inbox) > 6 {
			b.inbox = b.inbox[len(b.inbox)-6:]
		}
	}
	b.mu.Unlock()
	if addressed {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
}

func (b *Bot) allowCall(max int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	kept := b.callTimes[:0]
	for _, t := range b.callTimes {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	b.callTimes = kept
	if max > 0 && len(kept) >= max {
		return false
	}
	b.callTimes = append(b.callTimes, time.Now())
	return true
}

func (b *Bot) mind() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-b.wake:
		}
		select { // give people a moment to finish typing follow-ups
		case <-b.ctx.Done():
			return
		case <-time.After(600 * time.Millisecond):
		}
		b.mu.Lock()
		msgs := b.inbox
		b.inbox = nil
		b.mu.Unlock()
		if len(msgs) == 0 {
			continue
		}
		from := msgs[len(msgs)-1].from
		if !b.allowCall(b.m.settings().MaxPerMin) {
			b.mu.Lock()
			note := time.Since(b.limitNote) > time.Minute
			if note {
				b.limitNote = time.Now()
			}
			b.mu.Unlock()
			if note {
				b.m.tell(from, fmt.Sprintf("[LANBridge] %s hit its reply limit for this minute (change it in LANBridge > AI players).", b.Name))
			}
			continue
		}
		b.mu.Lock()
		b.thinking = true
		b.mu.Unlock()
		ctx, cancel := context.WithTimeout(b.ctx, 4*time.Minute)
		resp, err := b.provider.Generate(ctx, ai.Request{System: b.systemPrompt(), Prompt: b.prompt(msgs), MaxTokens: 1500})
		cancel()
		b.mu.Lock()
		b.thinking = false
		b.calls++
		if err != nil {
			b.lastErr = err.Error()
		} else {
			b.lastErr = ""
			b.tokensIn += resp.InputTokens
			b.tokensOut += resp.OutputTokens + resp.TotalTokens
		}
		b.mu.Unlock()
		if b.ctx.Err() != nil {
			return
		}
		if err != nil {
			b.m.log.Warnf("AI player %s: %v", b.Name, err)
			b.m.tell(from, fmt.Sprintf("[LANBridge] %s couldn't answer: %s", b.Name, shorten(err.Error(), 160)))
			continue
		}
		rep := parseReply(resp.Text)
		if rep.Say != "" {
			b.say(rep.Say)
		}
		host := msgs[len(msgs)-1].host
		b.doCommands(b.ctx, rep.Commands, from, host)
		b.build(rep.Build, rep.BuildCmds, rep.BuildAt, from, msgs[len(msgs)-1].text, host)
		for _, a := range rep.Actions {
			t, err := parseAction(a, from, b.m.playerNames())
			if err != nil {
				b.m.log.Debugf("AI player %s: skipped action %q: %v", b.Name, a, err)
				continue
			}
			if st, ok := t.(*simpleTask); ok && st.action == "stop" {
				b.StopTask()
				continue
			}
			if _, ok := t.(*followTask); ok {
				b.setTask(t)
				continue
			}
			b.enqueue(t)
		}
	}
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (b *Bot) systemPrompt() string {
	info, _ := ai.InfoFor(b.Kind)
	persona := b.Persona
	if persona == "" {
		persona = "Friendly, helpful and a little playful, like a good teammate."
	}
	return fmt.Sprintf(`You are %s, a player in a Minecraft Java Edition world. You're an AI (%s, made by %s) connected through LANBridge, and real people talk to you in the game chat.
Personality: %s

How to reply:
- Write like a Minecraft player in chat: short and casual, at most 2 sentences, no markdown.
- Keep it friendly and fine for all ages.
- Only real people talk to you; other AI players' messages are shown for context, don't answer them.
- If a message clearly isn't meant for you, you can stay quiet: empty "say" and nothing else.
- Don't claim you did something unless you include the action, command or build for it.

Actions you can take with your body (strings in "actions"):
  "follow <player>"               walk after a player until told to stop
  "come <player>"                 teleport next to a player right away
  "goto <x> <y> <z>"              walk to coordinates
  "stop"                          stop what you're doing
  "collect <block> <count>"       mine blocks nearby, e.g. "collect oak_log 8"; "log" means any log
  "attack <mob>"                  fight the nearest mob of that type; "attack hostile" for any monster
  "give <player> <item> <count>"  toss items from your inventory
  "equip <item>", "eat", "look <player>", "jump", "sneak", "unsneak"

Commands ("commands"): vanilla Minecraft commands without the slash, run as you, so @s and ~ ~ ~ mean you.
  Examples: "time set day", "weather clear", "give Max diamond 3", "effect give Max minecraft:speed 60 1", "summon minecraft:cat ~ ~ ~".
  Only use them when the person asking is allowed to (see "Permissions" in the message).

Building ("build"): when someone asks you to build something, put a detailed description of it in "build": what it is, style, rough size in blocks, materials and colors, rooms and features. Fill in sensible details they didn't mention. An architect step then surveys the ground and designs it for you, so don't write commands for builds. Keep "say" short, like "On it, give me a minute!"; you'll announce it when it's done.
  "build_at": "" builds in front of the person asking; a player name builds in front of them; "x y z" builds at those coordinates.

Answer with only this JSON object and nothing else:
{"say": "<chat message, or empty>", "actions": [], "commands": [], "build": "", "build_at": ""}`, b.Name, info.Name, info.Vendor, persona)
}

func (b *Bot) prompt(msgs []line) string {
	b.mu.Lock()
	st, tk, hist := b.state, b.task, append([]line(nil), b.history...)
	queued := len(b.queue)
	b.mu.Unlock()
	var sb strings.Builder
	if st != nil {
		sb.WriteString(st.Summary(b.Name))
	} else {
		sb.WriteString("You just joined the world.")
	}
	b.mu.Lock()
	building, desc, forWho, stage, since := b.building, b.buildDesc, b.buildFor, b.stage, b.buildAt
	b.mu.Unlock()
	if building {
		fmt.Fprintf(&sb, "\nYOU ARE BUSY BUILDING: %q for %s (%s, started %s ago). You can't start another build until it's finished, so leave \"build\" empty. If someone asks what you're doing or about the build, tell them how it's going. If they want you to stop or cancel it, add the action \"stop\".",
			desc, forWho, stage, time.Since(since).Round(time.Second))
	}
	sb.WriteString("\nCurrent task: ")
	if tk != nil {
		sb.WriteString(tk.desc())
		if queued > 0 {
			sb.WriteString(fmt.Sprintf(" (+%d more queued)", queued))
		}
	} else {
		sb.WriteString("nothing")
	}
	if others := b.m.otherBots(b.Name); len(others) > 0 {
		sb.WriteString("\nOther AI players here: " + strings.Join(others, ", "))
	}
	sb.WriteString("\n\nRecent chat, oldest first:\n")
	newest := map[time.Time]bool{}
	for _, m := range msgs {
		newest[m.at] = true
	}
	for _, l := range hist {
		if !newest[l.at] {
			sb.WriteString("<" + l.from + "> " + l.text + "\n")
		}
	}
	sb.WriteString(b.permissions(msgs[len(msgs)-1], st))
	sb.WriteString("\nNew message(s) for you:\n")
	for _, m := range msgs {
		sb.WriteString("<" + m.from + "> " + m.text + "\n")
	}
	return sb.String()
}

// permissions tells the model what the person asking may request.
func (b *Bot) permissions(last line, st *State) string {
	set := b.m.settings()
	can := func(ok bool) string {
		if ok {
			return "may"
		}
		return "may NOT"
	}
	role := "a guest"
	if last.host {
		role = "the host"
	}
	s := fmt.Sprintf("\nPermissions: %s is %s. They %s ask you to run commands, and %s ask you to build.",
		last.from, role, can(permitted(set.Commands, last.host)), can(permitted(set.Builds, last.host)))
	if st != nil {
		if p := st.player(last.from); p != nil {
			dir, _, _ := facing(p.Yaw)
			s += fmt.Sprintf(" %s is at x=%d, y=%d, z=%d, facing %s.", p.Name, int(math.Floor(p.X)), int(math.Floor(p.Y)), int(math.Floor(p.Z)), dir)
		}
	}
	return s + "\n"
}

type reply struct {
	Say       string
	Actions   []string
	Commands  []string
	Build     string   // what to build, for the architect
	BuildCmds []string // ready-made build commands (older answer format)
	BuildAt   string
}

// parseReply reads the model's JSON answer, tolerating code fences, extra
// prose, and actions written as objects instead of strings.
func parseReply(s string) reply {
	s = strings.TrimSpace(s)
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		var raw struct {
			Say      string            `json:"say"`
			Actions  []json.RawMessage `json:"actions"`
			Commands []string          `json:"commands"`
			Build    json.RawMessage   `json:"build"`
			BuildAt  string            `json:"build_at"`
		}
		if json.Unmarshal([]byte(s[i:j+1]), &raw) == nil {
			r := reply{Say: strings.TrimSpace(raw.Say), BuildAt: raw.BuildAt}
			for _, c := range raw.Commands {
				if c = strings.TrimSpace(c); c != "" {
					r.Commands = append(r.Commands, c)
				}
			}
			var desc string
			var cmds []string
			if json.Unmarshal(raw.Build, &desc) == nil {
				r.Build = strings.TrimSpace(desc)
			} else if json.Unmarshal(raw.Build, &cmds) == nil {
				for _, c := range cmds {
					if c = strings.TrimSpace(c); c != "" {
						r.BuildCmds = append(r.BuildCmds, c)
					}
				}
			}
			for _, a := range raw.Actions {
				var str string
				if json.Unmarshal(a, &str) == nil {
					if str = strings.TrimSpace(str); str != "" {
						r.Actions = append(r.Actions, str)
					}
					continue
				}
				var obj map[string]any
				if json.Unmarshal(a, &obj) == nil {
					if act := objectAction(obj); act != "" {
						r.Actions = append(r.Actions, act)
					}
				}
			}
			if len(r.Actions) > 6 {
				r.Actions = r.Actions[:6]
			}
			return r
		}
	}
	s = strings.TrimSpace(strings.Trim(s, "`"))
	s = strings.TrimPrefix(s, "json")
	return reply{Say: shorten(strings.TrimSpace(s), 220)}
}

func objectAction(o map[string]any) string {
	verb := ""
	for _, k := range []string{"do", "action", "type", "name"} {
		if v, ok := o[k].(string); ok && v != "" {
			verb = v
			break
		}
	}
	if verb == "" {
		return ""
	}
	parts := []string{verb}
	for _, k := range []string{"player", "target", "block", "item", "mob", "x", "y", "z", "count"} {
		switch v := o[k].(type) {
		case string:
			if v != "" {
				parts = append(parts, v)
			}
		case float64:
			parts = append(parts, fmt.Sprint(math.Round(v*10)/10))
		}
	}
	return strings.Join(parts, " ")
}

// BotStatus is shown in the control panel.
type BotStatus struct {
	Name      string   `json:"name"`
	Kind      ai.Kind  `json:"kind"`
	Model     string   `json:"model"`
	Persona   string   `json:"persona,omitempty"`
	Online    bool     `json:"online"`
	Task      string   `json:"task"`
	Thinking  bool     `json:"thinking"`
	LastSaid  string   `json:"lastSaid,omitempty"`
	Calls     int      `json:"calls"`
	TokensIn  int      `json:"tokensIn"`
	TokensOut int      `json:"tokensOut"`
	LastError string   `json:"lastError,omitempty"`
	BuildLog  []string `json:"buildLog,omitempty"`
	Health    float64  `json:"health"`
	Food      float64  `json:"food"`
}

func (b *Bot) status() BotStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := BotStatus{Name: b.Name, Kind: b.Kind, Model: b.provider.Describe(), Persona: b.Persona, Online: b.online, Thinking: b.thinking,
		LastSaid: b.lastSaid, Calls: b.calls, TokensIn: b.tokensIn, TokensOut: b.tokensOut, LastError: b.lastErr}
	if b.task != nil {
		s.Task = b.task.desc()
	}
	if b.building {
		s.Task = "building"
		if b.stage != "" {
			s.Task = b.stage
		}
	}
	if b.state != nil {
		s.Health, s.Food = b.state.Health, b.state.Food
	}
	s.BuildLog = append([]string(nil), b.buildLog...)
	return s
}
