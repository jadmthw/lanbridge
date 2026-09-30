package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// BaseURLs are the API endpoints; tests point them at fake servers.
var BaseURLs = map[Kind]string{
	OpenAI:    "https://api.openai.com/v1",
	Anthropic: "https://api.anthropic.com/v1",
	XAI:       "https://api.x.ai/v1",
	Google:    "https://generativelanguage.googleapis.com/v1beta",
}

var httpClient = &http.Client{Timeout: 90 * time.Second}

type httpProvider struct {
	kind  Kind
	key   string
	model string

	mu       sync.Mutex
	resolved string
}

func newHTTP(c Config) *httpProvider {
	return &httpProvider{kind: c.Kind, key: strings.TrimSpace(c.APIKey), model: strings.TrimSpace(c.Model)}
}

func (p *httpProvider) Describe() string {
	info, _ := InfoFor(p.kind)
	m := p.model
	if m == "" {
		m = info.DefaultModel
	}
	if m == "" {
		m = "newest available model"
	}
	return info.Name + " (" + m + ", API key)"
}

// modelName picks the configured model, the default, or the best one the account offers.
func (p *httpProvider) modelName(ctx context.Context) (string, error) {
	if p.model != "" {
		return strings.TrimPrefix(p.model, "models/"), nil
	}
	if info, _ := InfoFor(p.kind); info.DefaultModel != "" {
		return info.DefaultModel, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolved != "" {
		return p.resolved, nil
	}
	ids, err := listModels(ctx, p.kind, p.key)
	if err != nil {
		return "", err
	}
	if m := PickModel(p.kind, ids); m != "" {
		p.resolved = m
		return m, nil
	}
	return "", errors.New("your account doesn't list any chat models")
}

func (p *httpProvider) Generate(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	model, err := p.modelName(ctx)
	if err != nil {
		return Response{}, err
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1500
	}
	base := BaseURLs[p.kind]
	var (
		u    string
		body any
		hdr  = http.Header{"Content-Type": {"application/json"}}
	)
	switch p.kind {
	case OpenAI, XAI:
		u = base + "/chat/completions"
		hdr.Set("Authorization", "Bearer "+p.key)
		m := map[string]any{"model": model, "messages": []map[string]string{{"role": "system", "content": req.System}, {"role": "user", "content": req.Prompt}}}
		if p.kind == OpenAI {
			m["max_completion_tokens"] = req.MaxTokens
			m["response_format"] = map[string]string{"type": "json_object"}
		} else {
			m["max_tokens"] = req.MaxTokens
		}
		body = m
	case Anthropic:
		u = base + "/messages"
		hdr.Set("x-api-key", p.key)
		hdr.Set("anthropic-version", "2023-06-01")
		body = map[string]any{"model": model, "max_tokens": req.MaxTokens, "system": req.System,
			"messages": []map[string]string{{"role": "user", "content": req.Prompt}}}
	case Google:
		u = base + "/models/" + url.PathEscape(model) + ":generateContent"
		hdr.Set("x-goog-api-key", p.key)
		body = map[string]any{
			"systemInstruction": map[string]any{"parts": []map[string]string{{"text": req.System}}},
			"contents":          []map[string]any{{"role": "user", "parts": []map[string]string{{"text": req.Prompt}}}},
			"generationConfig":  map[string]any{"maxOutputTokens": req.MaxTokens, "responseMimeType": "application/json"},
		}
	}
	data, err := doJSON(ctx, http.MethodPost, u, hdr, body)
	if err != nil {
		return Response{}, err
	}
	resp := Response{Model: model, Took: time.Since(start)}
	switch p.kind {
	case OpenAI, XAI:
		var r struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
					Refusal string `json:"refusal"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &r); err != nil || len(r.Choices) == 0 {
			return Response{}, errors.New("unexpected answer from the API")
		}
		resp.Text = r.Choices[0].Message.Content
		if resp.Text == "" {
			resp.Text = r.Choices[0].Message.Refusal
		}
		resp.InputTokens, resp.OutputTokens = r.Usage.Prompt, r.Usage.Completion
	case Anthropic:
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				In  int `json:"input_tokens"`
				Out int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return Response{}, errors.New("unexpected answer from the API")
		}
		for _, c := range r.Content {
			if c.Type == "text" {
				resp.Text += c.Text
			}
		}
		resp.InputTokens, resp.OutputTokens = r.Usage.In, r.Usage.Out
	case Google:
		var r struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text    string `json:"text"`
						Thought bool   `json:"thought"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			Usage struct {
				In  int `json:"promptTokenCount"`
				Out int `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return Response{}, errors.New("unexpected answer from the API")
		}
		if len(r.Candidates) == 0 {
			return Response{}, errors.New("Gemini returned no answer (it may have been blocked by a safety filter)")
		}
		for _, part := range r.Candidates[0].Content.Parts {
			if !part.Thought {
				resp.Text += part.Text
			}
		}
		resp.InputTokens, resp.OutputTokens = r.Usage.In, r.Usage.Out
	}
	if strings.TrimSpace(resp.Text) == "" {
		return Response{}, errors.New("the model returned an empty answer")
	}
	return resp, nil
}

// APIError is an error answer from a provider's API.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	switch {
	case e.Status == 401 || e.Status == 403:
		return "the API key was rejected (" + e.Msg + ")"
	case e.Status == 429:
		return "rate limited or out of credits (" + e.Msg + ")"
	case e.Status == 404:
		return "model not found (" + e.Msg + ")"
	}
	return fmt.Sprintf("API error %d: %s", e.Status, e.Msg)
}

func doJSON(ctx context.Context, method, u string, hdr http.Header, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header = hdr
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Error json.RawMessage `json:"error"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && len(e.Error) > 0 {
			var inner struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(e.Error, &inner) == nil && inner.Message != "" {
				msg = inner.Message
			} else {
				var s string
				if json.Unmarshal(e.Error, &s) == nil && s != "" {
					msg = s
				}
			}
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &APIError{Status: res.StatusCode, Msg: msg}
	}
	return data, nil
}

// ListModels returns the model IDs an API key can use.
func ListModels(ctx context.Context, c Config) ([]string, error) {
	if c.Auth == AuthAccount {
		return nil, nil
	}
	return listModels(ctx, c.Kind, strings.TrimSpace(c.APIKey))
}

func listModels(ctx context.Context, kind Kind, key string) ([]string, error) {
	hdr := http.Header{}
	switch kind {
	case OpenAI, XAI:
		hdr.Set("Authorization", "Bearer "+key)
	case Anthropic:
		hdr.Set("x-api-key", key)
		hdr.Set("anthropic-version", "2023-06-01")
	case Google:
		hdr.Set("x-goog-api-key", key)
	}
	u := BaseURLs[kind] + "/models"
	if kind == Google {
		u += "?pageSize=200"
	} else if kind == Anthropic {
		u += "?limit=100"
	}
	data, err := doJSON(ctx, http.MethodGet, u, hdr, nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range r.Data {
		ids = append(ids, d.ID)
	}
	for _, m := range r.Models {
		for _, meth := range m.Methods {
			if meth == "generateContent" {
				ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
				break
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// PickModel chooses a sensible chat model from a list: fast and cheap, since
// players chat a lot and replies are short.
func PickModel(kind Kind, ids []string) string {
	prefix := map[Kind]string{OpenAI: "gpt-", Anthropic: "claude-", XAI: "grok-", Google: "gemini-"}[kind]
	skip := []string{"image", "vision", "imagine", "video", "audio", "tts", "realtime", "embed", "transcribe", "search", "codex", "live", "preview", "exp"}
	best, bestScore := "", -1
	for _, id := range ids {
		l := strings.ToLower(id)
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		bad := false
		for _, s := range skip {
			if strings.Contains(l, s) {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		score := 1
		for _, fast := range []string{"mini", "fast", "flash", "haiku"} {
			if strings.Contains(l, fast) {
				score += 10
			}
		}
		if strings.Contains(l, "non-reasoning") {
			score += 5
		}
		if score > bestScore || (score == bestScore && l > strings.ToLower(best)) {
			best, bestScore = id, score
		}
	}
	return best
}
