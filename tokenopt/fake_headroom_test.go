package tokenopt

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHeadroom stands in for `headroom proxy`. Its request and reply shapes are
// the ones read from Headroom v0.41.0's source (headroom/proxy/handlers/openai.py
// handle_compress and headroom/proxy/server.py), not invented: the request is
// {"model","messages","config"} and the reply is {"messages","tokens_before",
// "tokens_after","tokens_saved","transforms_applied"}.
type fakeHeadroom struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	requests []compressCall

	// compress rewrites the messages; nil means "compress every large tool result".
	compress func(msgs []any) []any
	// tokensSaved is reported in the reply.
	tokensSaved int
	// status, when non-zero, is returned instead of a compress result.
	status int
	// raw, when non-empty, is written verbatim as the 200 body.
	raw string
	// delay is slept before replying.
	delay time.Duration
	// token, when set, is required in x-headroom-proxy-token.
	token string
	// version is reported by /livez.
	version string
}

type compressCall struct {
	Model    string
	Messages []any
	Config   map[string]any
	Token    string
}

func newFakeHeadroom(t *testing.T) *fakeHeadroom {
	t.Helper()
	f := &fakeHeadroom{t: t, version: TestedHeadroomVersion, tokensSaved: 500}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeHeadroom) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/livez":
		json.NewEncoder(w).Encode(map[string]any{"service": "headroom-proxy", "status": "healthy", "alive": true, "version": f.version})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/compress":
		f.compressHandler(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeHeadroom) compressHandler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model    string         `json:"model"`
		Messages []any          `json:"messages"`
		Config   map[string]any `json:"config"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, compressCall{req.Model, req.Messages, req.Config, r.Header.Get("x-headroom-proxy-token")})
	status, raw, delay, token := f.status, f.raw, f.delay, f.token
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if token != "" && r.Header.Get("x-headroom-proxy-token") != token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if status != 0 {
		http.Error(w, "boom", status)
		return
	}
	if raw != "" {
		io.WriteString(w, raw)
		return
	}
	out := req.Messages
	if f.compress != nil {
		out = f.compress(deepCopy(req.Messages).([]any))
	} else {
		out = compressLargeToolResults(deepCopy(req.Messages).([]any))
	}
	json.NewEncoder(w).Encode(map[string]any{
		"messages": out, "tokens_before": 10000, "tokens_after": 10000 - f.tokensSaved,
		"tokens_saved": f.tokensSaved, "transforms_applied": []string{"fake"},
	})
}

func (f *fakeHeadroom) calls() []compressCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]compressCall(nil), f.requests...)
}

func (f *fakeHeadroom) callCount() int { return len(f.calls()) }

func deepCopy(v any) any {
	b, _ := json.Marshal(v)
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	var out any
	d.Decode(&out)
	return out
}

// squeeze is what the fake "engine" turns any big text into: shorter, and a pure
// function of the input.
func squeeze(s string) string { return fmt.Sprintf("[squeezed %d bytes]", len(s)) }

// compressLargeToolResults squeezes the text of every tool result over 200 bytes,
// in Claude (tool_result blocks) and OpenAI (role=tool) shapes alike.
func compressLargeToolResults(msgs []any) []any {
	var walk func(n any)
	squeezeContent := func(parent map[string]any) {
		switch c := parent["content"].(type) {
		case string:
			if len(c) > 200 {
				parent["content"] = squeeze(c)
			}
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok && len(t) > 200 {
						pm["text"] = squeeze(t)
					}
				}
			}
		}
	}
	walk = func(n any) {
		switch v := n.(type) {
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			if v["type"] == "tool_result" || v["role"] == "tool" {
				squeezeContent(v)
			}
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(msgs)
	return msgs
}

// --- request builders ---

func big(tag string, n int) string { return strings.Repeat(tag+" payload line <&> \n", n) }

type claudeTurn struct {
	tool    string
	id      string
	result  string
	isError bool
}

// claudeBody builds a Claude request carrying fields the proxy does not model, so
// tests can prove they survive.
func claudeBody(turns ...claudeTurn) []byte {
	msgs := []any{map[string]any{"role": "user", "content": "please investigate"}}
	for _, t := range turns {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "running " + t.tool},
				map[string]any{"type": "tool_use", "id": t.id, "name": t.tool, "input": map[string]any{"command": "ls", "n": 1.0}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": t.id, "content": t.result, "is_error": t.isError,
					"cache_control": map[string]any{"type": "ephemeral"}},
			}},
		)
	}
	b, _ := json.Marshal(map[string]any{
		"model": "claude-test", "max_tokens": 1024, "stream": true,
		"system":             "You are helpful.",
		"messages":           msgs,
		"stop_sequences":     []string{"END"},
		"top_k":              40,
		"metadata":           map[string]any{"user_id": "u1", "extra": map[string]any{"deep": []int{1, 2, 3}}},
		"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_tool_uses_20250919"}}},
	})
	return b
}

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return m
}
