package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// This file provides the shared scaffolding for end-to-end usage-accounting
// CHARACTERIZATION tests. Every test that uses it exercises a real HTTP handler
// against a fake Kiro backend (or an injected runner) and captures ALL usage
// sinks at once, so a later Phase-A change can be proven to move ONLY the sinks
// it is supposed to move.
//
// These are characterization tests: they assert the behavior that exists at the
// current source SHA, INCLUDING the known bug where context occupancy overrides
// the real upstream input token count. When Phase A lands, the expectations for
// the internal-accounting sinks change on purpose; the payload / credit /
// response / SSE-order invariants must NOT.
//
// No production code is modified. No live Kiro call is made. No real credential
// is used.

// kiroFrame is one scripted AWS event-stream frame the fake backend emits.
type kiroFrame struct {
	eventType string
	payload   map[string]interface{}
}

// fakeKiroBackend is an httptest server that emits a scripted event stream and
// records every request body it received (for payload-equivalence assertions).
type fakeKiroBackend struct {
	server     *httptest.Server
	mu         sync.Mutex
	bodies     [][]byte
	callCount  int
	frames     []kiroFrame
	truncate   bool // when true, write frames then abruptly close without clean EOF
	statusCode int
}

// newFakeKiroBackend builds a fake backend emitting the given frames on 200 OK.
func newFakeKiroBackend(t *testing.T, frames ...kiroFrame) *fakeKiroBackend {
	t.Helper()
	fb := &fakeKiroBackend{frames: frames, statusCode: 200}
	fb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllBody(r)
		fb.mu.Lock()
		fb.bodies = append(fb.bodies, body)
		fb.callCount++
		fb.mu.Unlock()

		if fb.statusCode != 200 {
			http.Error(w, "scripted upstream error", fb.statusCode)
			return
		}
		w.WriteHeader(http.StatusOK)
		for _, f := range fb.frames {
			_, _ = w.Write(awsEventStreamFrame(t, f.eventType, f.payload))
		}
		// A truncated stream is simulated by NOT writing a clean trailing frame;
		// the httptest server closes the connection when this handler returns.
		// parseEventStream sees the frames it got, then EOF.
	}))
	t.Cleanup(fb.server.Close)
	return fb
}

func readAllBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

func (fb *fakeKiroBackend) capturedBodies() [][]byte {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := make([][]byte, len(fb.bodies))
	copy(out, fb.bodies)
	return out
}

func (fb *fakeKiroBackend) calls() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.callCount
}

// UsageIntegrationResult is the full snapshot of every usage sink for one
// logical request, captured as before→after deltas so accumulation across a
// singleton pool is irrelevant.
type UsageIntegrationResult struct {
	// Client-visible usage (parsed from the actual response).
	ClientInputTokens  int
	ClientOutputTokens int
	ClientCacheCreate  int
	ClientCacheRead    int
	ClientText         string
	ClientStopReason   string
	ClientToolCalls    int

	// Per-API-key accounting deltas.
	APIKeyTokensDelta   int64
	APIKeyCreditsDelta  float64
	APIKeyRequestsDelta int64

	// Per-account accounting deltas.
	AccountTokensDelta  int64
	AccountCreditsDelta float64
	AccountRequestDelta int

	// Global handler counter deltas.
	GlobalTokensDelta   int64
	GlobalCreditsDelta  float64
	GlobalRequestsDelta int64
	GlobalSuccessDelta  int64
	GlobalFailedDelta   int64

	// Upstream / orchestration observations.
	UpstreamCalls    int
	UpstreamPayloads [][]byte

	// SSE (streaming only): ordered event names, plus parsed helpers.
	SSEEvents      []string
	HTTPStatus     int
	RawBody        string
	StreamingUsage []map[string]interface{}
}

// statsSnapshot captures the three cumulative sinks at one instant.
type statsSnapshot struct {
	apiKeyTokens   int64
	apiKeyCredits  float64
	apiKeyRequests int64
	acctTokens     int
	acctCredits    float64
	acctRequests   int
	globalTokens   int64
	globalCredits  float64
	globalReqs     int64
	globalSuccess  int64
	globalFailed   int64
}

func snapshotStats(h *Handler, accountID, apiKeyID string) statsSnapshot {
	var s statsSnapshot
	if e := config.GetApiKeyEntry(apiKeyID); e != nil {
		s.apiKeyTokens = e.TokensUsed
		s.apiKeyCredits = e.CreditsUsed
		s.apiKeyRequests = e.RequestsCount
	}
	if a := h.pool.GetByID(accountID); a != nil {
		s.acctTokens = a.TotalTokens
		s.acctCredits = a.TotalCredits
		s.acctRequests = a.RequestCount
	}
	s.globalTokens = atomicLoad(&h.totalTokens)
	s.globalCredits = h.getCredits()
	s.globalReqs = atomicLoad(&h.totalRequests)
	s.globalSuccess = atomicLoad(&h.successRequests)
	s.globalFailed = atomicLoad(&h.failedRequests)
	return s
}

// integrationEnv is a fully isolated handler + config + pool for one test.
type integrationEnv struct {
	h         *Handler
	accountID string
	apiKeyID  string
	apiKey    string
}

// newIntegrationEnv sets up an isolated config (TempDir), one enabled account,
// one enabled API key (auth required), and a fresh Handler. Endpoint/httpstore
// swapping is the caller's job (via swapKiroEndpointsForTest) for direct paths,
// or runner injection for web_search paths.
func newIntegrationEnv(t *testing.T, accountID, apiKeyValue string, runner ConversationRunner) *integrationEnv {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          accountID,
		Enabled:     true,
		AccessToken: "token-" + accountID,
		ProfileArn:  "arn:aws:codewhisperer:profile/" + accountID,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	// Enable real auth so the per-key context plumbing is exercised end-to-end.
	requireKey := true
	if err := config.UpdateSettingsPatch(nil, &requireKey, ""); err != nil {
		t.Fatalf("enable auth: %v", err)
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Key:     apiKeyValue,
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:               p,
		promptCache:        newPromptCacheTracker(defaultPromptCacheTTL),
		conversationRunner: runner,
	}
	return &integrationEnv{h: h, accountID: accountID, apiKeyID: entry.ID, apiKey: apiKeyValue}
}

// serveHTTP drives a request through the real ServeHTTP stack (auth included),
// capturing all sinks as deltas. Used for the 6 direct (non-runner) paths.
func (env *integrationEnv) serveHTTP(t *testing.T, fb *fakeKiroBackend, method, path, body string) UsageIntegrationResult {
	t.Helper()
	before := snapshotStats(env.h, env.accountID, env.apiKeyID)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)
	rec := httptest.NewRecorder()
	env.h.ServeHTTP(rec, req)

	after := snapshotStats(env.h, env.accountID, env.apiKeyID)
	res := diffStats(before, after)
	res.HTTPStatus = rec.Code
	res.RawBody = rec.Body.String()
	if fb != nil {
		res.UpstreamCalls = fb.calls()
		res.UpstreamPayloads = fb.capturedBodies()
	}
	return res
}

func diffStats(before, after statsSnapshot) UsageIntegrationResult {
	return UsageIntegrationResult{
		APIKeyTokensDelta:   after.apiKeyTokens - before.apiKeyTokens,
		APIKeyCreditsDelta:  after.apiKeyCredits - before.apiKeyCredits,
		APIKeyRequestsDelta: after.apiKeyRequests - before.apiKeyRequests,
		AccountTokensDelta:  int64(after.acctTokens - before.acctTokens),
		AccountCreditsDelta: after.acctCredits - before.acctCredits,
		AccountRequestDelta: after.acctRequests - before.acctRequests,
		GlobalTokensDelta:   after.globalTokens - before.globalTokens,
		GlobalCreditsDelta:  after.globalCredits - before.globalCredits,
		GlobalRequestsDelta: after.globalReqs - before.globalReqs,
		GlobalSuccessDelta:  after.globalSuccess - before.globalSuccess,
		GlobalFailedDelta:   after.globalFailed - before.globalFailed,
	}
}

// atomicLoad reads an int64 counter. The handler uses sync/atomic on these; a
// plain load under the sequential (non-parallel) test regime is race-free
// because ServeHTTP has fully returned before we snapshot.
func atomicLoad(p *int64) int64 { return *p }

// --- response parsing helpers ------------------------------------------------

func (r *UsageIntegrationResult) parseClaudeJSON(t *testing.T) {
	t.Helper()
	var resp ClaudeResponse
	if err := json.Unmarshal([]byte(r.RawBody), &resp); err != nil {
		t.Fatalf("decode claude json: %v; body=%s", err, r.RawBody)
	}
	r.ClientInputTokens = resp.Usage.InputTokens
	r.ClientOutputTokens = resp.Usage.OutputTokens
	r.ClientCacheCreate = resp.Usage.CacheCreationInputTokens
	r.ClientCacheRead = resp.Usage.CacheReadInputTokens
	r.ClientStopReason = resp.StopReason
	var text strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
		if b.Type == "tool_use" {
			r.ClientToolCalls++
		}
	}
	r.ClientText = text.String()
}

func (r *UsageIntegrationResult) parseOpenAIJSON(t *testing.T) {
	t.Helper()
	var resp OpenAIResponse
	if err := json.Unmarshal([]byte(r.RawBody), &resp); err != nil {
		t.Fatalf("decode openai json: %v; body=%s", err, r.RawBody)
	}
	r.ClientInputTokens = resp.Usage.PromptTokens
	r.ClientOutputTokens = resp.Usage.CompletionTokens
	if len(resp.Choices) > 0 {
		r.ClientText = openAIMessageText(resp.Choices[0].Message.Content)
		r.ClientStopReason = resp.Choices[0].FinishReason
		r.ClientToolCalls = len(resp.Choices[0].Message.ToolCalls)
	}
}

func (r *UsageIntegrationResult) parseResponsesJSON(t *testing.T) {
	t.Helper()
	var resp ResponsesObject
	if err := json.Unmarshal([]byte(r.RawBody), &resp); err != nil {
		t.Fatalf("decode responses json: %v; body=%s", err, r.RawBody)
	}
	r.ClientInputTokens = resp.Usage.InputTokens
	r.ClientOutputTokens = resp.Usage.OutputTokens
	var text strings.Builder
	for _, item := range resp.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
		if item.Type == "function_call" {
			r.ClientToolCalls++
		}
	}
	r.ClientText = text.String()
	r.ClientStopReason = resp.Status
}

func openAIMessageText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	default:
		return ""
	}
}

// parseSSE extracts ordered event names and any per-event usage maps from a raw
// SSE body. It reuses the same framing the handlers emit ("event: X\ndata: Y").
func (r *UsageIntegrationResult) parseSSE(t *testing.T) {
	t.Helper()
	for _, block := range strings.Split(r.RawBody, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var evName, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				evName = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if evName == "" && data == "" {
			continue
		}
		if evName != "" {
			r.SSEEvents = append(r.SSEEvents, evName)
		}
		if data != "" {
			var m map[string]interface{}
			if json.Unmarshal([]byte(data), &m) == nil {
				if u, ok := m["usage"].(map[string]interface{}); ok {
					r.StreamingUsage = append(r.StreamingUsage, u)
				}
			}
		}
	}
}

// semanticPayloadHash builds a stable structural fingerprint of a captured Kiro
// request body, excluding only genuinely nondeterministic fields
// (AgentContinuationId). It NEVER strips system prompt, history, tools, model,
// or profile — those are the invariants a Phase-A accounting fix must preserve.
// Returns the canonicalized JSON string (raw prompt text is part of the payload
// but the test never logs it; callers compare hashes, not contents).
func semanticPayloadHash(t *testing.T, body []byte) string {
	t.Helper()
	var v map[string]interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	stripNondeterministic(v)
	canon, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal payload: %v", err)
	}
	return string(canon)
}

func stripNondeterministic(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		delete(t, "agentContinuationId")
		delete(t, "conversationId")
		for _, child := range t {
			stripNondeterministic(child)
		}
	case []interface{}:
		for _, child := range t {
			stripNondeterministic(child)
		}
	}
}

// mustContext returns a context for handler calls that need one directly.
func mustContext() context.Context { return context.Background() }

// shortTimeout is used by the fake http client swap for direct paths.
var _ = time.Second

// --- per-path streaming usage extraction ------------------------------------
//
// Each API streams its final usage in a different shape:
//   - Claude: a `message_delta` event whose top-level `usage` carries input_tokens
//   - OpenAI: a `data:` chunk (no event name) with top-level `usage.prompt_tokens`
//   - Responses: a `response.completed` event whose usage is NESTED under
//     `response.usage.input_tokens` (so parseSSE's top-level capture misses it)
//
// These helpers require parseSSE to have run first (they read StreamingUsage),
// except the Responses one, which re-scans RawBody for the nested field.

// lastClaudeStreamUsageInput returns the input_tokens from the final streaming
// usage map that carried one (Claude's message_delta).
func lastClaudeStreamUsageInput(t *testing.T, res UsageIntegrationResult) int {
	t.Helper()
	got, ok := lastUsageIntField(res.StreamingUsage, "input_tokens")
	if !ok {
		t.Fatalf("no streaming usage with input_tokens found; body=%s", res.RawBody)
	}
	return got
}

// openAIStreamUsageInput returns prompt_tokens from the final OpenAI chunk usage.
func openAIStreamUsageInput(t *testing.T, res UsageIntegrationResult) int {
	t.Helper()
	got, ok := lastUsageIntField(res.StreamingUsage, "prompt_tokens")
	if !ok {
		t.Fatalf("no streaming usage with prompt_tokens found; body=%s", res.RawBody)
	}
	return got
}

// responsesStreamUsageInput scans the raw SSE body for the response.completed
// event and returns response.usage.input_tokens (nested, so parseSSE misses it).
func responsesStreamUsageInput(t *testing.T, res UsageIntegrationResult) int {
	t.Helper()
	for _, block := range strings.Split(res.RawBody, "\n\n") {
		block = strings.TrimSpace(block)
		if !strings.Contains(block, "response.completed") {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m struct {
				Response struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
				return m.Response.Usage.InputTokens
			}
		}
	}
	t.Fatalf("no response.completed usage found; body=%s", res.RawBody)
	return 0
}

// lastUsageIntField returns the value of the named field from the LAST usage map
// that contains it (streams may emit a zero-usage frame before the final one).
func lastUsageIntField(usages []map[string]interface{}, field string) (int, bool) {
	found := false
	val := 0
	for _, u := range usages {
		if raw, ok := u[field]; ok {
			if f, ok := raw.(float64); ok {
				val = int(f)
				found = true
			}
		}
	}
	return val, found
}
