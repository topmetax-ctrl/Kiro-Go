package proxy

// End-to-end coverage of the provider-local web_search strategy, exercised over
// real HTTP against a fake Anthropic-compatible upstream and a fake SearXNG.
//
// This is the hermetic half of the live acceptance test. It proves the whole
// chain without touching a real provider:
//
//	client request (native web_search tool)
//	  -> route resolves to provider X, strategy=local
//	  -> round 1 to X, carrying a synthetic ORDINARY web_search client tool
//	  -> X returns tool_use(query)
//	  -> SearchOrchestrator -> SearXNG executes for real (over HTTP)
//	  -> round 2 to the SAME X, carrying tool_result for the SAME tool_use id
//	  -> X returns the final answer
//	  -> client sees server_tool_use + web_search_tool_result + usage counter
//
// The Kiro-independence requirement is structural here, not asserted after the
// fact: no Kiro account is configured in any of these tests, and no Kiro
// credential exists to be used. A path that reached the pool or the MCP endpoint
// would fail outright rather than pass quietly.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

// fakeSearxng serves SearXNG's JSON search API shape and records the queries it
// was asked for, so a test can prove a real backend call happened.
type fakeSearxng struct {
	mu      sync.Mutex
	queries []string
	server  *httptest.Server
}

func newFakeSearxng(t *testing.T, results []map[string]interface{}) *fakeSearxng {
	t.Helper()
	f := &fakeSearxng{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		f.mu.Lock()
		f.queries = append(f.queries, q)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"query": q, "results": results})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSearxng) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// assertKiroPoolUntouched is the Kiro-independence assertion. It checks the pool
// recorded no TRAFFIC rather than that it has no entry: metrics.Reset()
// deliberately re-seeds provider entries so names survive a reset, so a
// zero-counter pool entry can legitimately exist from earlier traffic in the same
// test binary. Absence would therefore be the wrong (and flaky) assertion.
func assertKiroPoolUntouched(t *testing.T, context string) {
	t.Helper()
	d, ok := metrics.ProviderDetailFor(metrics.KiroPoolID, 60)
	if !ok {
		return // never seen at all: unambiguously untouched
	}
	if d.Requests != 0 || d.ModelRounds != 0 || d.InputTokens != 0 || d.OutputTokens != 0 {
		t.Errorf("%s: Kiro pool recorded traffic (requests=%d modelRounds=%d tokens=%d/%d); "+
			"inference must stay on the forwarded provider",
			context, d.Requests, d.ModelRounds, d.InputTokens, d.OutputTokens)
	}
}

// decodedSSE is one SSE frame with its data JSON already unmarshaled, so a test
// can assert on the event grammar rather than on substrings.
type decodedSSE struct {
	name string
	data map[string]interface{}
}

// decodeSSEEvents wraps the package's parseSSEEvents, decoding each frame's data.
func decodeSSEEvents(t *testing.T, raw string) []decodedSSE {
	t.Helper()
	var out []decodedSSE
	for _, ev := range parseSSEEvents(t, raw) {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(ev.data), &m); err != nil {
			t.Fatalf("event %q has undecodable data %q: %v", ev.event, ev.data, err)
		}
		out = append(out, decodedSSE{name: ev.event, data: m})
	}
	return out
}

// fakeUpstreamRound is one scripted reply from the fake provider.
type fakeUpstreamRound struct {
	status int
	body   string
}

// fakeUpstream is an Anthropic-compatible /messages endpoint that replays
// scripted rounds and records every request body it received.
type fakeUpstream struct {
	mu       sync.Mutex
	rounds   []fakeUpstreamRound
	got      [][]byte
	paths    []string
	authSeen []string
	server   *httptest.Server
}

func newFakeUpstream(t *testing.T, rounds ...fakeUpstreamRound) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{rounds: rounds}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		idx := len(u.got)
		u.got = append(u.got, body)
		u.paths = append(u.paths, r.URL.Path)
		u.authSeen = append(u.authSeen, r.Header.Get("Authorization"))
		var round fakeUpstreamRound
		if idx < len(u.rounds) {
			round = u.rounds[idx]
		} else {
			round = u.rounds[len(u.rounds)-1]
		}
		u.mu.Unlock()
		if round.status == 0 {
			round.status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(round.status)
		_, _ = w.Write([]byte(round.body))
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *fakeUpstream) requests() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.got...)
}

// setupLocalStrategyRoute configures ONE provider with webSearchStrategy=local,
// a route pointing at it, and web search enabled against the fake SearXNG.
// Deliberately adds no Kiro account: the local loop must not need one.
func setupLocalStrategyRoute(t *testing.T, clientModel, upstreamURL, searxngURL, targetModel string) {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	provider := config.UpstreamProvider{
		ID: "prov-local", Name: "provider-local", BaseURL: upstreamURL,
		ApiKey: "upstream-secret", Enabled: true,
		WebSearchStrategy: "local",
	}
	route := config.ModelRoute{
		ID: "route-local", Model: clientModel, Enabled: true,
		Targets: []config.RouteTarget{{
			UpstreamID: "prov-local", Priority: 0, Weight: 1, Enabled: true,
			TargetModel: targetModel,
		}},
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	enabled := true
	ws := config.GetWebSearchConfigRaw()
	ws.Enabled = true
	ws.SearXNG.Enabled = &enabled
	ws.SearXNG.BaseURL = searxngURL
	ws.SearXNG.MinimumResults = 1
	ws.Limits.MaxRounds = 4
	ws.Limits.MaxSearchesPerRequest = 5
	ws.Limits.MaxResultsPerSearch = 5
	ws.Limits.MaxConcurrentSearches = 2
	if err := config.UpdateWebSearchConfig(ws); err != nil {
		t.Fatalf("UpdateWebSearchConfig: %v", err)
	}
	if !config.WebSearchEnabled() {
		t.Fatal("web search must be enabled for the local strategy test")
	}
}

// nativeWebSearchRequestBody is what Claude Code actually sends: a native
// server-tool spec with a versioned type.
func nativeWebSearchRequestBody(model, question string) string {
	return `{"model":"` + model + `","max_tokens":1024,` +
		`"messages":[{"role":"user","content":"` + question + `"}],` +
		`"tools":[{"name":"web_search","type":"web_search_20250305"}]}`
}

// TestForwardLocalWebSearchEndToEnd is the hermetic acceptance test: one client
// request, two provider rounds against the SAME provider, one real SearXNG call
// in between, and a client response carrying the native server-tool blocks that
// make Claude Code render "Did N searches".
func TestForwardLocalWebSearchEndToEnd(t *testing.T) {
	metrics.Reset()
	metrics.ResetToolStats()

	searxng := newFakeSearxng(t, []map[string]interface{}{
		{"url": "https://go.dev/doc/devel/release", "title": "Go release history", "content": "Go 1.26 was released in August 2026.", "engine": "duckduckgo", "score": 1.0},
		{"url": "https://go.dev/dl/", "title": "Go downloads", "content": "Latest stable: go1.26", "engine": "google", "score": 0.9},
	})

	// Round 1: the provider asks for a search. Round 2: it answers, grounded in
	// the snippet the gateway fed back.
	upstream := newFakeUpstream(t,
		fakeUpstreamRound{body: `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model",
			"content":[
				{"type":"text","text":"Let me look that up."},
				{"type":"tool_use","id":"toolu_round1","name":"web_search","input":{"query":"latest go release"}}
			],
			"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":50}}`},
		fakeUpstreamRound{body: `{"id":"msg_2","type":"message","role":"assistant","model":"upstream-model",
			"content":[{"type":"text","text":"Go 1.26 was released in August 2026."}],
			"stop_reason":"end_turn","usage":{"input_tokens":200,"output_tokens":25}}`},
	)

	setupLocalStrategyRoute(t, "claude-local-test", upstream.server.URL, searxng.server.URL, "upstream-model")

	body := nativeWebSearchRequestBody("claude-local-test", "What is the latest Go release?")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))

	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// ---- provider rounds: same provider, same target model, two rounds --------
	reqs := upstream.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream rounds = %d, want 2 (search round + answer round)", len(reqs))
	}

	var round1 map[string]interface{}
	if err := json.Unmarshal(reqs[0], &round1); err != nil {
		t.Fatalf("round 1 body not JSON: %v", err)
	}
	if round1["model"] != "upstream-model" {
		t.Errorf("round 1 model = %v, want upstream-model (target mapping applied)", round1["model"])
	}
	// Round 1 must carry the synthetic ORDINARY client tool, never the native
	// server-tool spec: the provider is not executing an Anthropic server tool.
	tools, _ := round1["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("round 1 tools = %v, want exactly one", round1["tools"])
	}
	tool, _ := tools[0].(map[string]interface{})
	if tool["name"] != "web_search" {
		t.Errorf("round 1 tool name = %v, want web_search", tool["name"])
	}
	if _, hasType := tool["type"]; hasType {
		t.Errorf("round 1 tool still carries a native type: %v", tool)
	}
	if _, hasSchema := tool["input_schema"]; !hasSchema {
		t.Errorf("round 1 tool has no input_schema, so it is not a usable client tool: %v", tool)
	}

	var round2 map[string]interface{}
	if err := json.Unmarshal(reqs[1], &round2); err != nil {
		t.Fatalf("round 2 body not JSON: %v", err)
	}
	if round2["model"] != "upstream-model" {
		t.Errorf("round 2 model = %v, want upstream-model (same mapping as round 1)", round2["model"])
	}
	// The continuation must use the client-tool protocol and reference the exact
	// tool_use id round 1 produced.
	raw2 := string(reqs[1])
	for _, forbidden := range []string{"server_tool_use", "web_search_tool_result", "web_search_result"} {
		if strings.Contains(raw2, forbidden) {
			t.Errorf("round 2 leaked client-facing block %q into the provider transcript", forbidden)
		}
	}
	if !strings.Contains(raw2, "toolu_round1") {
		t.Error("round 2 does not reference round 1's tool_use id; the continuation linkage is broken")
	}
	if !strings.Contains(raw2, "tool_result") {
		t.Error("round 2 carries no tool_result")
	}
	// The search evidence must actually reach the provider, or the model has
	// nothing to ground its answer in.
	if !strings.Contains(raw2, "go.dev") {
		t.Error("round 2 does not contain the SearXNG result content")
	}

	// ---- the search really executed against the backend ----------------------
	if got := searxng.seen(); len(got) != 1 || got[0] != "latest go release" {
		t.Fatalf("searxng queries = %v, want exactly [latest go release]", got)
	}

	// ---- client response: native server-tool rendering -----------------------
	var resp struct {
		Content []map[string]interface{} `json:"content"`
		Usage   struct {
			InputTokens   int `json:"input_tokens"`
			OutputTokens  int `json:"output_tokens"`
			ServerToolUse *struct {
				WebSearchRequests int `json:"web_search_requests"`
			} `json:"server_tool_use"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not JSON: %v; body = %s", err, rec.Body.String())
	}
	var serverToolID string
	var sawResult, sawText bool
	for _, block := range resp.Content {
		switch block["type"] {
		case "server_tool_use":
			serverToolID, _ = block["id"].(string)
			if block["name"] != "web_search" {
				t.Errorf("server_tool_use name = %v, want web_search", block["name"])
			}
		case "web_search_tool_result":
			sawResult = true
			if got, _ := block["tool_use_id"].(string); got != serverToolID {
				t.Errorf("web_search_tool_result.tool_use_id = %q, want %q (the server_tool_use id)", got, serverToolID)
			}
			items, _ := block["content"].([]interface{})
			if len(items) == 0 {
				t.Error("web_search_tool_result carries no results")
			}
			for _, it := range items {
				m, _ := it.(map[string]interface{})
				if m["type"] != "web_search_result" {
					t.Errorf("result item type = %v, want web_search_result", m["type"])
				}
				if m["url"] == "" || m["url"] == nil {
					t.Errorf("result item has no url: %v", m)
				}
			}
		case "text":
			sawText = true
			if txt, _ := block["text"].(string); !strings.Contains(txt, "Go 1.26") {
				t.Errorf("final text does not use the search result: %q", txt)
			}
		}
	}
	if serverToolID == "" {
		t.Error("client response has no server_tool_use block, so Claude Code would render Did 0 searches")
	}
	if !sawResult {
		t.Error("client response has no web_search_tool_result block")
	}
	if !sawText {
		t.Error("client response has no final text block")
	}
	if resp.Usage.ServerToolUse == nil || resp.Usage.ServerToolUse.WebSearchRequests != 1 {
		t.Errorf("usage.server_tool_use = %+v, want web_search_requests=1", resp.Usage.ServerToolUse)
	}
	// Tokens are the sum across both rounds, reported once.
	if resp.Usage.InputTokens != 300 || resp.Usage.OutputTokens != 75 {
		t.Errorf("usage tokens = %d/%d, want 300/75 (sum of both rounds)",
			resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}

	// ---- accounting: one request, two model rounds, one search ---------------
	ov := metrics.Overall()
	if ov.Requests != 1 {
		t.Errorf("metrics requests = %d, want 1 (two provider rounds are one client request)", ov.Requests)
	}
	if ov.ModelRounds != 2 {
		t.Errorf("metrics modelRounds = %d, want 2", ov.ModelRounds)
	}
	if ov.InputTokens != 300 || ov.OutputTokens != 75 {
		t.Errorf("metrics tokens = %d/%d, want 300/75", ov.InputTokens, ov.OutputTokens)
	}
	if got := atomicLoad(&h.totalRequests); got != 1 {
		t.Errorf("handler totalRequests = %d, want 1", got)
	}
	if got := atomicLoad(&h.totalTokens); got != 375 {
		t.Errorf("handler totalTokens = %d, want 375", got)
	}

	// The pinned provider owns the traffic; the Kiro pool must be untouched.
	assertKiroPoolUntouched(t, "local strategy e2e")
	d, ok := metrics.ProviderDetailFor("prov-local", 60)
	if !ok {
		t.Fatal("pinned provider has no metrics")
	}
	if d.Requests != 1 || d.ModelRounds != 2 {
		t.Errorf("provider requests/modelRounds = %d/%d, want 1/2", d.Requests, d.ModelRounds)
	}

	// Tool stats: the acceptance signal is executions>0 attributed to searxng.
	st := metrics.ToolStatsFor(metrics.ToolKindWebSearch)
	if st.Uses != 1 {
		t.Errorf("tool uses = %d, want 1", st.Uses)
	}
	if st.Executions != 1 {
		t.Errorf("tool executions = %d, want 1", st.Executions)
	}
	if st.ByBackend["searxng"] != 1 {
		t.Errorf("byBackend[searxng] = %d, want 1 (proof SearXNG served the query)", st.ByBackend["searxng"])
	}
	if st.ByOrigin[metrics.ToolOriginKiroMCP] != 0 {
		t.Errorf("MCP origin recorded %d uses; the local loop must never touch Kiro MCP", st.ByOrigin[metrics.ToolOriginKiroMCP])
	}
}

// TestForwardLocalWebSearchWorksWithNoKiroAccount is the Kiro-independence
// regression. It runs the same local loop with the account pool explicitly
// stocked with an EXPIRED, DISABLED account — the state that used to turn a
// web_search request into a Kiro auth failure — and requires the request to
// succeed anyway.
//
// The Handler here is built with no pool and no conversationRunner, so any code
// path that reached for a Kiro account or the MCP endpoint would nil-panic or
// fail rather than pass. Success therefore means inference stayed on the
// forwarded provider and the search stayed on the local orchestrator.
func TestForwardLocalWebSearchWorksWithNoKiroAccount(t *testing.T) {
	metrics.Reset()
	metrics.ResetToolStats()

	searxng := newFakeSearxng(t, []map[string]interface{}{
		{"url": "https://example.org/a", "title": "Result A", "content": "Fresh fact about kiro-go.", "engine": "brave", "score": 1.0},
	})
	upstream := newFakeUpstream(t,
		fakeUpstreamRound{body: `{"id":"m1","type":"message","role":"assistant",
			"content":[{"type":"tool_use","id":"toolu_nokiro","name":"web_search","input":{"query":"kiro-go news"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`},
		fakeUpstreamRound{body: `{"id":"m2","type":"message","role":"assistant",
			"content":[{"type":"text","text":"Fresh fact about kiro-go."}],
			"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":8}}`},
	)
	setupLocalStrategyRoute(t, "claude-nokiro", upstream.server.URL, searxng.server.URL, "upstream-model")

	// An expired, disabled Kiro account: unusable by construction. If the local
	// loop depended on the pool at all, this is the state that would break it.
	if err := config.AddAccount(config.Account{
		ID: "expired-acct", Enabled: false, AccessToken: "", RefreshToken: "", ProfileArn: "",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if got := config.GetEnabledAccounts(); len(got) != 0 {
		t.Fatalf("test setup wrong: %d enabled Kiro accounts, want 0", len(got))
	}

	body := nativeWebSearchRequestBody("claude-nokiro", "Any news about kiro-go?")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))

	// No pool, no conversationRunner: a Kiro-dependent path cannot silently work.
	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 with no usable Kiro account; body = %s", rec.Code, rec.Body.String())
	}
	if got := searxng.seen(); len(got) != 1 {
		t.Fatalf("searxng queries = %v, want exactly one (the search must still run)", got)
	}
	if len(upstream.requests()) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(upstream.requests()))
	}
	assertKiroPoolUntouched(t, "no usable Kiro account")
	st := metrics.ToolStatsFor(metrics.ToolKindWebSearch)
	if st.ByBackend["searxng"] != 1 {
		t.Errorf("byBackend[searxng] = %d, want 1", st.ByBackend["searxng"])
	}
	if st.ByOrigin[metrics.ToolOriginKiroMCP] != 0 {
		t.Error("Kiro MCP origin recorded usage; the local loop must never use MCP")
	}
	body2 := rec.Body.String()
	for _, forbidden := range []string{"Improperly formed request", "invalid bearer", "403"} {
		if strings.Contains(body2, forbidden) {
			t.Errorf("response carries a Kiro-side error (%q): %s", forbidden, body2)
		}
	}
}

// TestForwardLocalDedupRunsOneBackendCall extends the dedup contract to the
// forward caller: two web_search tool_uses with the same query in one round are
// two uses, one execution, one cache hit — and the provider still receives a
// tool_result for BOTH tool_use ids, or the continuation would be rejected.
func TestForwardLocalDedupRunsOneBackendCall(t *testing.T) {
	metrics.Reset()
	metrics.ResetToolStats()

	searxng := newFakeSearxng(t, []map[string]interface{}{
		{"url": "https://example.org/x", "title": "X", "content": "duplicate query result", "engine": "e", "score": 1},
	})
	upstream := newFakeUpstream(t,
		fakeUpstreamRound{body: `{"id":"m1","type":"message","role":"assistant",
			"content":[
				{"type":"tool_use","id":"toolu_a","name":"web_search","input":{"query":"same query"}},
				{"type":"tool_use","id":"toolu_b","name":"web_search","input":{"query":"same query"}}
			],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`},
		fakeUpstreamRound{body: `{"id":"m2","type":"message","role":"assistant",
			"content":[{"type":"text","text":"answered"}],
			"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":5}}`},
	)
	setupLocalStrategyRoute(t, "claude-dedup", upstream.server.URL, searxng.server.URL, "upstream-model")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(nativeWebSearchRequestBody("claude-dedup", "dup")))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	(&Handler{}).handleClaudeMessagesInternal(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := searxng.seen(); len(got) != 1 {
		t.Fatalf("searxng calls = %v, want 1 (duplicate query must be deduped)", got)
	}
	st := metrics.ToolStatsFor(metrics.ToolKindWebSearch)
	if st.Uses != 2 {
		t.Errorf("uses = %d, want 2 (one per tool_use)", st.Uses)
	}
	if st.Executions != 1 {
		t.Errorf("executions = %d, want 1", st.Executions)
	}
	if st.CacheHits != 1 {
		t.Errorf("cacheHits = %d, want 1", st.CacheHits)
	}
	if st.ByBackend["searxng"] != 1 {
		t.Errorf("byBackend[searxng] = %d, want 1", st.ByBackend["searxng"])
	}

	// Both tool_use ids must be answered, even though only one backend call ran.
	round2 := string(upstream.requests()[1])
	for _, id := range []string{"toolu_a", "toolu_b"} {
		if !strings.Contains(round2, id) {
			t.Errorf("round 2 does not answer tool_use %s; the provider would reject the turn", id)
		}
	}
}

// TestForwardLocalStreamsToStreamingClients is the regression for the constraint
// that mattered most in practice: Claude Code ALWAYS sends stream=true, so a
// local-strategy route that rejected streaming rejected every real request.
//
// The loop stays buffered upstream — that is required to inspect tool uses — but
// the completed result is replayed to the client as a well-formed SSE sequence.
// The test asserts the event grammar a client actually depends on: message_start
// first, every content block opened and closed on its own index, the native
// server-tool pair present, the search counter on message_delta, message_stop
// last.
func TestForwardLocalStreamsToStreamingClients(t *testing.T) {
	searxng := newFakeSearxng(t, []map[string]interface{}{
		{"url": "https://example.org/streamed", "title": "Streamed", "content": "streamed fact", "score": 1},
	})
	upstream := newFakeUpstream(t,
		fakeUpstreamRound{body: `{"id":"m1","type":"message","role":"assistant",
			"content":[{"type":"tool_use","id":"toolu_stream","name":"web_search","input":{"query":"stream q"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":30,"output_tokens":10}}`},
		fakeUpstreamRound{body: `{"id":"m2","type":"message","role":"assistant",
			"content":[{"type":"text","text":"The streamed fact is here."}],
			"stop_reason":"end_turn","usage":{"input_tokens":40,"output_tokens":12}}`},
	)
	setupLocalStrategyRoute(t, "claude-stream-local", upstream.server.URL, searxng.server.URL, "upstream-model")
	metrics.Reset()

	body := `{"model":"claude-stream-local","stream":true,"max_tokens":256,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"web_search","type":"web_search_20250305"}]}`
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	(&Handler{}).handleClaudeMessagesInternal(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: a streaming client must be served, not rejected; body = %s",
			rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if len(upstream.requests()) != 2 {
		t.Fatalf("upstream rounds = %d, want 2 (the loop still runs for a streaming client)", len(upstream.requests()))
	}
	if got := searxng.seen(); len(got) != 1 {
		t.Fatalf("searxng queries = %v, want 1", got)
	}

	events := decodeSSEEvents(t, rec.Body.String())
	if len(events) == 0 {
		t.Fatal("no SSE events emitted")
	}
	if events[0].name != "message_start" {
		t.Errorf("first event = %q, want message_start", events[0].name)
	}
	if last := events[len(events)-1].name; last != "message_stop" {
		t.Errorf("last event = %q, want message_stop", last)
	}

	// Every opened block must be closed, exactly once, on its own index.
	opened := map[float64]string{}
	closed := map[float64]int{}
	var sawServerTool, sawResult, sawText bool
	var serverToolID string
	for _, ev := range events {
		switch ev.name {
		case "content_block_start":
			idx, _ := ev.data["index"].(float64)
			if prev, dup := opened[idx]; dup {
				t.Errorf("index %v opened twice (first as %s)", idx, prev)
			}
			cb, _ := ev.data["content_block"].(map[string]interface{})
			typ, _ := cb["type"].(string)
			opened[idx] = typ
			switch typ {
			case "server_tool_use":
				sawServerTool = true
				serverToolID, _ = cb["id"].(string)
				// The query must be complete in the start frame: this path emits no
				// input_json_delta, matching the pure path's synthesized stream.
				input, _ := cb["input"].(map[string]interface{})
				if q, _ := input["query"].(string); q != "stream q" {
					t.Errorf("server_tool_use input.query = %q, want %q", q, "stream q")
				}
			case "web_search_tool_result":
				sawResult = true
				if got, _ := cb["tool_use_id"].(string); got != serverToolID {
					t.Errorf("web_search_tool_result.tool_use_id = %q, want %q", got, serverToolID)
				}
				items, _ := cb["content"].([]interface{})
				if len(items) == 0 {
					t.Error("streamed web_search_tool_result carries no results")
				}
				for _, it := range items {
					m, _ := it.(map[string]interface{})
					if m["type"] != "web_search_result" {
						t.Errorf("streamed result item type = %v, want web_search_result", m["type"])
					}
				}
			case "text":
				sawText = true
			}
		case "content_block_stop":
			idx, _ := ev.data["index"].(float64)
			closed[idx]++
		}
	}
	if !sawServerTool || !sawResult || !sawText {
		t.Errorf("streamed blocks incomplete: serverTool=%v result=%v text=%v", sawServerTool, sawResult, sawText)
	}
	for idx := range opened {
		if closed[idx] != 1 {
			t.Errorf("index %v closed %d times, want exactly 1", idx, closed[idx])
		}
	}
	for idx := range closed {
		if _, ok := opened[idx]; !ok {
			t.Errorf("index %v was closed without being opened", idx)
		}
	}

	// The text must arrive as deltas and carry the answer.
	var streamed string
	for _, ev := range events {
		if ev.name != "content_block_delta" {
			continue
		}
		delta, _ := ev.data["delta"].(map[string]interface{})
		if delta["type"] == "text_delta" {
			t, _ := delta["text"].(string)
			streamed += t
		}
	}
	if !strings.Contains(streamed, "streamed fact") {
		t.Errorf("streamed text does not contain the answer: %q", streamed)
	}

	// message_delta carries the stop reason and the search counter.
	var sawDelta bool
	for _, ev := range events {
		if ev.name != "message_delta" {
			continue
		}
		sawDelta = true
		delta, _ := ev.data["delta"].(map[string]interface{})
		if delta["stop_reason"] != "end_turn" {
			t.Errorf("message_delta stop_reason = %v, want end_turn", delta["stop_reason"])
		}
		usage, _ := ev.data["usage"].(map[string]interface{})
		stu, _ := usage["server_tool_use"].(map[string]interface{})
		if stu == nil || stu["web_search_requests"] != float64(1) {
			t.Errorf("message_delta usage.server_tool_use = %v, want web_search_requests=1", stu)
		}
	}
	if !sawDelta {
		t.Error("no message_delta event: the client would never learn the stop reason or search count")
	}

	// The metric must record how the CLIENT was served. Reading the flag off an
	// upstream round would report every local-loop request as non-streamed, since
	// the rounds are always buffered.
	evs, _ := metrics.Events(metrics.EventFilter{Limit: 10})
	if len(evs) != 1 {
		t.Fatalf("metrics events = %d, want 1", len(evs))
	}
	if !evs[0].Stream {
		t.Error("Event.Stream = false for a streaming client; the streamed counter would undercount")
	}
	if evs[0].ModelRounds != 2 {
		t.Errorf("Event.ModelRounds = %d, want 2", evs[0].ModelRounds)
	}
}

// TestForwardLocalForcesNonStreamOnEveryRound pins the buffered contract at the
// wire. ClaudeRequest.Stream is `omitempty`, so a serialized continuation drops
// the field entirely — and an upstream that streams by default would then answer
// round 2 with SSE, which the round parser cannot read. Every round must
// therefore say stream:false explicitly.
func TestForwardLocalForcesNonStreamOnEveryRound(t *testing.T) {
	searxng := newFakeSearxng(t, []map[string]interface{}{
		{"url": "https://example.org/s", "title": "S", "content": "fact", "score": 1},
	})
	upstream := newFakeUpstream(t,
		fakeUpstreamRound{body: `{"id":"m1","type":"message","role":"assistant",
			"content":[{"type":"tool_use","id":"toolu_ns","name":"web_search","input":{"query":"q"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`},
		fakeUpstreamRound{body: `{"id":"m2","type":"message","role":"assistant",
			"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":6,"output_tokens":2}}`},
	)
	setupLocalStrategyRoute(t, "claude-nonstream", upstream.server.URL, searxng.server.URL, "upstream-model")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(nativeWebSearchRequestBody("claude-nonstream", "q")))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	(&Handler{}).handleClaudeMessagesInternal(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	reqs := upstream.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(reqs))
	}
	for i, raw := range reqs {
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("round %d body not JSON: %v", i+1, err)
		}
		v, present := m["stream"]
		if !present {
			t.Errorf("round %d omits stream; an upstream defaulting to SSE would break the loop", i+1)
			continue
		}
		if v != false {
			t.Errorf("round %d stream = %v, want false", i+1, v)
		}
	}
}

// TestForwardLocalReportsUpstreamStreamingAsCapabilityMismatch covers the
// provider that streams anyway. The failure must name the real cause — a
// provider that cannot serve buffered rounds — instead of surfacing the JSON
// parse error for the 'e' of "event:".
func TestForwardLocalReportsUpstreamStreamingAsCapabilityMismatch(t *testing.T) {
	searxng := newFakeSearxng(t, nil)
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	defer sse.Close()
	setupLocalStrategyRoute(t, "claude-sse-upstream", sse.URL, searxng.server.URL, "upstream-model")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(nativeWebSearchRequestBody("claude-sse-upstream", "q")))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	(&Handler{}).handleClaudeMessagesInternal(rec, r)

	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "streamed a response to a non-stream request") {
		t.Errorf("error does not name the capability mismatch: %s", body)
	}
	if strings.Contains(body, "invalid character") {
		t.Errorf("error surfaces a raw JSON parse failure instead of the real cause: %s", body)
	}
}
