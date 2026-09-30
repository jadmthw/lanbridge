// Package ai talks to the model providers that can run LANBridge's AI players.
//
// Each provider can be paid for with an API key (usage billed to that
// vendor's developer account credits) and, where the vendor allows it, with a
// consumer subscription through the vendor's own command-line tool.
package ai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Kind identifies a model family.
type Kind string

const (
	OpenAI    Kind = "openai"    // GPT
	Anthropic Kind = "anthropic" // Claude
	XAI       Kind = "xai"       // Grok
	Google    Kind = "google"    // Gemini
)

// Auth says how a provider is paid for.
type Auth string

const (
	AuthAPIKey  Auth = "api"     // API key, billed to developer-account credits
	AuthAccount Auth = "account" // subscription sign-in through the vendor's CLI
)

// Config is one provider's saved setup.
type Config struct {
	Kind    Kind   `json:"kind"`
	Auth    Auth   `json:"auth"`
	APIKey  string `json:"apiKey,omitempty"`
	Model   string `json:"model,omitempty"`
	Command string `json:"command,omitempty"` // path to the vendor CLI; default: search PATH
}

// Info describes a provider for the control panel.
type Info struct {
	Kind         Kind   `json:"kind"`
	Name         string `json:"name"`
	Vendor       string `json:"vendor"`
	Nick         string `json:"nick"`
	Account      string `json:"account,omitempty"`
	AccountCLI   string `json:"accountCli,omitempty"`
	AccountLogin string `json:"accountLogin,omitempty"`
	AccountNote  string `json:"accountNote,omitempty"`
	KeyURL       string `json:"keyUrl"`
	KeyNote      string `json:"keyNote"`
	DefaultModel string `json:"defaultModel"`
}

var infos = []Info{
	{Kind: OpenAI, Name: "GPT", Vendor: "OpenAI", Nick: "gpt",
		Account: "ChatGPT plan (Plus, Pro or Business), through OpenAI's Codex CLI", AccountCLI: "codex", AccountLogin: "codex login",
		KeyURL: "https://platform.openai.com/api-keys", KeyNote: "Billed to your OpenAI API credits.", DefaultModel: "gpt-5-mini"},
	{Kind: Anthropic, Name: "Claude", Vendor: "Anthropic", Nick: "claude",
		AccountNote: "Anthropic doesn't allow third-party apps to run on Claude Free, Pro or Max plans, so Claude uses an API key.",
		KeyURL:      "https://console.anthropic.com/settings/keys", KeyNote: "Billed to your Anthropic Console credits.", DefaultModel: "claude-haiku-4-5-20251001"},
	{Kind: XAI, Name: "Grok", Vendor: "xAI", Nick: "grok",
		Account: "SuperGrok or X Premium+ plan, through xAI's Grok Build CLI", AccountCLI: "grok", AccountLogin: "grok login",
		KeyURL: "https://console.x.ai", KeyNote: "Billed to your xAI API credits."},
	{Kind: Google, Name: "Gemini", Vendor: "Google", Nick: "gemini",
		AccountNote: "Google ended Gemini CLI sign-in for personal accounts and doesn't allow other apps to use it, so Gemini uses an API key.",
		KeyURL:      "https://aistudio.google.com/apikey", KeyNote: "Google AI Studio keys have a free tier.", DefaultModel: "gemini-flash-latest"},
}

// Infos lists every provider.
func Infos() []Info { return append([]Info(nil), infos...) }

// InfoFor returns one provider's description.
func InfoFor(k Kind) (Info, bool) {
	for _, i := range infos {
		if i.Kind == k {
			return i, true
		}
	}
	return Info{}, false
}

// ParseKind accepts the names people type in chat ("gpt", "claude", "grok", "gemini", …).
func ParseKind(s string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "gpt", "openai", "chatgpt", "codex":
		return OpenAI, true
	case "claude", "anthropic":
		return Anthropic, true
	case "grok", "xai", "x":
		return XAI, true
	case "gemini", "google":
		return Google, true
	}
	return "", false
}

// Request is one model call.
type Request struct {
	System    string
	Prompt    string
	MaxTokens int
	Schema    string // JSON schema for the answer; tools that support it enforce it
	// Progress, if set, receives status lines from CLI tools while they work.
	Progress func(line string)
}

// Response is a model's answer.
type Response struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
	TotalTokens  int // when a tool only reports a total (the Codex CLI)
	Took         time.Duration
}

// Provider generates text.
type Provider interface {
	Generate(ctx context.Context, req Request) (Response, error)
	Describe() string
}

// New returns a provider for a saved configuration.
func New(c Config) (Provider, error) {
	info, ok := InfoFor(c.Kind)
	if !ok {
		return nil, fmt.Errorf("unknown model provider %q", c.Kind)
	}
	if c.Auth == AuthAccount {
		if info.AccountCLI == "" {
			return nil, errors.New(info.AccountNote)
		}
		path, err := FindCLI(info.AccountCLI, c.Command)
		if err != nil {
			return nil, fmt.Errorf("the %s command isn't installed (or LANBridge can't find it). Install it, run \"%s\" once, then try again", info.AccountCLI, info.AccountLogin)
		}
		if c.Kind == OpenAI {
			return &codexCLI{path: path, model: c.Model}, nil
		}
		return &grokCLI{path: path, model: c.Model}, nil
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, fmt.Errorf("add a %s API key first", info.Vendor)
	}
	return newHTTP(c), nil
}

// FindCLI locates a vendor command-line tool.
func FindCLI(name, override string) (string, error) {
	if override != "" {
		if st, err := os.Stat(override); err == nil && !st.IsDir() {
			return override, nil
		}
		return "", fmt.Errorf("%s not found", override)
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(home, ".local", "bin"), filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, "."+name, "bin"),
		filepath.Join(home, ".bun", "bin"), filepath.Join(home, ".cargo", "bin"), "/opt/homebrew/bin", "/usr/local/bin",
	}
	exts := []string{""}
	if runtime.GOOS == "windows" {
		dirs = append(dirs, filepath.Join(os.Getenv("APPDATA"), "npm"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", name))
		exts = []string{".exe", ".cmd", ".bat"}
	}
	for _, d := range dirs {
		for _, e := range exts {
			p := filepath.Join(d, name+e)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

// KeyHint shows the end of an API key so people can tell keys apart.
func KeyHint(key string) string {
	key = strings.TrimSpace(key)
	if len(key) < 8 {
		return ""
	}
	return "…" + key[len(key)-4:]
}
