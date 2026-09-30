package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeAPI(t *testing.T, kind Kind) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		switch kind {
		case OpenAI, XAI:
			if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k-123" {
				http.Error(w, `{"error":{"message":"bad key"}}`, 401)
				return
			}
			if kind == OpenAI && req["response_format"] == nil {
				t.Error("OpenAI request should ask for JSON")
			}
			io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"{\"say\":\"hi\",\"actions\":[]}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		case Anthropic:
			if r.URL.Path != "/messages" || r.Header.Get("x-api-key") != "k-123" || r.Header.Get("anthropic-version") == "" {
				http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, 401)
				return
			}
			if req["system"] == nil || req["max_tokens"] == nil {
				t.Error("Anthropic request is missing system or max_tokens")
			}
			io.WriteString(w, `{"content":[{"type":"text","text":"{\"say\":\"hi\",\"actions\":[]}"}],"usage":{"input_tokens":10,"output_tokens":5}}`)
		case Google:
			if !strings.HasSuffix(r.URL.Path, ":generateContent") || r.Header.Get("x-goog-api-key") != "k-123" {
				http.Error(w, `{"error":{"code":403,"message":"API key not valid","status":"PERMISSION_DENIED"}}`, 403)
				return
			}
			io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true},{"text":"{\"say\":\"hi\",\"actions\":[]}"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAPIProviders(t *testing.T) {
	for _, kind := range []Kind{OpenAI, Anthropic, XAI, Google} {
		t.Run(string(kind), func(t *testing.T) {
			srv := fakeAPI(t, kind)
			old := BaseURLs[kind]
			BaseURLs[kind] = srv.URL
			defer func() { BaseURLs[kind] = old }()
			p, err := New(Config{Kind: kind, Auth: AuthAPIKey, APIKey: "k-123", Model: "test-model"})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := p.Generate(context.Background(), Request{System: "sys", Prompt: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(resp.Text, `"say":"hi"`) || resp.InputTokens != 10 || resp.OutputTokens != 5 {
				t.Fatalf("unexpected response %+v", resp)
			}
			bad, _ := New(Config{Kind: kind, Auth: AuthAPIKey, APIKey: "wrong", Model: "m"})
			if _, err := bad.Generate(context.Background(), Request{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "rejected") {
				t.Fatalf("expected a key rejection, got %v", err)
			}
		})
	}
}

func TestAccountRules(t *testing.T) {
	if _, err := New(Config{Kind: Anthropic, Auth: AuthAccount}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Claude must not offer subscription sign-in, got %v", err)
	}
	if _, err := New(Config{Kind: Google, Auth: AuthAccount}); err == nil {
		t.Fatal("Gemini must not offer subscription sign-in")
	}
	if _, err := New(Config{Kind: OpenAI, Auth: AuthAPIKey}); err == nil {
		t.Fatal("expected missing-key error")
	}
	if k, ok := ParseKind("ChatGPT"); !ok || k != OpenAI {
		t.Fatal("ParseKind")
	}
}

func TestPickModel(t *testing.T) {
	ids := []string{"grok-4", "grok-4-fast-reasoning", "grok-4-fast-non-reasoning", "grok-2-image", "grok-imagine-video"}
	if got := PickModel(XAI, ids); got != "grok-4-fast-non-reasoning" {
		t.Fatalf("got %q", got)
	}
	if got := PickModel(Google, []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-preview-tts"}); got != "gemini-2.5-flash" {
		t.Fatalf("got %q", got)
	}
}

// Fake Codex and Grok CLIs check that LANBridge calls them the documented way.
func TestAccountCLIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell scripts")
	}
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	os.WriteFile(codex, []byte(`#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    --sandbox) [ "$2" = "read-only" ] || exit 3; shift ;;
  esac
  shift
done
cat > /dev/null
printf '{"say":"from codex","actions":["follow Max"]}' > "$out"
`), 0o755)
	grok := filepath.Join(dir, "grok")
	os.WriteFile(grok, []byte(`#!/bin/sh
case "$*" in *--prompt-file*--output-format\ plain*) ;; *) echo "bad args: $*" >&2; exit 2 ;; esac
printf '{"say":"from grok","actions":[]}'
`), 0o755)
	for _, c := range []struct {
		kind Kind
		path string
		want string
	}{{OpenAI, codex, "from codex"}, {XAI, grok, "from grok"}} {
		p, err := New(Config{Kind: c.kind, Auth: AuthAccount, Command: c.path})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := p.Generate(context.Background(), Request{System: "s", Prompt: "p"})
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		if !strings.Contains(resp.Text, c.want) {
			t.Fatalf("%s: got %q", c.kind, resp.Text)
		}
	}
}
