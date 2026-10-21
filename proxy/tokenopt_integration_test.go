package proxy

// Token Optimization, end to end through the real handlers. The external engine
// is a stub that speaks Headroom's /v1/compress contract (shapes read from its
// v0.41.0 source); the "provider" is a recording fake upstream. What these tests
// pin is what reaches the provider, and that nothing can break a request.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/tokenopt"
)

// --- Headroom stub ---

type headroomStub struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  int
	tokens []string // x-headroom-proxy-token seen on /v1/compress
	down   bool     // answer 500
}

func newHeadroomStub(t *testing.T) *headroomStub {
	t.Helper()
	s := &headroomStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"service": "headroom-proxy", "status": "healthy", "alive": true, "version": tokenopt.TestedHeadroomVersion})
	})
	mux.HandleFunc("/v1/compress", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.calls++
		s.tokens = append(s.tokens, r.Header.Get("x-headroom-proxy-token"))
		down := s.down
		s.mu.Unlock()
		if down {
			http.Error(w, "down", 500)
			return
		}
		var req struct {
			Messages []any `json:"messages"`
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "bad", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"messages": stubSqueeze(req.Messages), "tokens_before": 5000, "tokens_after": 500,
			"tokens_saved": 4500, "transforms_applied": []string{"stub"},
		})
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *headroomStub) callCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

func stubText(s string) string { return "[squeezed:" + itoa(len(s)) + "]" }

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// stubSqueeze shortens every tool result over 200 bytes, in Claude and OpenAI shapes.
func stubSqueeze(n any) any {
	switch v := n.(type) {
	case []any:
		for i := range v {
			v[i] = stubSqueeze(v[i])
		}
	case map[string]any:
		if v["type"] == "tool_result" || v["role"] == "tool" {
			switch c := v["content"].(type) {
			case string:
				if len(c) > 200 {
					v["content"] = stubText(c)
				}
			case []any:
				for _, p := range c {
					if pm, ok := p.(map[string]any); ok {
						if t, ok := pm["text"].(string); ok && len(t) > 200 {
							pm["text"] = stubText(t)
						}
					}
				}
			}
		}
		for k, e := range v {
			v[k] = stubSqueeze(e)
		}
	}
	return n
}

// --- fixtures ---

func bigResult(tag string) string { return strings.Repeat(tag+" output line <&>\n", 120) }

const anthropicOK = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
const openaiOK = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`

// claudeRequest carries fields the proxy does not model, so a typed round-trip
// anywhere in the path would visibly drop them.
func claudeRequest(model, system string, results ...string) []byte {
	msgs := []any{map[string]any{"role": "user", "content": "investigate"}}
	for i, res := range results {
		id := "toolu_" + itoa(i+1)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": res},
			}},
		)
	}
	m := map[string]any{
		"model": model, "max_tokens": 256, "messages": msgs,
		"stop_sequences": []string{"END"}, "top_k": 40,
		"metadata":           map[string]any{"user_id": "u1"},
		"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_tool_uses_20250919"}}},
	}
	if system != "" {
		m["system"] = system
	}
	b, _ := json.Marshal(m)
	return b
}

// setupTokenOpt wires one provider route and the Token Optimization settings.
func setupTokenOpt(t *testing.T, model, upstreamURL string, tok config.TokenOptimizationConfig) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	provider := config.UpstreamProvider{ID: "prov", Name: "provider", BaseURL: upstreamURL, ApiKey: "k", Enabled: true}
	route := config.ModelRoute{ID: "route", Model: model, Enabled: true,
		Targets: []config.RouteTarget{{UpstreamID: "prov", Priority: 0, Weight: 1, Enabled: true}}}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, []config.ModelRoute{route}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateTokenOptimization(tok); err != nil {
		t.Fatal(err)
	}
}

func headroomOn(stub *headroomStub, mut func(*config.TokenOptimizationConfig)) config.TokenOptimizationConfig {
	c := config.TokenOptimizationConfig{
		Mode: "on", InputCompression: "headroom",
		Headroom: config.HeadroomConfig{BaseURL: stub.server.URL, TimeoutMs: 1500, MinBytes: 256},
	}
	if mut != nil {
		mut(&c)
	}
	return c
}

func postJSON(h *Handler, path string, handle func(http.ResponseWriter, *http.Request), body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	handle(rec, r)
	return rec
}

func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return m
}

func toolResultText(t *testing.T, body []byte, msgIdx int) string {
	t.Helper()
	msgs := decodeJSON(t, body)["messages"].([]any)
	return msgs[msgIdx].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
}

// --- Claude, relayed to a provider ---

func TestClaudeRelayCompressesToolResultsAndKeepsTheEnvelope(t *testing.T) {
	stub := newHeadroomStub(t)
	up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
	setupTokenOpt(t, "claude-opt", up.server.URL, headroomOn(stub, nil))

	res1, res2 := bigResult("alpha"), bigResult("beta")
	body := claudeRequest("claude-opt", "You are helpful.", res1, res2)
	h := &Handler{}
	rec := postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	sent := up.requests()
	if len(sent) != 1 {
		t.Fatalf("upstream requests = %d", len(sent))
	}
	// Same request, two strings shorter.
	want := decodeJSON(t, body)
	msgs := want["messages"].([]any)
	msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = stubText(res1)
	msgs[4].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = stubText(res2)
	if got := decodeJSON(t, sent[0]); !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Fatalf("provider received more than the two shortened strings.\n got %s\nwant %s", gj, wj)
	}
	if len(sent[0]) >= len(body) {
		t.Fatalf("request did not shrink: %d -> %d", len(body), len(sent[0]))
	}
}

// With the feature off the provider receives the client's bytes, untouched.
func TestClaudeRelayIsByteIdenticalWhenOffOrObserving(t *testing.T) {
	for name, mut := range map[string]func(*config.TokenOptimizationConfig){
		"off": func(c *config.TokenOptimizationConfig) { c.Mode = "off" },
		"observe": func(c *config.TokenOptimizationConfig) {
			c.Mode = "observe"
			c.Caveman = "caveman"
			c.Ponytail = "full"
		},
		"rtk": func(c *config.TokenOptimizationConfig) { c.InputCompression = "rtk" },
	} {
		t.Run(name, func(t *testing.T) {
			stub := newHeadroomStub(t)
			up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
			setupTokenOpt(t, "claude-opt", up.server.URL, headroomOn(stub, mut))

			body := claudeRequest("claude-opt", "You are helpful.", bigResult("alpha"))
			h := &Handler{}
			if rec := postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, body); rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got := up.requests()[0]; !bytes.Equal(got, body) {
				t.Fatalf("provider did not receive the client's exact bytes:\n got %s\nwant %s", got, body)
			}
			if name == "observe" {
				// Observation happens off the request path; it may or may not have landed yet.
				deadline := time.Now().Add(2 * time.Second)
				for stub.callCount() == 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if stub.callCount() == 0 {
					t.Fatal("observe mode never consulted the engine")
				}
			} else if stub.callCount() != 0 {
				t.Fatalf("engine called %d times in mode %s", stub.callCount(), name)
			}
		})
	}
}

// The engine being down must cost the request nothing.
func TestClaudeRelayFailsOpenWhenTheEngineIsDown(t *testing.T) {
	stub := newHeadroomStub(t)
	stub.down = true
	up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
	setupTokenOpt(t, "claude-opt", up.server.URL, headroomOn(stub, nil))

	body := claudeRequest("claude-opt", "", bigResult("alpha"))
	h := &Handler{}
	rec := postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("request failed because the engine did: %d %s", rec.Code, rec.Body.String())
	}
	if got := up.requests()[0]; !bytes.Equal(got, body) {
		t.Fatalf("a failed engine must leave the body byte-identical")
	}
}

func TestClaudeRelayFailsOpenWhenTheEngineIsUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
	setupTokenOpt(t, "claude-opt", up.server.URL, config.TokenOptimizationConfig{
		Mode: "on", InputCompression: "headroom", Headroom: config.HeadroomConfig{BaseURL: url, TimeoutMs: 300},
	})
	body := claudeRequest("claude-opt", "", bigResult("alpha"))
	h := &Handler{}
	if rec := postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, body); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := up.requests()[0]; !bytes.Equal(got, body) {
		t.Fatalf("body changed although the engine was unreachable")
	}
}

// The same history on the next turn must produce the same bytes (the provider's
// prompt cache lives on that), and only the new result is sent to the engine.
func TestClaudeRelayKeepsThePrefixStableAcrossTurns(t *testing.T) {
	stub := newHeadroomStub(t)
	up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
	setupTokenOpt(t, "claude-opt", up.server.URL, headroomOn(stub, nil))
	h := &Handler{}

	r1, r2 := bigResult("alpha"), bigResult("beta")
	postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, claudeRequest("claude-opt", "", r1))
	postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, claudeRequest("claude-opt", "", r1, r2))
	postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, claudeRequest("claude-opt", "", r1, r2))

	sent := up.requests()
	if len(sent) != 3 {
		t.Fatalf("upstream requests = %d", len(sent))
	}
	if toolResultText(t, sent[0], 2) != toolResultText(t, sent[1], 2) {
		t.Fatal("turn 2 rewrote a result turn 1 had already sent")
	}
	if !bytes.Equal(sent[1], sent[2]) {
		t.Fatal("an unchanged history produced different bytes on a repeat")
	}
	if stub.callCount() != 2 {
		t.Fatalf("engine calls = %d, want 2 (turn 3 must replay from memory)", stub.callCount())
	}
}

func TestClaudeRelayAppendsCavemanAndPonytailAfterTheClientsSystemPrompt(t *testing.T) {
	up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
	setupTokenOpt(t, "claude-opt", up.server.URL, config.TokenOptimizationConfig{Mode: "on", Caveman: "caveman", Ponytail: "lite"})
	h := &Handler{}

	body := claudeRequest("claude-opt", "You are helpful.", bigResult("alpha"))
	if rec := postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, body); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	sys := decodeJSON(t, up.requests()[0])["system"].(string)
	if !strings.HasPrefix(sys, "You are helpful.\n\n") {
		t.Fatalf("client text must come first: %.60q", sys)
	}
	if !strings.Contains(sys, "PONYTAIL MODE ACTIVE — level: lite") || !strings.Contains(sys, "Respond terse like smart caveman") {
		t.Fatalf("official blocks missing: %q", sys)
	}
	if strings.Index(sys, "PONYTAIL MODE ACTIVE") > strings.Index(sys, "Respond terse like smart caveman") {
		t.Fatal("ponytail must precede caveman")
	}
	// Everything else is the client's request.
	got, want := decodeJSON(t, up.requests()[0]), decodeJSON(t, body)
	want["system"] = got["system"]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prompt injection changed more than system")
	}
}

func TestClaudeRelayDoesNotDuplicateWhatTheClientAlreadyHas(t *testing.T) {
	up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
	setupTokenOpt(t, "claude-opt", up.server.URL, config.TokenOptimizationConfig{Mode: "on", Caveman: "caveman", Ponytail: "lite"})
	h := &Handler{}

	body := claudeRequest("claude-opt", "Already set up.\nRespond terse like smart caveman. All technical substance stay.", bigResult("alpha"))
	postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, body)
	sys := decodeJSON(t, up.requests()[0])["system"].(string)
	if n := strings.Count(sys, "Respond terse like smart caveman"); n != 1 {
		t.Fatalf("caveman text appears %d times, want exactly the client's own", n)
	}
	if !strings.Contains(sys, "PONYTAIL MODE ACTIVE") {
		t.Fatalf("the part the client lacks should still be added")
	}
}

func TestClaudeRelayKeepsStructuredSystemBlocksAndTheirCacheMarkers(t *testing.T) {
	up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
	setupTokenOpt(t, "claude-opt", up.server.URL, config.TokenOptimizationConfig{Mode: "on", Ponytail: "full"})
	h := &Handler{}

	body := []byte(`{"model":"claude-opt","max_tokens":64,"system":[{"type":"text","text":"mine","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, body)
	blocks := decodeJSON(t, up.requests()[0])["system"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("system blocks = %d, want the client's plus one", len(blocks))
	}
	first := blocks[0].(map[string]any)
	if first["text"] != "mine" || first["cache_control"] == nil {
		t.Fatalf("client's block (and its cache breakpoint) disturbed: %v", first)
	}
	if blocks[1].(map[string]any)["cache_control"] != nil {
		t.Fatalf("the appended block must not carry a cache breakpoint of its own")
	}
}

// Streaming bodies are relayed untouched; only the request side changes.
func TestClaudeRelayStreamingResponseIsUntouched(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer up.Close()
	stub := newHeadroomStub(t)
	setupTokenOpt(t, "claude-opt", up.URL, headroomOn(stub, nil))

	reqBody := bytes.Replace(claudeRequest("claude-opt", "", bigResult("alpha")), []byte(`"max_tokens"`), []byte(`"stream":true,"max_tokens"`), 1)
	h := &Handler{}
	rec := postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, reqBody)
	if rec.Body.String() != sse {
		t.Fatalf("stream was altered:\n got %q\nwant %q", rec.Body.String(), sse)
	}
	if len(got) >= len(reqBody) {
		t.Fatalf("the request side was not compressed: %d -> %d", len(reqBody), len(got))
	}
}

// --- OpenAI chat and Responses, relayed ---

func TestOpenAIChatRelayCompressesAndInjects(t *testing.T) {
	stub := newHeadroomStub(t)
	up := newFakeUpstream(t, fakeUpstreamRound{body: openaiOK})
	setupTokenOpt(t, "gpt-opt", up.server.URL, headroomOn(stub, func(c *config.TokenOptimizationConfig) { c.Ponytail = "full" }))

	res := bigResult("o")
	reqBody, _ := json.Marshal(map[string]any{
		"model": "gpt-opt", "seed": 7, "logprobs": true,
		"messages": []any{
			map[string]any{"role": "system", "content": "mine"},
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "run", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": res},
		},
	})
	h := &Handler{}
	if rec := postJSON(h, "/v1/chat/completions", h.handleOpenAIChat, reqBody); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, up.requests()[0])
	msgs := got["messages"].([]any)
	if msgs[3].(map[string]any)["content"] != stubText(res) || msgs[3].(map[string]any)["tool_call_id"] != "call_1" {
		t.Fatalf("tool message wrong: %v", msgs[3])
	}
	if sys := msgs[0].(map[string]any)["content"].(string); !strings.HasPrefix(sys, "mine\n\n") || !strings.Contains(sys, "PONYTAIL MODE ACTIVE") {
		t.Fatalf("system message = %.80q", sys)
	}
	if got["seed"].(json.Number).String() != "7" || got["logprobs"] != true {
		t.Fatalf("unknown fields lost: %v", got)
	}
	if got["messages"].([]any)[2].(map[string]any)["tool_calls"] == nil {
		t.Fatalf("assistant tool_calls lost")
	}
}

func TestResponsesRelayCompressesAndKeepsContinuation(t *testing.T) {
	stub := newHeadroomStub(t)
	up := newFakeUpstream(t, fakeUpstreamRound{body: `{"id":"resp_2","object":"response","status":"completed","output":[]}`})
	setupTokenOpt(t, "resp-opt", up.server.URL, headroomOn(stub, func(c *config.TokenOptimizationConfig) { c.Caveman = "ultracave" }))

	res := bigResult("z")
	reqBody, _ := json.Marshal(map[string]any{
		"model": "resp-opt", "previous_response_id": "resp_1", "store": true, "instructions": "mine",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": res},
		},
	})
	h := &Handler{}
	if rec := postJSON(h, "/v1/responses", h.handleOpenAIResponses, reqBody); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, up.requests()[0])
	items := got["input"].([]any)
	if items[1].(map[string]any)["output"] != stubText(res) || items[1].(map[string]any)["call_id"] != "c1" {
		t.Fatalf("function_call_output wrong: %v", items[1])
	}
	if got["previous_response_id"] != "resp_1" || got["store"] != true {
		t.Fatalf("continuation fields lost: %v", got)
	}
	if ins := got["instructions"].(string); !strings.HasPrefix(ins, "mine\n\n") || !strings.Contains(ins, "# ultracave") {
		t.Fatalf("instructions = %.80q", ins)
	}
}

// --- Kiro-native translation ---

const claudeCodeStylePrompt = "You are Claude Code, Anthropic's official CLI for Claude.\n" +
	"You are an interactive agent that helps users with software engineering tasks.\n# Doing tasks\nDo them well."

func primingContent(p *KiroPayload) string {
	if len(p.ConversationState.History) == 0 || p.ConversationState.History[0].UserInputMessage == nil {
		return ""
	}
	return p.ConversationState.History[0].UserInputMessage.Content
}

func claudeUserReq(system string) *ClaudeRequest {
	return &ClaudeRequest{Model: "claude-sonnet-4", System: system, MaxTokens: 100,
		Messages: []ClaudeMessage{{Role: "user", Content: "hello there"}}}
}

// FilterClaudeCode replaces the whole system prompt with a compact one. The
// behavior block must survive that, which is why it is appended afterwards.
func TestKiroNativeBehaviorSurvivesThePromptFilter(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdatePromptFilterConfig(true, false, false, nil); err != nil {
		t.Fatal(err)
	}
	plain := ClaudeToKiro(claudeUserReq(claudeCodeStylePrompt), false)
	if primingContent(plain) != claudeCodeBackendPrompt {
		t.Fatalf("test premise: the filter should have replaced the prompt")
	}

	req := claudeUserReq(claudeCodeStylePrompt)
	req.behavior = tokenopt.ComposeBehavior("caveman", "full")
	got := ClaudeToKiro(req, false)
	p := primingContent(got)
	if !strings.HasPrefix(p, claudeCodeBackendPrompt+"\n\n") || !strings.Contains(p, "PONYTAIL MODE ACTIVE — level: full") || !strings.Contains(p, "Respond terse like smart caveman") {
		t.Fatalf("behavior block lost or misplaced: %.200q", p)
	}
	// Switching an optimization on must not fork the conversation.
	if got.ConversationState.ConversationID != plain.ConversationState.ConversationID {
		t.Fatalf("conversation id changed: %s vs %s", got.ConversationState.ConversationID, plain.ConversationState.ConversationID)
	}
	// And a request with no system prompt at all still gets one.
	bare := claudeUserReq("")
	bare.behavior = tokenopt.ComposeBehavior("caveman", "off")
	if c := primingContent(ClaudeToKiro(bare, false)); !strings.Contains(c, "Respond terse like smart caveman") {
		t.Fatalf("no system prompt: priming = %q", c)
	}
}

func TestKiroNativeBehaviorIsNotDuplicated(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	req := claudeUserReq("Mine.\nRespond terse like smart caveman. All technical substance stay.")
	req.behavior = tokenopt.ComposeBehavior("caveman", "lite")
	p := primingContent(ClaudeToKiro(req, false))
	if n := strings.Count(p, "Respond terse like smart caveman"); n != 1 {
		t.Fatalf("caveman text appears %d times", n)
	}
	if !strings.Contains(p, "PONYTAIL MODE ACTIVE — level: lite") {
		t.Fatalf("missing ponytail: %q", p)
	}
}

func TestKiroNativeOpenAIBehaviorAndIdentity(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	mk := func() *OpenAIRequest {
		return &OpenAIRequest{Model: "claude-sonnet-4", Messages: []OpenAIMessage{
			{Role: "system", Content: "mine"}, {Role: "user", Content: "hello there"}}}
	}
	plain := OpenAIToKiro(mk(), false)
	req := mk()
	req.behavior = tokenopt.ComposeBehavior("ultracave", "ultra")
	got := OpenAIToKiro(req, false)
	p := primingContent(got)
	if !strings.HasPrefix(p, "mine\n\n") || !strings.Contains(p, "PONYTAIL MODE ACTIVE — level: ultra") || !strings.Contains(p, "# ultracave") {
		t.Fatalf("priming = %.200q", p)
	}
	if got.ConversationState.ConversationID != plain.ConversationState.ConversationID {
		t.Fatalf("conversation id changed by the behavior block")
	}
}

// With nothing selected, what reaches Kiro is exactly what it was before.
func TestKiroNativeIsUnchangedWithoutBehavior(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	a := ClaudeToKiro(claudeUserReq(claudeCodeStylePrompt), true)
	b := func() *KiroPayload {
		r := claudeUserReq(claudeCodeStylePrompt)
		r.behavior = tokenopt.Behavior{}
		return ClaudeToKiro(r, true)
	}()
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	// AgentContinuationId is a fresh UUID per call; compare everything else.
	a.ConversationState.AgentContinuationId, b.ConversationState.AgentContinuationId = "", ""
	ab, _ = json.Marshal(a)
	bb, _ = json.Marshal(b)
	if !bytes.Equal(ab, bb) {
		t.Fatalf("an empty behavior changed the Kiro payload")
	}
	if behaviorTokens(tokenopt.Behavior{}) != 0 {
		t.Fatal("an empty behavior must add no tokens to the estimate")
	}
	if behaviorTokens(tokenopt.ComposeBehavior("caveman", "full")) <= 0 {
		t.Fatal("an injected behavior must be counted in the estimate")
	}
}

// Responses-to-Kiro assembles typed messages; they are compressed through a
// chat rendering and read back.
func TestCompressOpenAIMessagesRoundTrip(t *testing.T) {
	stub := newHeadroomStub(t)
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateTokenOptimization(headroomOn(stub, nil)); err != nil {
		t.Fatal(err)
	}
	res := bigResult("m")
	msgs := []OpenAIMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: nil, ToolCalls: []ToolCall{{ID: "c1", Type: "function"}}},
		{Role: "tool", ToolCallID: "c1", Content: res},
	}
	msgs[1].ToolCalls[0].Function.Name = "run"
	msgs[1].ToolCalls[0].Function.Arguments = "{}"

	run := (&Handler{}).tokenOptBegin()
	got := run.compressOpenAIMessages(context.Background(), msgs, "m")
	if len(got) != 3 || got[2].Content != stubText(res) || got[2].ToolCallID != "c1" || got[1].ToolCalls[0].ID != "c1" || got[1].ToolCalls[0].Function.Name != "run" {
		t.Fatalf("round trip lost structure: %+v", got)
	}
	if msgs[2].Content != res {
		t.Fatal("the caller's messages must not be modified in place")
	}
}

// --- concurrency ---

// Settings change while requests are in flight. Run with -race.
func TestTokenOptConcurrentSettingsAndRequests(t *testing.T) {
	stub := newHeadroomStub(t)
	up := newFakeUpstream(t, fakeUpstreamRound{body: anthropicOK})
	setupTokenOpt(t, "claude-opt", up.server.URL, headroomOn(stub, nil))
	h := &Handler{}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				body := claudeRequest("claude-opt", "sys", bigResult("t"+itoa(g)))
				if rec := postJSON(h, "/v1/messages", h.handleClaudeMessagesInternal, body); rec.Code != 200 {
					t.Errorf("status %d: %s", rec.Code, rec.Body.String())
					return
				}
			}
		}(g)
	}
	patcherDone := make(chan struct{})
	go func() {
		defer close(patcherDone)
		toggles := []func(*config.TokenOptimizationConfig){
			func(c *config.TokenOptimizationConfig) { c.Mode = "off" },
			func(c *config.TokenOptimizationConfig) { c.Mode = "observe" },
			func(c *config.TokenOptimizationConfig) { c.Mode = "on"; c.Caveman = "caveman" },
			func(c *config.TokenOptimizationConfig) { c.Headroom.Mode = "token" },
		}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				if err := config.PatchTokenOptimization(toggles[i%len(toggles)]); err != nil {
					t.Errorf("patch: %v", err)
					return
				}
				h.tokenOptSnapshot()
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-patcherDone // the patcher writes the config file; let it finish before the temp dir goes
}
