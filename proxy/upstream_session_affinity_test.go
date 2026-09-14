package proxy

// Session-affinity regression tests for the forwarding path.
//
// The invariant throughout: the client's canonical session identity — Claude
// Code's X-Claude-Code-Session-Id, or a native OpenCode client's
// X-Opencode-Session — survives the gateway unchanged, on every attempt of
// every request of the same conversation: retries, provider failover, local
// web-search continuation rounds, streaming or not. The gateway NEVER
// manufactures a session id, and agent-level / request-level ids never stand
// in for the session. A provider-configured SessionHeader maps the SAME value
// to the provider-specific header (OpenCode Go's backend paths demand
// X-Opencode-Session even from a Claude Code conversation).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

// setupSessionAffinityRoute wires one route over the given providers. opts
// lets a test configure the provider session header mapping.
func setupSessionAffinityRoute(t *testing.T, clientModel string, sessionHeader string, urls ...string) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var ups []config.UpstreamProvider
	var targets []config.RouteTarget
	for i, u := range urls {
		id := "up-" + string(rune('a'+i))
		ups = append(ups, config.UpstreamProvider{
			ID: id, Name: "provider-" + string(rune('a'+i)), BaseURL: u, ApiKey: "s", Enabled: true,
			SessionHeader: sessionHeader,
		})
		targets = append(targets, config.RouteTarget{
			UpstreamID: id, Priority: i, Weight: 1, Enabled: true,
		})
	}
	route := config.ModelRoute{ID: "route-1", Model: clientModel, Targets: targets, Enabled: true}
	if err := config.UpdateUpstreamConfig(ups, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
}

// forwardWithHeaders sends one non-streaming Claude route forward request with
// the given client headers set.
func forwardWithHeaders(t *testing.T, clientModel string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"model":"` + clientModel + `","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(body), clientModel, false, "/messages", true, "") {
		t.Fatal("expected route to match and forward")
	}
	return rec
}

// headerRecorder collects the session headers seen across upstream requests.
type headerRecorder struct {
	mu       sync.Mutex
	claude   []string
	opencode []string
	agent    []string
}

func (rec *headerRecorder) values() (claude, opencode, agent []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.claude...), append([]string(nil), rec.opencode...), append([]string(nil), rec.agent...)
}

// A. Claude Code's native session header must reach the upstream intact — it
// must not fall victim to the curated forwardableRequestHeaders allow-list.
// D. Three requests of the same conversation all carry the SAME session.
func TestForwardPreservesClaudeCodeSessionID(t *testing.T) {
	metrics.Reset()
	var rec headerRecorder
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.claude = append(rec.claude, r.Header.Get(HeaderClaudeCodeSession))
		rec.opencode = append(rec.opencode, r.Header.Get(HeaderOpencodeSession))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	setupSessionAffinityRoute(t, "m", "", upstream.URL)
	for i := 0; i < 3; i++ {
		if code := forwardWithHeaders(t, "m", map[string]string{
			"X-Claude-Code-Session-Id": "session-A",
		}).Code; code != 200 {
			t.Fatalf("request %d: status = %d, want 200", i, code)
		}
	}
	claude, opencode, _ := rec.values()
	if len(claude) != 3 {
		t.Fatalf("upstream saw %d requests, want 3", len(claude))
	}
	for i, got := range claude {
		if got != "session-A" {
			t.Errorf("request %d: upstream X-Claude-Code-Session-Id = %q, want session-A", i, got)
		}
		if opencode[i] != "" {
			t.Errorf("request %d: upstream received X-Opencode-Session %q; none was requested", i, opencode[i])
		}
	}
}

// B. With the provider adapter mapping enabled (SessionHeader configured), an
// OpenCode Go target receives X-Opencode-Session with the EXACT same value the
// Claude Code client sent.
func TestForwardMapsSessionToOpencodeHeader(t *testing.T) {
	metrics.Reset()
	var gotClaude, gotOpencode string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaude = r.Header.Get(HeaderClaudeCodeSession)
		gotOpencode = r.Header.Get(HeaderOpencodeSession)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	setupSessionAffinityRoute(t, "m", "X-Opencode-Session", upstream.URL)
	if code := forwardWithHeaders(t, "m", map[string]string{
		"X-Claude-Code-Session-Id": "session-A",
	}).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotOpencode != "session-A" {
		t.Fatalf("OpenCode Go target X-Opencode-Session = %q, want session-A (exact same value)", gotOpencode)
	}
	if gotClaude != "session-A" {
		t.Errorf("native header no longer preserved alongside the mapping: %q", gotClaude)
	}
}

// C. + I. No session header in, no session header out — and above all no
// manufactured per-request UUID: repeated sessionless requests must not
// acquire a generated session.
func TestForwardDoesNotGenerateSessionID(t *testing.T) {
	metrics.Reset()
	var rec headerRecorder
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.claude = append(rec.claude, r.Header.Get(HeaderClaudeCodeSession))
		rec.opencode = append(rec.opencode, r.Header.Get(HeaderOpencodeSession))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	setupSessionAffinityRoute(t, "m", "X-Opencode-Session", upstream.URL)
	for i := 0; i < 3; i++ {
		forwardWithHeaders(t, "m", nil)
	}
	claude, opencode, _ := rec.values()
	if len(claude) != 3 {
		t.Fatalf("upstream saw %d requests, want 3", len(claude))
	}
	for i := range claude {
		if claude[i] != "" || opencode[i] != "" {
			t.Errorf("request %d: generated session header(s) %q / %q; a sessionless request must stay sessionless",
				i, claude[i], opencode[i])
		}
	}
}

// E. Failover: the backup provider receives the SAME logical session the
// primary was given — affinity is derived from the inbound request on every
// attempt, not from per-target state.
func TestForwardSessionAffinitySurvivesFailover(t *testing.T) {
	metrics.Reset()
	var mu sync.Mutex
	var seen []string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(HeaderClaudeCodeSession))
		mu.Unlock()
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(HeaderClaudeCodeSession))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer backup.Close()

	setupSessionAffinityRoute(t, "m", "", primary.URL, backup.URL)
	if code := forwardWithHeaders(t, "m", map[string]string{
		"X-Claude-Code-Session-Id": "session-A",
	}).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("attempts recorded = %d, want 2 (primary fail + backup)", len(seen))
	}
	for i, got := range seen {
		if got != "session-A" {
			t.Errorf("attempt %d (target %d) session = %q, want session-A", i, i, got)
		}
	}
}

// F. + G. Parallel agents of one session share the session id but differ in
// agent id; request ids differ per call. Session affinity must remain the
// session, never the agent or request id — and neither agent nor request id
// may leak upstream as session state.
func TestForwardSessionIgnoresAgentAndRequestIDs(t *testing.T) {
	metrics.Reset()
	var rec headerRecorder
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.claude = append(rec.claude, r.Header.Get(HeaderClaudeCodeSession))
		rec.agent = append(rec.agent, r.Header.Get("X-Claude-Code-Agent-Id"))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	setupSessionAffinityRoute(t, "m", "", upstream.URL)
	for i, agent := range []string{"agent-1", "agent-2"} {
		forwardWithHeaders(t, "m", map[string]string{
			"X-Claude-Code-Session-Id": "session-A",
			"X-Claude-Code-Agent-Id":   agent,
			"X-Client-Request-Id":      fmt.Sprintf("req-%d", i),
		})
	}
	claude, _, agents := rec.values()
	if len(claude) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(claude))
	}
	for i, got := range claude {
		if got != "session-A" {
			t.Errorf("request %d: session = %q, want session-A (agent %q must not become the session)", i, got, agents[i])
		}
	}
	for i, got := range agents {
		if got != "" {
			t.Errorf("request %d: agent id %q leaked upstream; it is agent-level state, not session state", i, got)
		}
	}
}

// H. A native OpenCode client already sending X-Opencode-Session keeps it
// preserved as-is, with or without a configured mapping.
func TestForwardPreservesNativeOpencodeSession(t *testing.T) {
	metrics.Reset()
	var gotOpencode string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOpencode = r.Header.Get(HeaderOpencodeSession)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	setupSessionAffinityRoute(t, "m", "", upstream.URL)
	forwardWithHeaders(t, "m", map[string]string{"X-Opencode-Session": "opencode-session-1"})
	if gotOpencode != "opencode-session-1" {
		t.Fatalf("native OpenCode session = %q, want opencode-session-1", gotOpencode)
	}
}

// The local web-search loop: every continuation round replays the session
// identity, so the pinned provider sees ONE logical session across the whole
// loop — with the provider mapping applied too when configured.
func TestForwardLocalWebSearchRoundsKeepSessionAffinity(t *testing.T) {
	metrics.Reset()
	metrics.ResetToolStats()

	searxng := newFakeSearxng(t, []map[string]interface{}{
		{"url": "https://go.dev/dl/", "title": "Go downloads", "content": "Latest stable: go1.26", "engine": "duckduckgo", "score": 1.0},
	})

	upstream := newFakeUpstream(t,
		fakeUpstreamRound{body: `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model",
			"content":[
				{"type":"text","text":"Let me look that up."},
				{"type":"tool_use","id":"toolu_r1","name":"web_search","input":{"query":"latest go release"}}
			],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`},
		fakeUpstreamRound{body: `{"id":"msg_2","type":"message","role":"assistant","model":"upstream-model",
			"content":[{"type":"text","text":"Go 1.26 is the latest."}],
			"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":5}}`},
	)

	// Same route setup as the local-strategy e2e test, plus the provider
	// session mapping, so BOTH the preserved header and the mapped header can
	// be asserted on round 2.
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	provider := config.UpstreamProvider{
		ID: "prov-local", Name: "provider-local", BaseURL: upstream.server.URL,
		ApiKey: "upstream-secret", Enabled: true,
		WebSearchStrategy: "local",
		SessionHeader:     "X-Opencode-Session",
	}
	route := config.ModelRoute{
		ID: "route-local", Model: "claude-local-sess", Enabled: true,
		Targets: []config.RouteTarget{{UpstreamID: "prov-local", Priority: 0, Weight: 1, Enabled: true, TargetModel: "upstream-model"}},
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
	enabled := true
	ws := config.GetWebSearchConfigRaw()
	ws.Enabled = true
	ws.SearXNG.Enabled = &enabled
	ws.SearXNG.BaseURL = searxng.server.URL
	ws.SearXNG.MinimumResults = 1
	ws.Limits.MaxRounds = 4
	ws.Limits.MaxSearchesPerRequest = 5
	ws.Limits.MaxResultsPerSearch = 5
	ws.Limits.MaxConcurrentSearches = 2
	if err := config.UpdateWebSearchConfig(ws); err != nil {
		t.Fatalf("UpdateWebSearchConfig: %v", err)
	}

	body := nativeWebSearchRequestBody("claude-local-sess", "What is the latest Go release?")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-Claude-Code-Session-Id", "session-A")
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))

	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	reqs := upstream.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(reqs))
	}
	for i := 0; i < 2; i++ {
		if got := upstream.header(i, HeaderClaudeCodeSession); got != "session-A" {
			t.Errorf("round %d: X-Claude-Code-Session-Id = %q, want session-A (continuation must keep affinity)", i, got)
		}
		if got := upstream.header(i, HeaderOpencodeSession); got != "session-A" {
			t.Errorf("round %d: mapped X-Opencode-Session = %q, want session-A", i, got)
		}
	}
}
