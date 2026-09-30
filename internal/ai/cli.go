package ai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ReplySchema is the JSON shape AI players answer with.
const ReplySchema = `{"type":"object","properties":{"say":{"type":"string"},"actions":{"type":"array","items":{"type":"string"}}},"required":["say","actions"],"additionalProperties":false}`

// codexCLI runs OpenAI's Codex CLI, which bills the user's ChatGPT plan when
// they signed in with "codex login".
type codexCLI struct {
	path   string
	model  string
	simple atomic.Bool // this Codex version doesn't know the newer flags
}

func (c *codexCLI) Describe() string {
	m := c.model
	if m == "" {
		m = "Codex default model"
	}
	return "GPT (" + m + ", ChatGPT plan via Codex CLI)"
}

func (c *codexCLI) Generate(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	dir, err := os.MkdirTemp("", "lanbridge-codex-")
	if err != nil {
		return Response{}, err
	}
	defer os.RemoveAll(dir)
	schema, out := filepath.Join(dir, "schema.json"), filepath.Join(dir, "reply.txt")
	if err := os.WriteFile(schema, []byte(ReplySchema), 0o600); err != nil {
		return Response{}, err
	}
	run := func(simple bool) (string, error) {
		args := []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only", "--color", "never", "-o", out}
		if !simple {
			args = append(args, "--ephemeral", "--ignore-user-config", "--ignore-rules", "--cd", dir, "--output-schema", schema)
		}
		if c.model != "" {
			args = append(args, "--model", c.model)
		}
		args = append(args, "-")
		return runCLI(ctx, c.path, dir, args, req.System+"\n\n"+req.Prompt, out)
	}
	text, err := run(c.simple.Load())
	if err != nil && !c.simple.Load() && looksLikeFlagError(err) {
		c.simple.Store(true)
		text, err = run(true)
	}
	if err != nil {
		return Response{}, explainCLI("codex", "codex login", err)
	}
	return Response{Text: text, Model: firstNonEmpty(c.model, "codex default"), Took: time.Since(start)}, nil
}

// grokCLI runs xAI's Grok Build CLI, which bills the user's SuperGrok or X
// Premium+ plan when they signed in with "grok login".
type grokCLI struct {
	path   string
	model  string
	simple atomic.Bool
}

func (g *grokCLI) Describe() string {
	m := g.model
	if m == "" {
		m = "Grok Build default model"
	}
	return "Grok (" + m + ", SuperGrok plan via Grok Build CLI)"
}

func (g *grokCLI) Generate(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	dir, err := os.MkdirTemp("", "lanbridge-grok-")
	if err != nil {
		return Response{}, err
	}
	defer os.RemoveAll(dir)
	prompt := req.System + "\n\n" + req.Prompt
	pf := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(pf, []byte(prompt), 0o600); err != nil {
		return Response{}, err
	}
	run := func(simple bool) (string, error) {
		var args []string
		if simple {
			args = []string{"-p", prompt}
		} else {
			args = []string{"--prompt-file", pf, "--output-format", "plain", "--cwd", dir, "--max-turns", "2", "--tools", "read_file", "--no-auto-update"}
		}
		if g.model != "" {
			args = append(args, "--model", g.model)
		}
		return runCLI(ctx, g.path, dir, args, "", "")
	}
	text, err := run(g.simple.Load())
	if err != nil && !g.simple.Load() && looksLikeFlagError(err) {
		g.simple.Store(true)
		text, err = run(true)
	}
	if err != nil {
		return Response{}, explainCLI("grok", "grok login", err)
	}
	return Response{Text: text, Model: firstNonEmpty(g.model, "grok default"), Took: time.Since(start)}, nil
}

type cliError struct {
	err    error
	stderr string
}

func (e *cliError) Error() string {
	s := strings.TrimSpace(e.stderr)
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	if s == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + s
}

// runCLI runs a tool in an empty scratch folder and returns its answer, read
// from outFile if given, else from stdout.
func runCLI(ctx context.Context, path, dir string, args []string, stdin, outFile string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "CI=1")
	err := cmd.Run()
	text := stdout.String()
	if outFile != "" {
		if b, rerr := os.ReadFile(outFile); rerr == nil {
			text = string(b)
		}
	}
	if strings.TrimSpace(text) != "" && err == nil {
		return strings.TrimSpace(text), nil
	}
	if err == nil {
		err = errors.New("no answer")
	}
	if ctx.Err() != nil {
		err = errors.New("timed out")
	}
	return "", &cliError{err: err, stderr: stderr.String() + "\n" + stdout.String()}
}

func looksLikeFlagError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unexpected argument") || strings.Contains(s, "unknown option") ||
		strings.Contains(s, "unrecognized") || strings.Contains(s, "unknown flag") || strings.Contains(s, "found argument")
}

func explainCLI(name, login string, err error) error {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "login") || strings.Contains(s, "log in") || strings.Contains(s, "not authenticated") ||
		strings.Contains(s, "unauthorized") || strings.Contains(s, "401") || strings.Contains(s, "sign in"):
		return fmt.Errorf("%s isn't signed in; run \"%s\" in a terminal first", name, login)
	case strings.Contains(s, "usage limit") || strings.Contains(s, "rate limit") || strings.Contains(s, "429") || strings.Contains(s, "quota"):
		return fmt.Errorf("your plan's usage limit was reached (%s)", name)
	}
	return fmt.Errorf("%s failed: %v", name, err)
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
