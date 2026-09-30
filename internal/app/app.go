// Package app ties hosting, joining and saved settings together for the
// control panel and the command line.
package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"lanbridge/internal/ai"
	"lanbridge/internal/bots"
	"lanbridge/internal/bridge"
	"lanbridge/internal/invite"
	"lanbridge/internal/logx"
	"lanbridge/internal/relay"
	"lanbridge/internal/tunnel"
	"lanbridge/internal/ws"
)

// Set at build time by main.
var (
	Version      = "dev"
	DefaultRelay = ""
)

// Settings are saved between runs.
type Settings struct {
	Name          string `json:"name"`
	RelayURL      string `json:"relayUrl"`
	RelayCustom   bool   `json:"relayCustom"`
	RelayToken    string `json:"relayToken"`
	HostPort      int    `json:"hostPort"`
	UPnP          bool   `json:"upnp"`
	ManualForward bool   `json:"manualForward"`
	JoinPort      int    `json:"joinPort"`
	ShareLAN      bool   `json:"shareLan"`
	Secret        string `json:"secret,omitempty"`
	LastCode      string `json:"lastCode,omitempty"`

	AI bots.Settings `json:"ai"`
}

// EffectiveRelay is the relay hosting will use.
func (s Settings) EffectiveRelay() string {
	if s.RelayCustom {
		return s.RelayURL
	}
	return DefaultRelay
}

func defaults() Settings {
	return Settings{Name: defaultName(), HostPort: tunnel.DefaultHostPort, UPnP: true, JoinPort: 25565, AI: bots.DefaultSettings()}
}

func defaultName() string {
	if u, err := user.Current(); err == nil {
		n := u.Username
		if i := strings.LastIndexAny(n, `\/`); i >= 0 {
			n = n[i+1:]
		}
		if n != "" && n != "root" {
			return truncate(n, 24)
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return truncate(strings.Split(h, ".")[0], 24)
	}
	return "Friend"
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// App is one running LANBridge.
type App struct {
	log  *logx.Logger
	path string

	mu   sync.Mutex
	s    Settings
	host *tunnel.Host
	join *tunnel.Joiner
	ai   *bots.Manager
}

// New loads saved settings.
func New(log *logx.Logger) (*App, error) {
	tunnel.Version = Version
	ws.UserAgent = "lanbridge/" + Version
	a := &App{log: log, s: defaults()}
	if dir, err := os.UserConfigDir(); err == nil {
		a.path = filepath.Join(dir, "LANBridge", "settings.json")
		if b, err := os.ReadFile(a.path); err == nil {
			s := a.s
			if err := json.Unmarshal(b, &s); err != nil {
				log.Warnf("ignoring unreadable settings file %s: %v", a.path, err)
			} else {
				a.s = s
			}
		}
	}
	if a.s.HostPort < 1 || a.s.HostPort > 65535 {
		a.s.HostPort = tunnel.DefaultHostPort
	}
	if a.s.JoinPort < 1 || a.s.JoinPort > 65535 {
		a.s.JoinPort = 25565
	}
	if strings.TrimSpace(a.s.Name) == "" {
		a.s.Name = defaultName()
	}
	a.ai = bots.NewManager(log, a.aiSettings)
	return a, nil
}

// aiSettings returns a copy of the AI settings (the manager reads them concurrently).
func (a *App) aiSettings() bots.Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s.AI
	s.Providers = make(map[ai.Kind]ai.Config, len(a.s.AI.Providers))
	for k, v := range a.s.AI.Providers {
		s.Providers[k] = v
	}
	s.Folders = append([]string(nil), a.s.AI.Folders...)
	return s
}

// Log returns the app's logger.
func (a *App) Log() *logx.Logger { return a.log }

func (a *App) saveLocked() {
	if a.path == "" {
		return
	}
	b, _ := json.MarshalIndent(a.s, "", "  ")
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err == nil {
		tmp := a.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, a.path)
		}
		if err == nil {
			return
		}
		a.log.Warnf("couldn't save settings: %v", err)
	}
}

func (a *App) secretLocked() ([16]byte, error) {
	var sec [16]byte
	if b, err := base64.RawURLEncoding.DecodeString(a.s.Secret); err == nil && len(b) == 16 {
		copy(sec[:], b)
		return sec, nil
	}
	sec, err := invite.NewSecret()
	if err != nil {
		return sec, err
	}
	a.s.Secret = base64.RawURLEncoding.EncodeToString(sec[:])
	a.saveLocked()
	return sec, nil
}

// HostOptions override saved settings for one hosting session (CLI flags).
type HostOptions struct {
	MCPort     int
	Port       int
	Name       string
	RelayURL   *string
	RelayToken *string
	UPnP       *bool
	Forwarded  *bool
}

// StartHost starts sharing.
func (a *App) StartHost(o HostOptions) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.host != nil {
		return nil
	}
	if a.join != nil {
		return errors.New("leave the world you joined before hosting")
	}
	sec, err := a.secretLocked()
	if err != nil {
		return err
	}
	s := a.s
	cfg := tunnel.HostConfig{Secret: sec, Name: s.Name, ListenAddr: fmt.Sprintf(":%d", s.HostPort), UPnP: s.UPnP,
		ManualForward: s.ManualForward, RelayURL: s.EffectiveRelay(), RelayToken: s.RelayToken, Log: a.log}
	if o.Port > 0 {
		cfg.ListenAddr = fmt.Sprintf(":%d", o.Port)
	}
	if o.Name != "" {
		cfg.Name = o.Name
	}
	if o.RelayURL != nil {
		cfg.RelayURL = *o.RelayURL
	}
	if o.RelayToken != nil {
		cfg.RelayToken = *o.RelayToken
	}
	if o.UPnP != nil {
		cfg.UPnP = *o.UPnP
	}
	if o.Forwarded != nil {
		cfg.ManualForward = *o.Forwarded
	}
	if o.MCPort > 0 {
		cfg.Target = fmt.Sprintf("127.0.0.1:%d", o.MCPort)
	}
	h, err := tunnel.StartHost(context.Background(), cfg)
	if err != nil {
		return err
	}
	a.host = h
	return nil
}

// Host returns the running host, if any.
func (a *App) Host() *tunnel.Host {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.host
}

// StopHost stops sharing.
func (a *App) StopHost() {
	a.mu.Lock()
	h := a.host
	a.host = nil
	a.mu.Unlock()
	if h != nil {
		h.Stop()
	}
}

// NewCode replaces the invite secret, so old codes stop working.
func (a *App) NewCode() error {
	sec, err := invite.NewSecret()
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.s.Secret = base64.RawURLEncoding.EncodeToString(sec[:])
	a.saveLocked()
	restart := a.host != nil
	a.mu.Unlock()
	a.log.Infof("made a new invite code; old codes no longer work")
	if restart {
		a.StopHost()
		return a.StartHost(HostOptions{})
	}
	return nil
}

// SelectWorld picks which detected world to share ("" = automatic).
func (a *App) SelectWorld(key string) error {
	h := a.Host()
	if h == nil {
		return errors.New("not hosting")
	}
	h.SelectWorld(key)
	return nil
}

// SetManualPort shares whatever listens on this port on this computer.
func (a *App) SetManualPort(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("enter the port number Minecraft shows in chat, like 54321")
	}
	h := a.Host()
	if h == nil {
		return errors.New("not hosting")
	}
	h.SetManualTarget(fmt.Sprintf("127.0.0.1:%d", port))
	return nil
}

// JoinOptions override saved settings for one join (CLI flags).
type JoinOptions struct {
	LocalPort int
	ShareLAN  bool
}

// StartJoin connects to a friend's world.
func (a *App) StartJoin(code string, o JoinOptions) error {
	inv, err := invite.Decode(code)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.host != nil {
		return errors.New("stop hosting before joining someone else")
	}
	if a.join != nil {
		a.join.Stop()
		a.join = nil
	}
	port := a.s.JoinPort
	if o.LocalPort != 0 {
		port = o.LocalPort
	}
	j, err := tunnel.StartJoin(context.Background(), tunnel.JoinConfig{Invite: inv, LocalPort: port, ShareLAN: a.s.ShareLAN || o.ShareLAN, Log: a.log})
	if err != nil {
		return err
	}
	a.join = j
	a.s.LastCode = strings.TrimSpace(code)
	a.saveLocked()
	name := inv.Name
	if name == "" {
		name = "your friend"
	}
	a.log.Infof("joining %s", name)
	return nil
}

// Joiner returns the running joiner, if any.
func (a *App) Joiner() *tunnel.Joiner {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.join
}

// StopJoin leaves.
func (a *App) StopJoin() {
	a.mu.Lock()
	j := a.join
	a.join = nil
	a.mu.Unlock()
	if j != nil {
		j.Stop()
	}
}

// Shutdown stops everything, including removing AI players from the world.
func (a *App) Shutdown() {
	a.ai.Close()
	a.StopHost()
	a.StopJoin()
}

// SettingsUpdate changes some settings; nil fields stay as they are.
type SettingsUpdate struct {
	Name          *string `json:"name"`
	RelayURL      *string `json:"relayUrl"`
	RelayToken    *string `json:"relayToken"`
	HostPort      *int    `json:"hostPort"`
	UPnP          *bool   `json:"upnp"`
	ManualForward *bool   `json:"manualForward"`
	JoinPort      *int    `json:"joinPort"`
	ShareLAN      *bool   `json:"shareLan"`
}

// UpdateSettings validates and saves settings. They apply the next time
// hosting or joining starts.
func (a *App) UpdateSettings(u SettingsUpdate) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	if u.Name != nil {
		n := strings.TrimSpace(*u.Name)
		if n == "" {
			return errors.New("your name can't be empty")
		}
		s.Name = truncate(n, 24)
	}
	if u.RelayURL != nil {
		norm, err := relay.NormalizeURL(*u.RelayURL)
		if err != nil {
			return err
		}
		if norm == "" || norm == DefaultRelay {
			s.RelayCustom, s.RelayURL = false, ""
		} else {
			s.RelayCustom, s.RelayURL = true, norm
		}
	}
	if u.RelayToken != nil {
		s.RelayToken = strings.TrimSpace(*u.RelayToken)
		if strings.ContainsAny(s.RelayToken, " \t\r\n") {
			return errors.New("the relay token can't contain spaces")
		}
	}
	if u.HostPort != nil {
		if *u.HostPort < 1 || *u.HostPort > 65535 {
			return errors.New("the hosting port must be between 1 and 65535")
		}
		s.HostPort = *u.HostPort
	}
	if u.JoinPort != nil {
		if *u.JoinPort < 1 || *u.JoinPort > 65535 {
			return errors.New("the Minecraft port must be between 1 and 65535")
		}
		s.JoinPort = *u.JoinPort
	}
	if u.UPnP != nil {
		s.UPnP = *u.UPnP
	}
	if u.ManualForward != nil {
		s.ManualForward = *u.ManualForward
	}
	if u.ShareLAN != nil {
		s.ShareLAN = *u.ShareLAN
	}
	a.s = s
	a.saveLocked()
	a.log.Infof("settings saved")
	return nil
}

// SettingsView is what the control panel shows.
type SettingsView struct {
	Name           string `json:"name"`
	RelayURL       string `json:"relayUrl"`
	RelayIsDefault bool   `json:"relayIsDefault"`
	DefaultRelay   string `json:"defaultRelay"`
	RelayToken     string `json:"relayToken"`
	HostPort       int    `json:"hostPort"`
	UPnP           bool   `json:"upnp"`
	ManualForward  bool   `json:"manualForward"`
	JoinPort       int    `json:"joinPort"`
	ShareLAN       bool   `json:"shareLan"`
	LastCode       string `json:"lastCode"`
	Path           string `json:"path"`
}

// State is everything the control panel shows.
type State struct {
	Version  string             `json:"version"`
	OS       string             `json:"os"`
	Arch     string             `json:"arch"`
	Mode     string             `json:"mode"` // idle, host or join
	Host     *tunnel.HostStatus `json:"host,omitempty"`
	Join     *tunnel.JoinStatus `json:"join,omitempty"`
	Settings SettingsView       `json:"settings"`
	Logs     []logx.Entry       `json:"logs"`
}

// State returns a snapshot for the control panel.
func (a *App) State() State {
	a.mu.Lock()
	h, j, s := a.host, a.join, a.s
	a.mu.Unlock()
	osName := map[string]string{"darwin": "macOS", "windows": "Windows", "linux": "Linux"}[runtime.GOOS]
	if osName == "" {
		osName = runtime.GOOS
	}
	st := State{Version: Version, OS: osName, Arch: runtime.GOARCH, Mode: "idle", Logs: a.log.Recent(150),
		Settings: SettingsView{Name: s.Name, RelayURL: s.EffectiveRelay(), RelayIsDefault: !s.RelayCustom, DefaultRelay: DefaultRelay,
			RelayToken: s.RelayToken, HostPort: s.HostPort, UPnP: s.UPnP, ManualForward: s.ManualForward, JoinPort: s.JoinPort,
			ShareLAN: s.ShareLAN, LastCode: s.LastCode, Path: a.path}}
	if h != nil {
		hs := h.Status()
		st.Host, st.Mode = &hs, "host"
	}
	if j != nil {
		js := j.Status()
		st.Join, st.Mode = &js, "join"
	}
	return st
}

// ---- AI players ----

// AIStatus returns the AI players page's state.
func (a *App) AIStatus() bots.Status { return a.ai.Status() }

// ProviderUpdate changes one model provider's setup; nil fields stay as they are.
type ProviderUpdate struct {
	Kind     ai.Kind  `json:"kind"`
	Auth     *ai.Auth `json:"auth"`
	APIKey   *string  `json:"apiKey"` // empty keeps the saved key
	ClearKey bool     `json:"clearKey"`
	Model    *string  `json:"model"`
	Command  *string  `json:"command"`
	Remove   bool     `json:"remove"`
}

// AISetProvider saves a provider's setup.
func (a *App) AISetProvider(u ProviderUpdate) error {
	info, ok := ai.InfoFor(u.Kind)
	if !ok {
		return errors.New("unknown model provider")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	provs := map[ai.Kind]ai.Config{}
	for k, v := range a.s.AI.Providers {
		provs[k] = v
	}
	if u.Remove {
		delete(provs, u.Kind)
	} else {
		c, exists := provs[u.Kind]
		if !exists {
			c = ai.Config{Kind: u.Kind, Auth: ai.AuthAPIKey}
		}
		if u.Auth != nil {
			if *u.Auth == ai.AuthAccount && info.AccountCLI == "" {
				return errors.New(info.AccountNote)
			}
			if *u.Auth != ai.AuthAccount && *u.Auth != ai.AuthAPIKey {
				return errors.New("unknown sign-in method")
			}
			c.Auth = *u.Auth
		}
		if u.APIKey != nil && strings.TrimSpace(*u.APIKey) != "" {
			c.APIKey = strings.TrimSpace(*u.APIKey)
		}
		if u.ClearKey {
			c.APIKey = ""
		}
		if u.Model != nil {
			c.Model = strings.TrimSpace(*u.Model)
		}
		if u.Command != nil {
			c.Command = strings.TrimSpace(*u.Command)
		}
		provs[u.Kind] = c
	}
	a.s.AI.Providers = provs
	a.saveLocked()
	return nil
}

func (a *App) providerConfig(kind ai.Kind) (ai.Config, error) {
	c, ok := a.aiSettings().Providers[kind]
	if !ok {
		info, _ := ai.InfoFor(kind)
		return c, fmt.Errorf("set up %s first", info.Name)
	}
	return c, nil
}

// AITest checks that a provider answers.
func (a *App) AITest(ctx context.Context, kind ai.Kind) (string, error) {
	c, err := a.providerConfig(kind)
	if err != nil {
		return "", err
	}
	return a.ai.Test(ctx, c)
}

// AIModels lists the models a provider's API key can use.
func (a *App) AIModels(ctx context.Context, kind ai.Kind) ([]string, error) {
	c, err := a.providerConfig(kind)
	if err != nil {
		return nil, err
	}
	return ai.ListModels(ctx, c)
}

// AIInstall copies the Scarpet bridge app into a Minecraft folder.
func (a *App) AIInstall(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("pick a Minecraft folder")
	}
	p, err := bridge.Install(dir)
	if err != nil {
		return "", err
	}
	a.log.Infof("installed the AI bridge script at %s", p)
	return p, a.AIAddFolder(dir)
}

// AIAddFolder remembers a Minecraft folder LANBridge didn't find by itself.
func (a *App) AIAddFolder(dir string) error {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return errors.New("that folder doesn't exist")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.s.AI.Folders {
		if f == dir {
			return nil
		}
	}
	a.s.AI.Folders = append(append([]string(nil), a.s.AI.Folders...), dir)
	a.saveLocked()
	return nil
}

// AISpawn adds an AI player.
func (a *App) AISpawn(req bots.SpawnRequest) (string, error) { return a.ai.Spawn(req) }

// AIRemove removes an AI player ("all" removes every one).
func (a *App) AIRemove(name string) error {
	if strings.EqualFold(name, "all") {
		a.ai.RemoveAll()
		return nil
	}
	return a.ai.Remove(name)
}

// AIStop stops an AI player's current task.
func (a *App) AIStop(name string) error { return a.ai.StopTask(name) }

// AIOptions changes the AI limits.
type AIOptions struct {
	HostOnly  *bool `json:"hostOnly"`
	MaxBots   *int  `json:"maxBots"`
	MaxPerMin *int  `json:"maxPerMinute"`
}

// AISetOptions saves the AI limits.
func (a *App) AISetOptions(o AIOptions) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if o.HostOnly != nil {
		a.s.AI.HostOnly = *o.HostOnly
	}
	if o.MaxBots != nil {
		if *o.MaxBots < 1 || *o.MaxBots > 20 {
			return errors.New("AI players: pick 1 to 20")
		}
		a.s.AI.MaxBots = *o.MaxBots
	}
	if o.MaxPerMin != nil {
		if *o.MaxPerMin < 1 || *o.MaxPerMin > 120 {
			return errors.New("replies per minute: pick 1 to 120")
		}
		a.s.AI.MaxPerMin = *o.MaxPerMin
	}
	a.saveLocked()
	return nil
}
