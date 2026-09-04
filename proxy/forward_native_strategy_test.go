package proxy

// The native strategy must be a pure relay: the provider executes Anthropic's
// server-side web_search itself, and the gateway neither rewrites the tool nor
// runs a local search.
//
// A capability-honesty rule rides along: a native provider reports only what it
// observed (usage.server_tool_use.web_search_requests). Local backend and cache
// figures stay zero, because the gateway has no visibility into how the upstream
// searched — partial observability must stay unknown, not be fabricated.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

// setupNativeStrategyRoute configures one provider declaring webSearchStrategy=native
// plus a fake SearXNG that must never be called.
func setupNativeStrategyRoute(t *testing.T, clientModel, upstreamURL, searxngURL string) {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	provider := config.UpstreamProvider{
		ID: "prov-native", Name: "provider-native", BaseURL: upstreamURL,
		ApiKey: "s", Enabled: true, WebSearchStrategy: "native",
	}
	route := config.ModelRoute{
		ID: "route-native", Model: clientModel, Enabled: true,
		Targets: []config.RouteTarget{{UpstreamID: "prov-native", Priority: 0, Weight: 1, Enabled: true}},
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
	// Web search fully configured and ON: the native path must still not use it.
	enabled := true
	ws := config.GetWebSearchConfigRaw()
	ws.Enabled = true
	ws.SearXNG.Enabled = &enabled
	ws.SearXNG.BaseURL = searxngURL
	if err := config.UpdateWebSearchConfig(ws); err != nil {
		t.Fatalf("UpdateWebSearchConfig: %v", err)
	}
}

// TestForwardNativeWebSearchRelaysVerbatim proves the native strategy forwards
// the request unchanged (native tool spec intact, one round only) and never
// invokes the local executor.
func TestForwardNativeWebSearchRelaysVerbatim(t *testing.T) {
	metrics.Reset()
	metrics.ResetToolStats()

	searxng := newFakeSearxng(t, []map[string]interface{}{
		{"url": "https://must.not/be/called", "title": "nope", "content": "nope", "score": 1},
	})
	// What a genuinely native upstream returns: server_tool_use +
	// web_search_tool_result it produced itself, plus its own usage counter.
	upstream := newFakeUpstream(t, fakeUpstreamRound{body: `{"id":"msg_n","type":"message","role":"assistant",
		"content":[
			{"type":"server_tool_use","id":"srvtoolu_up","name":"web_search","input":{"query":"upstream ran this"}},
			{"type":"web_search_tool_result","tool_use_id":"srvtoolu_up","content":[
				{"type":"web_search_result","url":"https://upstream.example/a","title":"A","encrypted_content":"opaque"}
			]},
			{"type":"text","text":"Answer from the upstream's own search."}
		],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":40,"output_tokens":12,"server_tool_use":{"web_search_requests":1}}}`})

	setupNativeStrategyRoute(t, "claude-native-test", upstream.server.URL, searxng.server.URL)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(nativeWebSearchRequestBody("claude-native-test", "what happened today?")))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// One round only — no tool loop.
	reqs := upstream.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream rounds = %d, want 1 (native is a single relay)", len(reqs))
	}
	// The native tool spec must reach the provider untouched: no synthetic rewrite.
	sent := string(reqs[0])
	if !strings.Contains(sent, `"type":"web_search_20250305"`) {
		t.Errorf("native tool type was rewritten; provider received: %s", sent)
	}
	if strings.Contains(sent, "input_schema") {
		t.Errorf("a synthetic client-tool schema was injected into a native relay: %s", sent)
	}

	// The local search backend must never have been consulted.
	if got := searxng.seen(); len(got) != 0 {
		t.Fatalf("SearXNG was called %v on a native route; the upstream owns execution", got)
	}

	// The provider's own blocks reach the client unchanged.
	var resp struct {
		Content []map[string]interface{} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not JSON: %v", err)
	}
	var sawServerTool, sawResult bool
	for _, b := range resp.Content {
		switch b["type"] {
		case "server_tool_use":
			sawServerTool = true
			if b["id"] != "srvtoolu_up" {
				t.Errorf("server_tool_use id = %v, want the upstream's own srvtoolu_up", b["id"])
			}
		case "web_search_tool_result":
			sawResult = true
			if b["tool_use_id"] != "srvtoolu_up" {
				t.Errorf("web_search_tool_result.tool_use_id = %v, want srvtoolu_up", b["tool_use_id"])
			}
		}
	}
	if !sawServerTool || !sawResult {
		t.Errorf("native blocks did not survive the relay: %+v", resp.Content)
	}

	// Capability honesty: uses come from the upstream's counter; executions and
	// cache hits stay unknown (0) because the gateway did not run the search.
	st := metrics.ToolStatsFor(metrics.ToolKindWebSearch)
	if st.Uses != 1 {
		t.Errorf("tool uses = %d, want 1 (from usage.server_tool_use)", st.Uses)
	}
	if st.Executions != 0 {
		t.Errorf("tool executions = %d, want 0: the gateway executed nothing", st.Executions)
	}
	if st.CacheHits != 0 {
		t.Errorf("cacheHits = %d, want 0 for an opaque upstream search", st.CacheHits)
	}
	if len(st.ByBackend) != 0 {
		t.Errorf("byBackend = %v, want empty: no local backend ran", st.ByBackend)
	}
	if st.ByOrigin[metrics.ToolOriginUpstreamNative] != 1 {
		t.Errorf("byOrigin[upstream_native] = %d, want 1", st.ByOrigin[metrics.ToolOriginUpstreamNative])
	}
	if st.ByOrigin[metrics.ToolOriginKiroOrchestrator] != 0 {
		t.Errorf("byOrigin[kiro_orchestrator] = %d, want 0 on a native route", st.ByOrigin[metrics.ToolOriginKiroOrchestrator])
	}

	// One request, one model round: the native relay is not a loop.
	ov := metrics.Overall()
	if ov.Requests != 1 || ov.ModelRounds != 1 {
		t.Errorf("metrics requests/modelRounds = %d/%d, want 1/1", ov.Requests, ov.ModelRounds)
	}
	assertKiroPoolUntouched(t, "native strategy relay")
}

// TestForwardUnsupportedWebSearchIsExplicitError pins the third strategy: an
// unsupported provider produces a clear 400 rather than silently switching the
// request to the Kiro pool.
func TestForwardUnsupportedWebSearchIsExplicitError(t *testing.T) {
	metrics.Reset()
	searxng := newFakeSearxng(t, nil)
	upstream := newFakeUpstream(t, fakeUpstreamRound{body: `{"content":[]}`})

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	provider := config.UpstreamProvider{
		ID: "prov-unsup", Name: "provider-unsup", BaseURL: upstream.server.URL,
		ApiKey: "s", Enabled: true, WebSearchStrategy: "unsupported",
	}
	route := config.ModelRoute{
		ID: "route-unsup", Model: "claude-unsup", Enabled: true,
		Targets: []config.RouteTarget{{UpstreamID: "prov-unsup", Priority: 0, Weight: 1, Enabled: true}},
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
	enabled := true
	ws := config.GetWebSearchConfigRaw()
	ws.Enabled = true
	ws.SearXNG.Enabled = &enabled
	ws.SearXNG.BaseURL = searxng.server.URL
	if err := config.UpdateWebSearchConfig(ws); err != nil {
		t.Fatalf("UpdateWebSearchConfig: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(nativeWebSearchRequestBody("claude-unsup", "anything")))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	(&Handler{}).handleClaudeMessagesInternal(rec, r)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 for an unsupported provider; body = %s", rec.Code, rec.Body.String())
	}
	if len(upstream.requests()) != 0 {
		t.Errorf("upstream received %d requests; an unsupported route must not forward", len(upstream.requests()))
	}
	if got := searxng.seen(); len(got) != 0 {
		t.Errorf("SearXNG called %v; unsupported must not silently become local", got)
	}
	assertKiroPoolUntouched(t, "unsupported strategy must not fall through to the pool")
}

// TestForwardUnspecifiedStrategyPreservesRawPassthrough is the compatibility
// guarantee for every already-deployed provider: with no webSearchStrategy set,
// the request relays exactly as before — no synthetic rewrite, no local loop.
func TestForwardUnspecifiedStrategyPreservesRawPassthrough(t *testing.T) {
	metrics.Reset()
	searxng := newFakeSearxng(t, nil)
	upstream := newFakeUpstream(t, fakeUpstreamRound{body: `{"id":"m","type":"message","role":"assistant",
		"content":[{"type":"text","text":"plain"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":5,"output_tokens":2}}`})

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// No WebSearchStrategy field at all — the pre-Phase-2 shape.
	provider := config.UpstreamProvider{
		ID: "prov-unset", Name: "provider-unset", BaseURL: upstream.server.URL,
		ApiKey: "s", Enabled: true,
	}
	route := config.ModelRoute{
		ID: "route-unset", Model: "claude-unset", Enabled: true,
		Targets: []config.RouteTarget{{UpstreamID: "prov-unset", Priority: 0, Weight: 1, Enabled: true}},
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
	enabled := true
	ws := config.GetWebSearchConfigRaw()
	ws.Enabled = true
	ws.SearXNG.Enabled = &enabled
	ws.SearXNG.BaseURL = searxng.server.URL
	if err := config.UpdateWebSearchConfig(ws); err != nil {
		t.Fatalf("UpdateWebSearchConfig: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(nativeWebSearchRequestBody("claude-unset", "anything")))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	(&Handler{}).handleClaudeMessagesInternal(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	reqs := upstream.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream rounds = %d, want 1 (raw passthrough, no loop)", len(reqs))
	}
	if !strings.Contains(string(reqs[0]), `"type":"web_search_20250305"`) {
		t.Errorf("unset strategy rewrote the tool; body = %s", string(reqs[0]))
	}
	if got := searxng.seen(); len(got) != 0 {
		t.Errorf("SearXNG called %v; unset must not become local", got)
	}
}
