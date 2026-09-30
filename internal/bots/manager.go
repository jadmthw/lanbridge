// Package bots runs AI-controlled players (GPT, Claude, Grok, Gemini) inside
// a Minecraft world through the Carpet mod bridge, and routes chat to them.
package bots

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"lanbridge/internal/ai"
	"lanbridge/internal/bridge"
	"lanbridge/internal/logx"
)

// Settings are saved with the app's settings.
type Settings struct {
	Providers map[ai.Kind]ai.Config `json:"providers,omitempty"`
	HostOnly  bool                  `json:"hostOnly"`     // only the world's host may add or remove AI players from chat
	MaxBots   int                   `json:"maxBots"`      // at most this many AI players at once
	MaxPerMin int                   `json:"maxPerMinute"` // replies per AI player per minute (protects your credits)
	Folders   []string              `json:"folders,omitempty"`
}

// DefaultSettings are the out-of-the-box limits.
func DefaultSettings() Settings { return Settings{HostOnly: true, MaxBots: 4, MaxPerMin: 10} }

// link is the part of *bridge.Client the manager uses.
type link interface {
	Send(op string, args map[string]any)
	Call(ctx context.Context, op string, args map[string]any, out any) error
}

// Manager owns the bridge connection and the AI players.
type Manager struct {
	log      *logx.Logger
	settings func() Settings
	ctx      context.Context
	cancel   context.CancelFunc

	mu        sync.Mutex
	client    *bridge.Client
	link      link
	hello     bridge.Hello
	since     time.Time
	lastErr   string
	bots      map[string]*Bot
	sticky    map[string]sticky
	interest  time.Time
	instances []bridge.Instance
	instTime  time.Time
	tests     map[ai.Kind]string
}

type sticky struct {
	bot string
	at  time.Time
}

// NewManager starts watching for a Minecraft world running the bridge app.
func NewManager(log *logx.Logger, settings func() Settings) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{log: log, settings: settings, ctx: ctx, cancel: cancel, bots: map[string]*Bot{}, sticky: map[string]sticky{}, tests: map[ai.Kind]string{}}
	go m.run()
	return m
}

// Close removes every AI player and stops.
func (m *Manager) Close() {
	m.RemoveAll()
	m.cancel()
	m.mu.Lock()
	c := m.client
	m.mu.Unlock()
	if c != nil {
		time.Sleep(300 * time.Millisecond) // let the removals reach the game
		c.Close()
	}
}

// Touch marks that someone is looking at the AI page, so the manager keeps
// looking for a world even before any provider is set up.
func (m *Manager) Touch() {
	m.mu.Lock()
	m.interest = time.Now()
	m.mu.Unlock()
}

func (m *Manager) wanted() bool {
	m.mu.Lock()
	recent := time.Since(m.interest) < 15*time.Minute
	m.mu.Unlock()
	return recent || len(m.settings().Providers) > 0
}

func (m *Manager) run() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		m.mu.Lock()
		c := m.client
		m.mu.Unlock()
		if c == nil {
			if m.wanted() {
				m.connect()
			}
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
			}
			continue
		}
		select {
		case <-m.ctx.Done():
			return
		case ev := <-c.Events():
			m.handle(ev)
		case <-c.Done():
			m.disconnected(c)
		case <-t.C:
			if h, err := bridge.ReadHello(c.Dir); err == nil {
				m.mu.Lock()
				m.hello = h
				m.mu.Unlock()
			}
		}
	}
}

func (m *Manager) connect() {
	dir, h, ok := bridge.Active(m.settings().Folders)
	if !ok {
		return
	}
	c, err := bridge.Open(dir, m.log)
	if err != nil {
		m.mu.Lock()
		m.lastErr = err.Error()
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.client, m.link, m.hello, m.since, m.lastErr = c, c, h, time.Now(), ""
	m.mu.Unlock()
	m.log.Infof("AI players: connected to Minecraft world %q", h.World)
}

func (m *Manager) disconnected(c *bridge.Client) {
	m.mu.Lock()
	if m.client != c {
		m.mu.Unlock()
		return
	}
	m.client, m.link = nil, nil
	if err := c.Err(); err != nil && !errors.Is(err, bridge.ErrClosed) {
		m.lastErr = err.Error()
	}
	bots := m.bots
	m.bots = map[string]*Bot{}
	m.mu.Unlock()
	for _, b := range bots {
		b.stop()
	}
	m.log.Infof("AI players: the Minecraft world closed")
}

// attach connects the manager to a link directly (tests).
func (m *Manager) attach(l link, h bridge.Hello) {
	m.mu.Lock()
	m.link, m.hello, m.since = l, h, time.Now()
	m.mu.Unlock()
}

func (m *Manager) send(op string, args map[string]any) {
	m.mu.Lock()
	l := m.link
	m.mu.Unlock()
	if l != nil {
		l.Send(op, args)
	}
}

func (m *Manager) call(ctx context.Context, op string, args map[string]any, out any) error {
	m.mu.Lock()
	l := m.link
	m.mu.Unlock()
	if l == nil {
		return bridge.ErrClosed
	}
	return l.Call(ctx, op, args, out)
}

// tell sends a system message to one player, or everyone when to is "".
func (m *Manager) tell(to, text string) {
	args := map[string]any{"text": text}
	if to != "" {
		args["to"] = to
	}
	m.send("tell", args)
}

func (m *Manager) bot(name string) *Bot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bots[strings.ToLower(name)]
}

func (m *Manager) allBots() []*Bot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Bot, 0, len(m.bots))
	for _, b := range m.bots {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].created.Before(out[j].created) })
	return out
}

func (m *Manager) otherBots(self string) []string {
	var out []string
	for _, b := range m.allBots() {
		if b.Name != self {
			out = append(out, b.Name)
		}
	}
	return out
}

func (m *Manager) playerNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.hello.Players...)
}

func (m *Manager) handle(ev bridge.Event) {
	switch ev.Type {
	case "chat":
		m.onChat(ev.Player, bool(ev.Host), ev.Message)
	case "join":
		if b := m.bot(ev.Player); b != nil && bool(ev.Fake) {
			b.markJoined()
		}
	case "leave":
		if b := m.bot(ev.Player); b != nil && bool(ev.Fake) {
			m.mu.Lock()
			delete(m.bots, strings.ToLower(ev.Player))
			m.mu.Unlock()
			b.stop()
			b.mu.Lock()
			expected := b.removing
			b.mu.Unlock()
			if !expected {
				m.log.Infof("AI player %s left the world", b.Name)
			}
		}
	case "death":
		if b := m.bot(ev.Player); b != nil {
			m.log.Infof("AI player %s died", b.Name)
			b.StopTask()
			go m.afterDeath(b)
		}
	}
}

// afterDeath respawns an AI player if Carpet left it dead.
func (m *Manager) afterDeath(b *Bot) {
	select {
	case <-b.ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}
	var st State
	if err := m.call(b.ctx, "state", map[string]any{"name": b.Name}, &st); err == nil && st.Online && st.Health > 0 {
		return
	}
	b.mu.Lock()
	b.removing = true
	b.mu.Unlock()
	_ = m.call(b.ctx, "remove", map[string]any{"name": b.Name}, nil)
	select {
	case <-b.ctx.Done():
	case <-time.After(3 * time.Second):
	}
	if _, err := m.Spawn(SpawnRequest{Kind: b.Kind, Name: b.Name, Persona: b.Persona, Mode: b.Mode}); err != nil {
		m.log.Warnf("couldn't respawn %s: %v", b.Name, err)
	}
}

// heard records a chat line for every AI player's memory.
func (m *Manager) heard(from, text string, fromBot bool) {
	l := line{at: time.Now(), from: from, text: text}
	for _, b := range m.allBots() {
		if b.Name != from {
			b.hear(l, false)
		}
	}
	_ = fromBot
}

var wordRe = regexp.MustCompile(`[A-Za-z0-9_]+`)

// addressed decides which AI players a chat message is for: those named in
// it, "everyone"/"@all", or the one this player was just talking to.
func (m *Manager) addressed(from, text string) []*Bot {
	bots := m.allBots()
	if len(bots) == 0 {
		return nil
	}
	lower := strings.ToLower(text)
	words := map[string]bool{}
	for _, w := range wordRe.FindAllString(lower, -1) {
		words[w] = true
	}
	var out []*Bot
	if words["everyone"] || words["all"] && strings.Contains(lower, "@all") || words["bots"] || words["guys"] && len(bots) > 1 {
		out = bots
	} else {
		kinds := map[ai.Kind][]*Bot{}
		for _, b := range bots {
			kinds[b.Kind] = append(kinds[b.Kind], b)
		}
		for _, b := range bots {
			n := strings.ToLower(b.Name)
			info, _ := ai.InfoFor(b.Kind)
			if words[n] || strings.Contains(lower, "@"+n) || (words[info.Nick] && len(kinds[b.Kind]) == 1) {
				out = append(out, b)
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(out) == 0 {
		if s, ok := m.sticky[strings.ToLower(from)]; ok && time.Since(s.at) < 90*time.Second {
			if b := m.bots[strings.ToLower(s.bot)]; b != nil {
				out = []*Bot{b}
			}
		}
	}
	if len(out) == 1 {
		m.sticky[strings.ToLower(from)] = sticky{bot: out[0].Name, at: time.Now()}
	}
	return out
}

func (m *Manager) onChat(from string, host bool, text string) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(strings.ToLower(text), "!ai") {
		m.command(from, host, strings.Fields(text)[1:])
		return
	}
	to := map[*Bot]bool{}
	for _, b := range m.addressed(from, text) {
		to[b] = true
	}
	l := line{at: time.Now(), from: from, text: text}
	for _, b := range m.allBots() {
		b.hear(l, to[b])
	}
}

const help = "[LANBridge] !ai spawn <gpt|claude|grok|gemini> [name] [personality]  -  !ai remove <name|all>  -  !ai stop <name|all>  -  !ai list. Talk to an AI player by saying its name."

func (m *Manager) command(from string, host bool, args []string) {
	if len(args) == 0 {
		m.tell(from, help)
		return
	}
	restricted := m.settings().HostOnly && !host
	switch strings.ToLower(args[0]) {
	case "spawn", "add", "summon":
		if restricted {
			m.tell(from, "[LANBridge] Only the host can add AI players (they can change that in LANBridge).")
			return
		}
		if len(args) < 2 {
			m.tell(from, "[LANBridge] Which model? !ai spawn gpt, claude, grok or gemini")
			return
		}
		kind, ok := ai.ParseKind(args[1])
		if !ok {
			m.tell(from, "[LANBridge] I don't know "+args[1]+". Use gpt, claude, grok or gemini.")
			return
		}
		req := SpawnRequest{Kind: kind, Near: from}
		if len(args) > 2 {
			req.Name = args[2]
			req.Persona = strings.Join(args[3:], " ")
		}
		go func() {
			name, err := m.Spawn(req)
			if err != nil {
				m.tell(from, "[LANBridge] Couldn't add an AI player: "+err.Error())
				return
			}
			info, _ := ai.InfoFor(kind)
			m.tell("", fmt.Sprintf("[LANBridge] %s (%s) joined. Say its name to talk to it.", name, info.Name))
		}()
	case "remove", "kick", "despawn":
		if restricted {
			m.tell(from, "[LANBridge] Only the host can remove AI players.")
			return
		}
		if len(args) < 2 {
			m.tell(from, "[LANBridge] Remove who? !ai remove <name> or !ai remove all")
			return
		}
		go func() {
			if strings.EqualFold(args[1], "all") {
				m.RemoveAll()
				return
			}
			if err := m.Remove(args[1]); err != nil {
				m.tell(from, "[LANBridge] "+err.Error())
			}
		}()
	case "stop":
		for _, b := range m.allBots() {
			if len(args) < 2 || strings.EqualFold(args[1], "all") || strings.EqualFold(args[1], b.Name) {
				b.StopTask()
			}
		}
	case "list":
		bots := m.allBots()
		if len(bots) == 0 {
			m.tell(from, "[LANBridge] No AI players yet. Try !ai spawn claude")
			return
		}
		var parts []string
		for _, b := range bots {
			s := b.status()
			info, _ := ai.InfoFor(b.Kind)
			d := b.Name + " (" + info.Name
			if s.Task != "" {
				d += ", " + s.Task
			}
			parts = append(parts, d+")")
		}
		m.tell(from, "[LANBridge] AI players: "+strings.Join(parts, ", "))
	default:
		m.tell(from, help)
	}
}

// SpawnRequest describes a new AI player.
type SpawnRequest struct {
	Kind    ai.Kind `json:"kind"`
	Name    string  `json:"name"`
	Persona string  `json:"persona"`
	Mode    string  `json:"mode"` // survival (default) or creative
	Near    string  `json:"near"` // spawn next to this player
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

var reserved = map[string]bool{"all": true, "survival": true, "creative": true, "spectating": true, "adventure": true, "spectator": true, "everyone": true, "bots": true}

// Spawn adds an AI player to the connected world and returns its name.
func (m *Manager) Spawn(req SpawnRequest) (string, error) {
	m.mu.Lock()
	connected := m.link != nil
	count := len(m.bots)
	m.mu.Unlock()
	if !connected {
		return "", errors.New("no Minecraft world is connected. Open your world (with Carpet) and run /script load lanbridge")
	}
	s := m.settings()
	if s.MaxBots > 0 && count >= s.MaxBots {
		return "", fmt.Errorf("that's the limit of %d AI players (change it in LANBridge)", s.MaxBots)
	}
	cfg, ok := s.Providers[req.Kind]
	if !ok {
		info, _ := ai.InfoFor(req.Kind)
		return "", fmt.Errorf("%s isn't set up yet. Add it in LANBridge > AI players", info.Name)
	}
	prov, err := ai.New(cfg)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = m.autoName(req.Kind)
	}
	if !nameRe.MatchString(name) || reserved[strings.ToLower(name)] {
		return "", errors.New("names need 3-16 letters, numbers or underscores")
	}
	for _, p := range m.playerNames() {
		if strings.EqualFold(p, name) {
			return "", errors.New("a player called " + name + " is already in the world")
		}
	}
	mode := "survival"
	if req.Mode == "creative" {
		mode = "creative"
	}
	b := newBot(m, name, req.Kind, req.Persona, mode, prov)
	m.mu.Lock()
	if m.bots[strings.ToLower(name)] != nil {
		m.mu.Unlock()
		return "", errors.New(name + " is already here")
	}
	m.bots[strings.ToLower(name)] = b
	m.mu.Unlock()
	fail := func(err error) (string, error) {
		m.mu.Lock()
		if m.bots[strings.ToLower(name)] == b {
			delete(m.bots, strings.ToLower(name))
		}
		m.mu.Unlock()
		b.stop()
		return "", err
	}
	var res struct {
		Output []string `json:"output"`
	}
	if err := m.call(m.ctx, "spawn", map[string]any{"name": name, "near": req.Near, "mode": mode}, &res); err != nil {
		return fail(err)
	}
	select {
	case <-b.joined:
	case <-time.After(12 * time.Second):
		msg := "Carpet didn't add the player"
		if len(res.Output) > 0 {
			msg += ": " + strings.Join(res.Output, " ")
		}
		return fail(errors.New(msg))
	case <-m.ctx.Done():
		return fail(errors.New("stopped"))
	}
	b.start()
	m.log.Infof("AI player %s joined (%s)", name, prov.Describe())
	return name, nil
}

func (m *Manager) autoName(kind ai.Kind) string {
	info, _ := ai.InfoFor(kind)
	taken := map[string]bool{}
	for _, p := range m.playerNames() {
		taken[strings.ToLower(p)] = true
	}
	m.mu.Lock()
	for k := range m.bots {
		taken[k] = true
	}
	m.mu.Unlock()
	for i := 1; ; i++ {
		n := fmt.Sprintf("%s_%d", info.Name, i)
		if !taken[strings.ToLower(n)] {
			return n
		}
	}
}

// Remove takes an AI player out of the world.
func (m *Manager) Remove(name string) error {
	b := m.bot(name)
	if b == nil {
		return fmt.Errorf("there's no AI player called %s", name)
	}
	b.mu.Lock()
	b.removing = true
	b.mu.Unlock()
	err := m.call(m.ctx, "remove", map[string]any{"name": b.Name}, nil)
	m.mu.Lock()
	if m.bots[strings.ToLower(b.Name)] == b {
		delete(m.bots, strings.ToLower(b.Name))
	}
	m.mu.Unlock()
	b.stop()
	if err == nil {
		m.log.Infof("removed AI player %s", b.Name)
	}
	return err
}

// RemoveAll takes every AI player out.
func (m *Manager) RemoveAll() {
	for _, b := range m.allBots() {
		_ = m.Remove(b.Name)
	}
}

// StopTask makes an AI player stop what it's doing.
func (m *Manager) StopTask(name string) error {
	b := m.bot(name)
	if b == nil {
		return fmt.Errorf("there's no AI player called %s", name)
	}
	b.StopTask()
	return nil
}

// Test asks a provider for a tiny answer, to check that it's set up.
func (m *Manager) Test(ctx context.Context, cfg ai.Config) (string, error) {
	p, err := ai.New(cfg)
	if err != nil {
		m.setTest(cfg.Kind, "error: "+err.Error())
		return "", err
	}
	resp, err := p.Generate(ctx, ai.Request{
		System: `You are testing a connection. Answer with only this JSON: {"say": "ready", "actions": []}`,
		Prompt: "Connection test.", MaxTokens: 1500})
	if err != nil {
		m.setTest(cfg.Kind, "error: "+err.Error())
		return "", err
	}
	r := parseReply(resp.Text)
	msg := fmt.Sprintf("Works (%s, %.1fs): %q", resp.Model, resp.Took.Seconds(), shorten(r.Say, 40))
	m.setTest(cfg.Kind, msg)
	return msg, nil
}

func (m *Manager) setTest(k ai.Kind, s string) {
	m.mu.Lock()
	m.tests[k] = s
	m.mu.Unlock()
}

// ProviderStatus describes one model family's setup.
type ProviderStatus struct {
	ai.Info
	Auth       ai.Auth `json:"auth"`
	Configured bool    `json:"configured"`
	HasKey     bool    `json:"hasKey"`
	KeyHint    string  `json:"keyHint,omitempty"`
	Model      string  `json:"model"`
	CLIPath    string  `json:"cliPath,omitempty"`
	LastTest   string  `json:"lastTest,omitempty"`
}

// Status is everything the AI page shows.
type Status struct {
	Connected bool              `json:"connected"`
	World     string            `json:"world,omitempty"`
	Players   []string          `json:"players"`
	Since     time.Time         `json:"since"`
	Error     string            `json:"error,omitempty"`
	Command   string            `json:"command"`
	Instances []bridge.Instance `json:"instances"`
	Providers []ProviderStatus  `json:"providers"`
	Bots      []BotStatus       `json:"bots"`
	HostOnly  bool              `json:"hostOnly"`
	MaxBots   int               `json:"maxBots"`
	MaxPerMin int               `json:"maxPerMinute"`
}

// Status returns a snapshot for the control panel.
func (m *Manager) Status() Status {
	m.Touch()
	s := m.settings()
	m.mu.Lock()
	st := Status{Connected: m.link != nil, World: m.hello.World, Players: append([]string{}, m.hello.Players...), Since: m.since,
		Error: m.lastErr, Command: "/script load " + bridge.AppName, HostOnly: s.HostOnly, MaxBots: s.MaxBots, MaxPerMin: s.MaxPerMin}
	if time.Since(m.instTime) > 5*time.Second {
		m.instances, m.instTime = nil, time.Now()
		m.mu.Unlock()
		inst := bridge.Instances(s.Folders)
		m.mu.Lock()
		m.instances = inst
	}
	st.Instances = append([]bridge.Instance{}, m.instances...)
	tests := map[ai.Kind]string{}
	for k, v := range m.tests {
		tests[k] = v
	}
	m.mu.Unlock()
	if !st.Connected {
		st.World, st.Players = "", []string{}
	}
	for _, info := range ai.Infos() {
		ps := ProviderStatus{Info: info, Auth: ai.AuthAPIKey, LastTest: tests[info.Kind]}
		if c, ok := s.Providers[info.Kind]; ok {
			ps.Configured, ps.Auth, ps.Model = true, c.Auth, c.Model
			ps.HasKey, ps.KeyHint = c.APIKey != "", ai.KeyHint(c.APIKey)
		}
		if info.AccountCLI != "" {
			if p, err := ai.FindCLI(info.AccountCLI, ""); err == nil {
				ps.CLIPath = p
			}
		}
		st.Providers = append(st.Providers, ps)
	}
	st.Bots = []BotStatus{}
	for _, b := range m.allBots() {
		st.Bots = append(st.Bots, b.status())
	}
	return st
}
