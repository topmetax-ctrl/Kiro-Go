package proxy

// Session-affinity regression tests for the forwarding path.
//
// The invariant throughout: the client's canonical session identity — Claude
// Code's X-Claude-Code-Session-Id, a native OpenCode client's
// X-Opencode-Session, or (strictly validated, last resort) the session inside
// metadata.user_id — survives the gateway unchanged, on every attempt of
// every request of the same conversation: retries, provider failover, local
// web-search continuation rounds, streaming or not. The gateway NEVER
// manufactures a session id, and agent-level / request-level ids never stand
// in for the session. A provider-configured SessionHeader maps the SAME value
// to the provider-specific header (OpenCode Go's backend paths demand
// X-Opencode-Session even from a Claude Code conversation).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
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

// The admin Test probe must exercise the same session path real traffic takes:
// a marked synthetic session id, plus the provider's SessionHeader mapping.
// A sessionless probe made OpenCode Go's backend answer 400 MissingSessionID,
// so the panel read a perfectly working target as "HTTP 400 Provider rejected".
func TestUpstreamTestProbeCarriesSessionAffinity(t *testing.T) {
	metrics.Reset()
	var mu sync.Mutex
	seen := map[string]map[string]string{} // path -> header -> value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = map[string]string{
			HeaderClaudeCodeSession: r.Header.Get(HeaderClaudeCodeSession),
			HeaderOpencodeSession:   r.Header.Get(HeaderOpencodeSession),
		}
		mu.Unlock()
		if r.URL.Path == "/chat/completions" {
			// Only the Anthropic shape is served here, so the probe falls through
			// to /messages exactly as it would for a real Claude route target.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"probe-ok"}`))
	}))
	defer upstream.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	provider := config.UpstreamProvider{
		ID: "probe-up", Name: "probe-provider", BaseURL: upstream.URL,
		ApiKey: "s", Enabled: true, SessionHeader: "X-Opencode-Session",
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, nil); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"id":"probe-up","model":"m"}`)
	r := httptest.NewRequest(http.MethodPost, "/admin/api/upstream-test", body)
	h := &Handler{}
	h.runUpstreamTest(rec, r, "probe-up", "", "", "", "", "m")

	var res struct {
		Ok   bool   `json:"ok"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("test result not JSON: %v; body=%s", err, rec.Body.String())
	}
	if !res.Ok {
		t.Fatalf("probe failed: %s", rec.Body.String())
	}
	if res.Path != "/messages" {
		t.Fatalf("probe answered on %q, want /messages", res.Path)
	}

	mu.Lock()
	defer mu.Unlock()
	msg := seen["/messages"]
	claude, opencode := msg[HeaderClaudeCodeSession], msg[HeaderOpencodeSession]
	if !strings.HasPrefix(claude, ProbeSessionPrefix) {
		t.Errorf("probe session id = %q, want the %q prefix", claude, ProbeSessionPrefix)
	}
	if opencode != claude {
		t.Errorf("mapped X-Opencode-Session = %q, want the same synthetic id (%q)", opencode, claude)
	}
	chat := seen["/chat/completions"]
	if chat == nil {
		t.Fatal("probe never attempted /chat/completions before falling back")
	}
	if chat[HeaderClaudeCodeSession] != claude {
		t.Errorf("probe changed its synthetic session between shapes: /chat/completions %q vs /messages %q",
			chat[HeaderClaudeCodeSession], claude)
	}
}

// Without a configured mapping the probe still carries the synthetic native
// session id, but must not invent a mapping header.
func TestUpstreamTestProbeWithoutMappingHasNoMappedHeader(t *testing.T) {
	metrics.Reset()
	var mu sync.Mutex
	var claude, opencode string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		claude = r.Header.Get(HeaderClaudeCodeSession)
		opencode = r.Header.Get(HeaderOpencodeSession)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"probe-ok"}`))
	}))
	defer upstream.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	provider := config.UpstreamProvider{
		ID: "probe-up", Name: "probe-provider", BaseURL: upstream.URL,
		ApiKey: "s", Enabled: true,
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, nil); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/upstream-test", strings.NewReader(`{"id":"probe-up","model":"m"}`))
	h := &Handler{}
	h.runUpstreamTest(rec, r, "probe-up", "", "", "", "", "m")

	var res struct {
		Ok bool `json:"ok"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("test result not JSON: %v", err)
	}
	if !res.Ok {
		t.Fatalf("probe failed: %s", rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasPrefix(claude, ProbeSessionPrefix) {
		t.Errorf("probe session id = %q, want the %q prefix", claude, ProbeSessionPrefix)
	}
	if opencode != "" {
		t.Errorf("unmapped provider received X-Opencode-Session %q; probes must not invent mappings", opencode)
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

// ─── Body-metadata compatibility fallback ────────────────────────────────────
//
// Aggregators that rebuild requests (9router and peers) drop the session
// headers but relay the same Claude Code identity inside metadata.user_id.
// The gateway accepts that value as the LAST source, under strict shape
// validation; an absent or malformed value leaves the request sessionless —
// the gateway never manufactures an identity from an account field, a
// request id, or free-form junk.

// bodyWithMetadataUserID builds a minimal Claude request whose metadata.user_id
// is exactly the given raw string (JSON-encoded, so any shape can be planted).
func bodyWithMetadataUserID(t *testing.T, userID string) string {
	t.Helper()
	raw, err := json.Marshal(userID)
	if err != nil {
		t.Fatalf("marshal user_id: %v", err)
	}
	return `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":` + string(raw) + `}}`
}

// forwardBodyViaHandler posts a raw Claude request body through the real
// /v1/messages handler, so the body-derived session fallback wiring runs end
// to end. Returns the recorder so goroutine callers can report without Fatal.
func forwardBodyViaHandler(t *testing.T, body string, headers map[string]string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)
	if rec.Code != 200 {
		return rec, fmt.Errorf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	return rec, nil
}

// newSessionRecordingUpstream returns a fake upstream that always answers 200
// and records the session headers of every request it sees.
func newSessionRecordingUpstream(t *testing.T, rec *headerRecorder) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.claude = append(rec.claude, r.Header.Get(HeaderClaudeCodeSession))
		rec.opencode = append(rec.opencode, r.Header.Get(HeaderOpencodeSession))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// metadataSessionID accepts exactly the two known shapes and nothing else:
// the JSON envelope's session_id field, or the native envelope's trailing
// _session_<uuid>. Account fields, non-UUIDs, and junk all yield "".
func TestMetadataSessionIDExtractor(t *testing.T) {
	const valid = "11111111-2222-4333-8444-555555555555"
	const other = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"json envelope", `{"device_id":"` + strings.Repeat("a", 64) + `","account_uuid":"` + other + `","session_id":"` + valid + `"}`, valid},
		{"json uppercase uuid kept as sent", `{"session_id":"AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"}`, "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"},
		{"json without session_id", `{"device_id":"` + valid + `","account_uuid":"` + other + `"}`, ""},
		{"json session_id not a uuid", `{"session_id":"not-a-uuid"}`, ""},
		{"json array is not an envelope", `["` + valid + `"]`, ""},
		{"json malformed", `{"session_id":`, ""},
		{"native envelope", "user_" + strings.Repeat("b", 64) + "_account_" + other + "_session_" + valid, valid},
		{"native with trailing junk", "user_x_account_" + other + "_session_" + valid + "/turn", ""},
		{"plain uuid has no marker", valid, ""},
		{"plain string", "totally-not-a-session", ""},
		{"empty", "", ""},
		{"oversized", strings.Repeat("x", 5000), ""},
	}
	for _, tc := range cases {
		if got := metadataSessionID(tc.raw); got != tc.want {
			t.Errorf("%s: metadataSessionID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Header sources keep precedence over the body fallback; the fallback fires
// only when neither session header is present.
func TestClientSessionAffinitySourcePrecedence(t *testing.T) {
	const meta = "66666666-7777-4333-8444-aaaaaaaaaaaa"
	newReq := func() *http.Request { return httptest.NewRequest(http.MethodPost, "/v1/messages", nil) }

	r := newReq()
	r.Header.Set(HeaderClaudeCodeSession, "session-A")
	if sess := clientSessionAffinity(withBodySessionFallback(r, meta)); sess.ID != "session-A" || sess.Source != sessionSourceClaudeCode {
		t.Errorf("claude header + body: got {%s %s}, want session-A/claude-code", sess.ID, sess.Source)
	}

	r = newReq()
	r.Header.Set(HeaderOpencodeSession, "session-O")
	if sess := clientSessionAffinity(withBodySessionFallback(r, meta)); sess.ID != "session-O" || sess.Source != sessionSourceOpencode {
		t.Errorf("opencode header + body: got {%s %s}, want session-O/opencode", sess.ID, sess.Source)
	}

	if sess := clientSessionAffinity(withBodySessionFallback(newReq(), meta)); sess.ID != meta || sess.Source != sessionSourceClaudeMetadata {
		t.Errorf("body only: got {%s %s}, want %s/claude-metadata", sess.ID, sess.Source, meta)
	}

	if sess := clientSessionAffinity(newReq()); sess.present() || sess.Source != sessionSourceNone {
		t.Errorf("no identity: got {%s %s}, want none", sess.ID, sess.Source)
	}
}

// Metadata-only request (JSON envelope shape): the upstream must receive the
// recovered session under BOTH the canonical Claude header and the provider
// mapping header, with the exact value from the body.
func TestForwardRecoversSessionFromBodyMetadata(t *testing.T) {
	metrics.Reset()
	const sid = "11111111-2222-4333-8444-555555555555"
	var rec headerRecorder
	upstream := newSessionRecordingUpstream(t, &rec)
	setupSessionAffinityRoute(t, "m", "X-Opencode-Session", upstream.URL)

	body := bodyWithMetadataUserID(t, `{"device_id":"`+strings.Repeat("a", 64)+`","account_uuid":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","session_id":"`+sid+`"}`)
	if _, err := forwardBodyViaHandler(t, body, nil); err != nil {
		t.Fatalf("forward: %v", err)
	}

	claude, opencode, _ := rec.values()
	if len(claude) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(claude))
	}
	if claude[0] != sid {
		t.Errorf("upstream X-Claude-Code-Session-Id = %q, want %q", claude[0], sid)
	}
	if opencode[0] != sid {
		t.Errorf("mapped X-Opencode-Session = %q, want %q", opencode[0], sid)
	}
}

// Claude Code's native user_id envelope is recognized the same way.
func TestForwardRecoversSessionFromNativeMetadataEnvelope(t *testing.T) {
	metrics.Reset()
	const sid = "22222222-3333-4333-8444-666666666666"
	var rec headerRecorder
	upstream := newSessionRecordingUpstream(t, &rec)
	setupSessionAffinityRoute(t, "m", "X-Opencode-Session", upstream.URL)

	body := bodyWithMetadataUserID(t, "user_"+strings.Repeat("b", 64)+"_account_aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee_session_"+sid)
	if _, err := forwardBodyViaHandler(t, body, nil); err != nil {
		t.Fatalf("forward: %v", err)
	}

	claude, opencode, _ := rec.values()
	if len(claude) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(claude))
	}
	if claude[0] != sid || opencode[0] != sid {
		t.Errorf("upstream session headers = (%q, %q), want (%q, %q)", claude[0], opencode[0], sid, sid)
	}
}

// Header beats body: a client presenting both is unambiguous — the header
// value is forwarded, the body value never leaves the gateway.
func TestForwardSessionHeaderWinsOverBodyMetadata(t *testing.T) {
	metrics.Reset()
	const bodySID = "77777777-8888-4333-8444-aaaaaaaaaaaa"
	var rec headerRecorder
	upstream := newSessionRecordingUpstream(t, &rec)
	setupSessionAffinityRoute(t, "m", "X-Opencode-Session", upstream.URL)

	body := bodyWithMetadataUserID(t, `{"session_id":"`+bodySID+`"}`)
	if _, err := forwardBodyViaHandler(t, body, map[string]string{
		"X-Claude-Code-Session-Id": "session-A",
	}); err != nil {
		t.Fatalf("forward: %v", err)
	}

	claude, opencode, _ := rec.values()
	if len(claude) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(claude))
	}
	if claude[0] != "session-A" || opencode[0] != "session-A" {
		t.Errorf("upstream session headers = (%q, %q), want session-A twice", claude[0], opencode[0])
	}
}

// Malformed or foreign metadata must NOT invent a session: the upstream
// request stays sessionless, exactly like a client that sent nothing.
func TestForwardMalformedMetadataStaysSessionless(t *testing.T) {
	const uuidLike = "11111111-2222-4333-8444-555555555555"
	cases := []struct {
		name string
		raw  string
	}{
		{"free-form string", "totally-not-a-session"},
		{"device_id never taken", `{"device_id":"` + uuidLike + `"}`},
		{"account_uuid never taken", `{"account_uuid":"` + uuidLike + `"}`},
		{"non-uuid session", `{"session_id":"kiro-go-probe-x"}`},
		{"native junk suffix", "user_x_account_y_session_zzz"},
		{"oversized", strings.Repeat("x", 5000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.Reset()
			var rec headerRecorder
			upstream := newSessionRecordingUpstream(t, &rec)
			setupSessionAffinityRoute(t, "m", "X-Opencode-Session", upstream.URL)

			if _, err := forwardBodyViaHandler(t, bodyWithMetadataUserID(t, tc.raw), nil); err != nil {
				t.Fatalf("forward: %v", err)
			}
			claude, opencode, _ := rec.values()
			if len(claude) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(claude))
			}
			if claude[0] != "" || opencode[0] != "" {
				t.Errorf("upstream session headers = (%q, %q), want none — malformed metadata must not invent an identity", claude[0], opencode[0])
			}
		})
	}
}

// Local web-search continuation rounds replay the body-derived identity too:
// both rounds carry the recovered session under both spellings.
func TestForwardLocalWebSearchRoundsKeepBodyMetadataSession(t *testing.T) {
	metrics.Reset()
	metrics.ResetToolStats()
	const sid = "88888888-9999-4333-8444-bbbbbbbbbbbb"

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
		ID: "route-local-meta", Model: "claude-local-meta", Enabled: true,
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

	env, _ := json.Marshal(map[string]string{"session_id": sid})
	uid, _ := json.Marshal(string(env))
	body := strings.TrimSuffix(nativeWebSearchRequestBody("claude-local-meta", "What is the latest Go release?"), "}") +
		`,"metadata":{"user_id":` + string(uid) + `}}`
	if _, err := forwardBodyViaHandler(t, body, nil); err != nil {
		t.Fatalf("forward: %v", err)
	}

	if reqs := upstream.requests(); len(reqs) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(reqs))
	}
	for i := 0; i < 2; i++ {
		if got := upstream.header(i, HeaderClaudeCodeSession); got != sid {
			t.Errorf("round %d: X-Claude-Code-Session-Id = %q, want %q", i, got, sid)
		}
		if got := upstream.header(i, HeaderOpencodeSession); got != sid {
			t.Errorf("round %d: mapped X-Opencode-Session = %q, want %q", i, got, sid)
		}
	}
}

// Body-derived identity is request-scoped: concurrent requests carrying
// different metadata sessions must not cross-contaminate — resolution state
// lives in each request's context, never in shared mutable state.
func TestForwardBodyMetadataRequestScoped(t *testing.T) {
	metrics.Reset()
	ids := []string{
		"44444444-5555-4333-8444-888888888888",
		"55555555-6666-4333-8444-999999999999",
	}
	var rec headerRecorder
	upstream := newSessionRecordingUpstream(t, &rec)
	setupSessionAffinityRoute(t, "m", "X-Opencode-Session", upstream.URL)

	recorders := make([]*httptest.ResponseRecorder, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			env, _ := json.Marshal(map[string]string{"session_id": id})
			uid, _ := json.Marshal(string(env))
			body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":` + string(uid) + `}}`
			recorders[i], errs[i] = forwardBodyViaHandler(t, body, nil)
		}(i, id)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	for i, rc := range recorders {
		if rc == nil || rc.Code != 200 {
			t.Errorf("request %d did not complete: %v", i, rc)
		}
	}
	claude, _, _ := rec.values()
	got := append([]string(nil), claude...)
	sort.Strings(got)
	want := append([]string(nil), ids...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("upstream saw sessions %v, want exactly %v", got, want)
	}
}
