package proxy

// Session compatibility / provider session policy tests.
//
// Companion to upstream_session_affinity_test.go, which covers the two native
// carriers and the body fallback. This file covers what was added on top:
//
//   - the generic conversation carriers (X-Session-Affinity, X-Session-Id,
//     Session-Id) and their place in the precedence order;
//   - bounded validation applied to EVERY header carrier, native included, so an
//     unusable value falls through to the next source instead of travelling;
//   - config.SessionMissingPolicy: a provider may fall back to ONE request-scoped
//     synthetic UUID when the client presented no conversation identity, and that
//     value must be stable across key rotation, failover and web-search rounds,
//     distinct per request, and carried ONLY by the provider's configured header;
//   - that none of the above disturbs usage/cache telemetry, error sanitization,
//     probe isolation, or providers left on the default policy.
//
// The invariant a synthetic identity must never break: it is availability, not
// affinity. It never claims to be a client conversation id, and it never reaches
// a native session header unless the operator configured that header itself.

import (
	"context"
	"encoding/json"
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

// ─── helpers ────────────────────────────────────────────────────────────────

// setupSessionPolicyRoute wires one route over the given upstream URLs with a
// provider session header and missing-session policy.
func setupSessionPolicyRoute(t *testing.T, clientModel, sessionHeader, missingPolicy string, urls ...string) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var ups []config.UpstreamProvider
	var targets []config.RouteTarget
	for i, u := range urls {
		id := fmt.Sprintf("up-%d", i)
		ups = append(ups, config.UpstreamProvider{
			ID: id, Name: fmt.Sprintf("provider-%d", i), BaseURL: u, ApiKey: "s", Enabled: true,
			SessionHeader:        sessionHeader,
			SessionMissingPolicy: missingPolicy,
		})
		targets = append(targets, config.RouteTarget{UpstreamID: id, Priority: i, Weight: 1, Enabled: true})
	}
	route := config.ModelRoute{ID: "route-1", Model: clientModel, Targets: targets, Enabled: true}
	if err := config.UpdateUpstreamConfig(ups, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
}

// forwardWithHolder forwards one non-streaming request with a synthetic-session
// holder attached, which is what serveInference does for real traffic (see
// TestServeInferenceAttachesSyntheticHolder). subPath selects the endpoint
// family; stream selects the wire mode.
func forwardWithHolder(t *testing.T, clientModel, subPath string, stream bool, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"model":"` + clientModel + `","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1"+subPath, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	r = withRequestSyntheticSession(r)
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(body), clientModel, stream, subPath, subPath == "/messages", "") {
		t.Fatal("expected route to match and forward")
	}
	return rec
}

// sessionSink is an upstream that records every session-ish header it is sent.
type sessionSink struct {
	mu     sync.Mutex
	seen   []http.Header
	status int
	body   string
}

func (s *sessionSink) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Clone())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if s.status != 0 {
			w.WriteHeader(s.status)
		}
		if s.body != "" {
			_, _ = w.Write([]byte(s.body))
			return
		}
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}
}

func (s *sessionSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// get returns header name from request i.
func (s *sessionSink) get(i int, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.seen) {
		return ""
	}
	return s.seen[i].Get(name)
}

func newSessionSink(t *testing.T, s *sessionSink) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv
}

// canonicalUUID reports whether v is exactly a canonical 36-char UUID. The
// synthetic wire value must be indistinguishable in FORM from a real session id
// — a recognizable prefix would be rejected by backends that validate the
// format, which is the whole reason the value is bare.
func canonicalUUID(v string) bool { return strictUUID(v) == v && v != "" }

// ─── C/D/E: generic conversation carriers ───────────────────────────────────

func TestForwardAcceptsGenericSessionCarriers(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		wantEchoed string
	}{
		{"X-Session-Affinity", HeaderSessionAffinity, HeaderSessionAffinity},
		{"X-Session-Id", HeaderSessionID, HeaderSessionID},
		{"Session-Id", HeaderSessionIDBare, HeaderSessionIDBare},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.Reset()
			sink := &sessionSink{}
			srv := newSessionSink(t, sink)
			setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

			if code := forwardWithHolder(t, "m", "/messages", false, map[string]string{
				tc.header: "conv-1",
			}).Code; code != 200 {
				t.Fatalf("status = %d, want 200", code)
			}
			// The carrier the client used is echoed with the canonical value...
			if got := sink.get(0, tc.wantEchoed); got != "conv-1" {
				t.Errorf("%s = %q, want conv-1", tc.wantEchoed, got)
			}
			// ...and the provider mapping carries the SAME value, which is what
			// makes a generic client work against a session-demanding backend.
			if got := sink.get(0, HeaderOpencodeSession); got != "conv-1" {
				t.Errorf("mapped X-Opencode-Session = %q, want conv-1", got)
			}
			// A generic carrier must not be promoted into a native spelling: the
			// client did not claim to be Claude Code.
			if got := sink.get(0, HeaderClaudeCodeSession); got != "" {
				t.Errorf("X-Claude-Code-Session-Id = %q, want empty for a generic carrier", got)
			}
		})
	}
}

// I. Precedence is the contract: native beats generic, generic beats the body
// fallback, and every present carrier is reconciled to the one winning value so
// the upstream never sees two logical sessions on one request.
func TestForwardSessionCarrierPrecedence(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

	if code := forwardWithHolder(t, "m", "/messages", false, map[string]string{
		HeaderClaudeCodeSession: "native-claude",
		HeaderOpencodeSession:   "native-opencode",
		HeaderSessionAffinity:   "generic-affinity",
		HeaderSessionID:         "generic-x",
		HeaderSessionIDBare:     "generic-bare",
	}).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, name := range []string{
		HeaderClaudeCodeSession, HeaderOpencodeSession,
		HeaderSessionAffinity, HeaderSessionID, HeaderSessionIDBare,
	} {
		if got := sink.get(0, name); got != "native-claude" {
			t.Errorf("%s = %q, want native-claude (all present carriers reconcile to the winner)", name, got)
		}
	}
}

func TestForwardGenericCarrierBeatsBodyMetadata(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

	// Body carries a valid metadata identity; an explicit generic header must win.
	body := bodyWithMetadataUserID(t, `{"session_id":"from-body"}`)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set(HeaderSessionAffinity, "from-header")
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	r = withRequestSyntheticSession(r)
	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := sink.get(0, HeaderOpencodeSession); got != "from-header" {
		t.Errorf("mapped session = %q, want from-header (explicit header beats body fallback)", got)
	}
}

// ─── H/I/J/K: bounded validation on every header carrier ────────────────────

// An unusable carrier is treated as absent: resolution falls through to the next
// source. Previously only the body carrier was bounded, so a 900KB or
// control-character-bearing native header would have been relayed verbatim.
func TestForwardRejectsUnusableSessionHeaderValues(t *testing.T) {
	oversized := strings.Repeat("a", maxSessionTokenLen+1)
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"oversized native", map[string]string{HeaderClaudeCodeSession: oversized}},
		{"oversized generic", map[string]string{HeaderSessionAffinity: oversized}},
		{"control char native", map[string]string{HeaderOpencodeSession: "abc\x01def"}},
		{"control char generic", map[string]string{HeaderSessionID: "abc\x7fdef"}},
		{"whitespace only native", map[string]string{HeaderClaudeCodeSession: "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.Reset()
			sink := &sessionSink{}
			srv := newSessionSink(t, sink)
			// Passthrough policy: with no usable carrier the request must stay
			// sessionless rather than acquiring anything.
			setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

			if code := forwardWithHolder(t, "m", "/messages", false, tc.headers).Code; code != 200 {
				t.Fatalf("status = %d, want 200", code)
			}
			for _, name := range []string{
				HeaderClaudeCodeSession, HeaderOpencodeSession,
				HeaderSessionAffinity, HeaderSessionID, HeaderSessionIDBare,
			} {
				if got := sink.get(0, name); got != "" {
					t.Errorf("%s = %q, want empty: an unusable carrier must not travel", name, got)
				}
			}
		})
	}
}

// An unusable higher-precedence carrier must not shadow a usable lower one.
func TestForwardUnusableCarrierFallsThroughToNext(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

	if code := forwardWithHolder(t, "m", "/messages", false, map[string]string{
		HeaderClaudeCodeSession: strings.Repeat("a", maxSessionTokenLen+1), // unusable
		HeaderSessionAffinity:   "usable-generic",
	}).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := sink.get(0, HeaderOpencodeSession); got != "usable-generic" {
		t.Errorf("mapped session = %q, want usable-generic", got)
	}
	if got := sink.get(0, HeaderClaudeCodeSession); got != "" {
		t.Errorf("X-Claude-Code-Session-Id = %q, want empty (the client's value was unusable)", got)
	}
}

// ─── L/M/N: missing-session policy ──────────────────────────────────────────

// L. Default policy is unchanged behavior: no client session, no injection.
func TestForwardPassthroughPolicyStaysSessionless(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

	if code := forwardWithHolder(t, "m", "/messages", false, nil).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := sink.get(0, HeaderOpencodeSession); got != "" {
		t.Errorf("X-Opencode-Session = %q, want empty under the default passthrough policy", got)
	}
}

// M + N. synthetic_request mints a canonical UUID and carries it ONLY through the
// provider's configured header. This is the production case: a client that
// carried no session at all reaching a backend that rejects sessionless requests.
func TestForwardSyntheticRequestPolicyInjectsCanonicalUUID(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", config.SessionMissingPolicyRaw, srv.URL)

	if code := forwardWithHolder(t, "m", "/messages", false, nil).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	got := sink.get(0, HeaderOpencodeSession)
	if !canonicalUUID(got) {
		t.Errorf("X-Opencode-Session = %q, want a bare canonical UUID", got)
	}
	// A synthetic identity must never masquerade as a client conversation id.
	if v := sink.get(0, HeaderClaudeCodeSession); v != "" {
		t.Errorf("X-Claude-Code-Session-Id = %q, want empty: a synthetic session must not pose as a native one", v)
	}
	for _, name := range []string{HeaderSessionAffinity, HeaderSessionID, HeaderSessionIDBare} {
		if v := sink.get(0, name); v != "" {
			t.Errorf("%s = %q, want empty: synthetic travels only through the configured header", name, v)
		}
	}
}

// A real client session must be preserved even when the policy would allow a
// synthetic one — the fallback is for absence, never a replacement.
func TestForwardSyntheticPolicyDoesNotOverrideRealSession(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", config.SessionMissingPolicyRaw, srv.URL)

	if code := forwardWithHolder(t, "m", "/messages", false, map[string]string{
		HeaderClaudeCodeSession: "real-session",
	}).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := sink.get(0, HeaderOpencodeSession); got != "real-session" {
		t.Errorf("mapped session = %q, want real-session", got)
	}
}

// synthetic_request without a header to carry it must do nothing rather than
// silently generate an identity that goes nowhere. (Admin write refuses this
// combination; a hand-edited config still must not misbehave.)
func TestForwardSyntheticPolicyWithoutSessionHeaderInjectsNothing(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "", config.SessionMissingPolicyRaw, srv.URL)

	if code := forwardWithHolder(t, "m", "/messages", false, nil).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, name := range []string{
		HeaderClaudeCodeSession, HeaderOpencodeSession,
		HeaderSessionAffinity, HeaderSessionID, HeaderSessionIDBare,
	} {
		if got := sink.get(0, name); got != "" {
			t.Errorf("%s = %q, want empty", name, got)
		}
	}
}

// Y. A provider with no SessionHeader receives no injected session even when the
// client sent one — mapping is opt-in per provider.
func TestForwardProviderWithoutSessionHeaderGetsNoMapping(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "", "", srv.URL)

	if code := forwardWithHolder(t, "m", "/messages", false, map[string]string{
		HeaderSessionAffinity: "conv-1",
	}).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := sink.get(0, HeaderOpencodeSession); got != "" {
		t.Errorf("X-Opencode-Session = %q, want empty for an unmapped provider", got)
	}
	// The client's own carrier is still preserved: that is pass-through, not mapping.
	if got := sink.get(0, HeaderSessionAffinity); got != "conv-1" {
		t.Errorf("X-Session-Affinity = %q, want conv-1", got)
	}
}

// ─── O/P: one synthetic per logical request ─────────────────────────────────

// O. Key rotation within a provider: every attempt of ONE request presents the
// SAME synthetic identity. A per-attempt UUID would make each retry look like a
// new conversation to the backend.
func TestSyntheticSessionStableAcrossConnectionRotation(t *testing.T) {
	metrics.Reset()
	var mu sync.Mutex
	var seen []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(HeaderOpencodeSession))
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// 429 is retryable and triggers the next connection.
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer srv.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	provider := config.UpstreamProvider{
		ID: "up-0", Name: "provider-0", BaseURL: srv.URL, Enabled: true,
		SessionHeader:        "X-Opencode-Session",
		SessionMissingPolicy: config.SessionMissingPolicyRaw,
		Connections: []config.UpstreamConnection{
			{ID: "c1", Name: "k1", ApiKey: "k1", Enabled: true},
			{ID: "c2", Name: "k2", ApiKey: "k2", Enabled: true},
		},
	}
	route := config.ModelRoute{ID: "route-1", Model: "m", Enabled: true,
		Targets: []config.RouteTarget{{UpstreamID: "up-0", Priority: 0, Weight: 1, Enabled: true}}}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	forwardWithHolder(t, "m", "/messages", false, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("upstream attempts = %d, want at least 2 (rotation did not happen)", len(seen))
	}
	if !canonicalUUID(seen[0]) {
		t.Fatalf("attempt 0 session = %q, want a canonical UUID", seen[0])
	}
	for i, v := range seen {
		if v != seen[0] {
			t.Errorf("attempt %d session = %q, want %q: one logical request is one session", i, v, seen[0])
		}
	}
}

// P. Failover to another provider keeps the SAME synthetic identity, provided
// the destination maps sessions at all.
func TestSyntheticSessionStableAcrossFailover(t *testing.T) {
	metrics.Reset()
	var mu sync.Mutex
	var seen []string
	record := func(r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(HeaderOpencodeSession))
		mu.Unlock()
	}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer backup.Close()

	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", config.SessionMissingPolicyRaw, primary.URL, backup.URL)
	if code := forwardWithHolder(t, "m", "/messages", false, nil).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("upstream attempts = %d, want 2", len(seen))
	}
	if !canonicalUUID(seen[0]) {
		t.Fatalf("primary session = %q, want a canonical UUID", seen[0])
	}
	if seen[0] != seen[1] {
		t.Errorf("failover session = %q, want %q: failover is the same client turn", seen[1], seen[0])
	}
}

// A destination provider that maps no session must not receive the synthetic
// identity just because the previous target in the ladder did.
func TestSyntheticSessionNotLeakedToUnmappedFailoverTarget(t *testing.T) {
	metrics.Reset()
	var mu sync.Mutex
	var backupHeaders http.Header
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backupHeaders = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer backup.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	ups := []config.UpstreamProvider{
		{ID: "up-0", Name: "mapped", BaseURL: primary.URL, ApiKey: "s", Enabled: true,
			SessionHeader: "X-Opencode-Session", SessionMissingPolicy: config.SessionMissingPolicyRaw},
		{ID: "up-1", Name: "unmapped", BaseURL: backup.URL, ApiKey: "s", Enabled: true},
	}
	route := config.ModelRoute{ID: "route-1", Model: "m", Enabled: true, Targets: []config.RouteTarget{
		{UpstreamID: "up-0", Priority: 0, Weight: 1, Enabled: true},
		{UpstreamID: "up-1", Priority: 1, Weight: 1, Enabled: true},
	}}
	if err := config.UpdateUpstreamConfig(ups, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	if code := forwardWithHolder(t, "m", "/messages", false, nil).Code; code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{
		HeaderClaudeCodeSession, HeaderOpencodeSession,
		HeaderSessionAffinity, HeaderSessionID, HeaderSessionIDBare,
	} {
		if got := backupHeaders.Get(name); got != "" {
			t.Errorf("unmapped failover target received %s = %q, want empty", name, got)
		}
	}
}

// ─── S: per-request isolation ───────────────────────────────────────────────

// S. Concurrent requests get DISTINCT synthetic identities. Two conversations
// must never be merged into one upstream session by the fallback.
func TestSyntheticSessionDistinctPerConcurrentRequest(t *testing.T) {
	metrics.Reset()
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get(HeaderOpencodeSession)]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer srv.Close()

	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", config.SessionMissingPolicyRaw, srv.URL)

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			body := `{"model":"m","messages":[]}`
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
			r = withRequestSyntheticSession(r)
			h := &Handler{}
			h.tryForwardUpstream(r, rec, []byte(body), "m", false, "/messages", true, "")
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Errorf("distinct synthetic sessions = %d, want %d (%v)", len(seen), n, seen)
	}
	for id, count := range seen {
		if !canonicalUUID(id) {
			t.Errorf("synthetic session %q is not a canonical UUID", id)
		}
		if count != 1 {
			t.Errorf("session %q used by %d requests, want 1", id, count)
		}
	}
}

// The holder generates its value once and only once, however many consumers ask.
func TestRequestSyntheticSessionIsStableAndLazy(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if got := requestSyntheticSessionID(r.Context()); got != "" {
		t.Errorf("no holder attached should yield %q, got %q", "", got)
	}
	r = withRequestSyntheticSession(r)
	first := requestSyntheticSessionID(r.Context())
	if !canonicalUUID(first) {
		t.Fatalf("synthetic id = %q, want a canonical UUID", first)
	}
	for i := 0; i < 5; i++ {
		if got := requestSyntheticSessionID(r.Context()); got != first {
			t.Fatalf("call %d returned %q, want the stable %q", i, got, first)
		}
	}
	// A second request gets its own identity.
	other := withRequestSyntheticSession(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if second := requestSyntheticSessionID(other.Context()); second == first {
		t.Error("two requests share one synthetic identity; each must get its own")
	}
}

// The one shared logical request boundary attaches the holder for every
// inference endpoint, which is what makes the fallback available at all.
func TestServeInferenceAttachesSyntheticHolder(t *testing.T) {
	h := &Handler{}
	var got string
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	h.serveInference(rec, r, "claude",
		func(w http.ResponseWriter, r *http.Request) *http.Request { return r },
		func(w http.ResponseWriter, r *http.Request) { got = requestSyntheticSessionID(r.Context()) },
	)
	if !canonicalUUID(got) {
		t.Errorf("serveInference handler saw synthetic id %q, want a canonical UUID", got)
	}
}

// ─── U/V/W/X: wire modes and endpoint families ──────────────────────────────

// U + V/W/X. The policy is endpoint- and wire-mode-agnostic: it lives at egress,
// so all three inference families and both wire modes behave identically. The
// OpenAI families previously had no session participation at all.
func TestSyntheticSessionAcrossEndpointsAndWireModes(t *testing.T) {
	for _, subPath := range []string{"/messages", "/chat/completions", "/responses"} {
		for _, stream := range []bool{false, true} {
			name := fmt.Sprintf("%s stream=%v", subPath, stream)
			t.Run(name, func(t *testing.T) {
				metrics.Reset()
				sink := &sessionSink{}
				if stream {
					sink.body = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				srv := newSessionSink(t, sink)
				setupSessionPolicyRoute(t, "m", "X-Opencode-Session", config.SessionMissingPolicyRaw, srv.URL)

				forwardWithHolder(t, "m", subPath, stream, nil)
				if sink.count() == 0 {
					t.Fatal("upstream saw no request")
				}
				if got := sink.get(0, HeaderOpencodeSession); !canonicalUUID(got) {
					t.Errorf("X-Opencode-Session = %q, want a canonical UUID", got)
				}
			})
		}
	}
}

// A real generic carrier must reach the upstream on the OpenAI families too.
func TestGenericCarrierReachesOpenAIEndpoints(t *testing.T) {
	for _, subPath := range []string{"/chat/completions", "/responses"} {
		t.Run(subPath, func(t *testing.T) {
			metrics.Reset()
			sink := &sessionSink{}
			srv := newSessionSink(t, sink)
			setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

			forwardWithHolder(t, "m", subPath, false, map[string]string{HeaderSessionAffinity: "conv-9"})
			if got := sink.get(0, HeaderOpencodeSession); got != "conv-9" {
				t.Errorf("mapped session = %q, want conv-9", got)
			}
		})
	}
}

// ─── Q: local web-search rounds ─────────────────────────────────────────────

// Q. The local web-search loop resolves the EFFECTIVE session once against the
// pinned provider and replays it, so a synthetic identity covers every
// continuation round of the one logical request.
//
// This is the regression that matters most in the loop: it resolved the identity
// with the plain client resolver, which knows nothing about provider policy, so a
// sessionless request reached a session-demanding provider with nothing — and
// minting per round would have been just as wrong, making each continuation look
// like a new conversation.
func TestForwardLocalWebSearchSyntheticSessionStableAcrossRounds(t *testing.T) {
	metrics.Reset()
	metrics.ResetToolStats()

	searxng := newFakeSearxng(t, []map[string]interface{}{
		{"url": "https://go.dev/dl/", "title": "Go downloads", "content": "Latest stable: go1.26", "engine": "duckduckgo", "score": 1.0},
	})
	upstream := newFakeUpstream(t,
		fakeUpstreamRound{body: `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model",
			"content":[
				{"type":"text","text":"Looking that up."},
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
		WebSearchStrategy:    "local",
		SessionHeader:        "X-Opencode-Session",
		SessionMissingPolicy: config.SessionMissingPolicyRaw,
	}
	route := config.ModelRoute{
		ID: "route-local", Model: "claude-local-synth", Enabled: true,
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

	// No session carrier at all: exactly the failing production shape.
	body := nativeWebSearchRequestBody("claude-local-synth", "What is the latest Go release?")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	r = withRequestSyntheticSession(r)

	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	reqs := upstream.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(reqs))
	}
	first := upstream.header(0, HeaderOpencodeSession)
	if !canonicalUUID(first) {
		t.Fatalf("round 0 X-Opencode-Session = %q, want a canonical UUID", first)
	}
	for i := 0; i < 2; i++ {
		if got := upstream.header(i, HeaderOpencodeSession); got != first {
			t.Errorf("round %d session = %q, want %q: all rounds are one logical request", i, got, first)
		}
		if got := upstream.header(i, HeaderClaudeCodeSession); got != "" {
			t.Errorf("round %d X-Claude-Code-Session-Id = %q, want empty: synthetic must not pose as native", i, got)
		}
	}

	// The single Event the loop files must explain the identity too.
	evs, total := metrics.Events(metrics.EventFilter{})
	if total == 0 {
		t.Fatal("no event recorded for the local loop")
	}
	if evs[0].SessionSource != sessionSourceSyntheticRequest || evs[0].SessionScope != "request" {
		t.Errorf("event session = %q/%q, want synthetic-request/request", evs[0].SessionSource, evs[0].SessionScope)
	}
	if !evs[0].SessionMapped {
		t.Error("event SessionMapped = false, want true")
	}
}

// ─── T: probe isolation ─────────────────────────────────────────────────────

// T. Probe traffic keeps its own clearly-marked synthetic identity, and normal
// traffic's request-scoped identity never wears the probe marking. The two must
// stay distinguishable upstream.
func TestProbeAndRequestSyntheticIdentitiesAreDistinct(t *testing.T) {
	probe := probeSessionID("req-123")
	if !strings.HasPrefix(probe, ProbeSessionPrefix) {
		t.Errorf("probe id = %q, want the %q prefix", probe, ProbeSessionPrefix)
	}
	if canonicalUUID(probe) {
		t.Error("probe id must not be a bare UUID: probe traffic stays recognizable upstream")
	}
	r := withRequestSyntheticSession(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	reqID := requestSyntheticSessionID(r.Context())
	if strings.HasPrefix(reqID, ProbeSessionPrefix) {
		t.Errorf("request synthetic id %q carries the probe prefix; policies must not cross over", reqID)
	}
	// Probe identity is derived from the probe's own request id, so the same
	// probe operation reuses one identity across its attempts.
	if again := probeSessionID("req-123"); again != probe {
		t.Errorf("probe id not stable for one operation: %q vs %q", again, probe)
	}
}

// ─── AB: public error sanitization ──────────────────────────────────────────

// AB. The upstream's MissingSessionID text is operator diagnostics, never client
// output. The client must not learn the provider's name, backend, or raw message.
func TestMissingSessionIDNotExposedToClient(t *testing.T) {
	metrics.Reset()
	const raw = "MissingSessionID: Error from provider (Console Go): Request is missing x-opencode-session and cannot be routed efficiently."
	sink := &sessionSink{status: 400, body: `{"error":{"message":"` + raw + `"}}`}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

	rec := forwardWithHolder(t, "m", "/messages", false, nil)
	body := rec.Body.String()
	for _, leak := range []string{"MissingSessionID", "Console Go", "x-opencode-session", "provider-0", srv.URL} {
		if strings.Contains(body, leak) {
			t.Errorf("client response leaked %q; body = %s", leak, body)
		}
	}
}

// ─── AC: no raw session in metrics ──────────────────────────────────────────

// AC. Observability explains the identity without recording it. Neither the raw
// value nor a hash of it may appear in the event.
func TestSessionMetricsRecordSourceAndScopeButNeverTheID(t *testing.T) {
	cases := []struct {
		name       string
		policy     string
		headers    map[string]string
		wantSource string
		wantScope  string
		wantMapped bool
		secret     string
	}{
		{"claude native", "", map[string]string{HeaderClaudeCodeSession: "sekrit-claude"},
			sessionSourceClaudeCode, "conversation", true, "sekrit-claude"},
		{"generic affinity", "", map[string]string{HeaderSessionAffinity: "sekrit-generic"},
			sessionSourceGenericAffinity, "conversation", true, "sekrit-generic"},
		{"synthetic", config.SessionMissingPolicyRaw, nil,
			sessionSourceSyntheticRequest, "request", true, ""},
		{"none", "", nil, sessionSourceNone, "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.Reset()
			sink := &sessionSink{}
			srv := newSessionSink(t, sink)
			setupSessionPolicyRoute(t, "m", "X-Opencode-Session", tc.policy, srv.URL)

			forwardWithHolder(t, "m", "/messages", false, tc.headers)

			evs, total := metrics.Events(metrics.EventFilter{})
			if total == 0 || len(evs) == 0 {
				t.Fatal("no event recorded")
			}
			ev := evs[0]
			if ev.SessionSource != tc.wantSource {
				t.Errorf("SessionSource = %q, want %q", ev.SessionSource, tc.wantSource)
			}
			if ev.SessionScope != tc.wantScope {
				t.Errorf("SessionScope = %q, want %q", ev.SessionScope, tc.wantScope)
			}
			if ev.SessionMapped != tc.wantMapped {
				t.Errorf("SessionMapped = %v, want %v", ev.SessionMapped, tc.wantMapped)
			}
			blob, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			if tc.secret != "" && strings.Contains(string(blob), tc.secret) {
				t.Errorf("event JSON contains the raw session id %q: %s", tc.secret, blob)
			}
			if tc.wantSource == sessionSourceSyntheticRequest {
				if id := sink.get(0, HeaderOpencodeSession); id != "" && strings.Contains(string(blob), id) {
					t.Errorf("event JSON contains the synthetic session id %q: %s", id, blob)
				}
			}
		})
	}
}

// ─── AD: no hidden per-API-key namespacing ──────────────────────────────────

// AD. The same session value from two different client API keys travels
// unchanged. Session identity is an untrusted routing/cache hint, so it is
// neither namespaced nor derived from the caller's credential — and upstream
// collision semantics are explicitly not an isolation guarantee Kiro-Go makes.
func TestSameSessionFromDifferentAPIKeysIsNotNamespaced(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "X-Opencode-Session", "", srv.URL)

	for _, key := range []string{"key-A", "key-B"} {
		rec := httptest.NewRecorder()
		body := `{"model":"m","messages":[]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		r.Header.Set(HeaderClaudeCodeSession, "shared-session")
		r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, key))
		r = withRequestSyntheticSession(r)
		h := &Handler{}
		if !h.tryForwardUpstream(r, rec, []byte(body), "m", false, "/messages", true, "") {
			t.Fatal("expected forward")
		}
	}
	if sink.count() != 2 {
		t.Fatalf("upstream requests = %d, want 2", sink.count())
	}
	for i := 0; i < 2; i++ {
		if got := sink.get(i, HeaderOpencodeSession); got != "shared-session" {
			t.Errorf("request %d session = %q, want the unmodified shared-session", i, got)
		}
	}
}

// ─── AE: cache/usage telemetry untouched ────────────────────────────────────

// AE. Session work must not disturb usage extraction. The same upstream payload
// yields identical token and cache figures with a session, with a synthetic
// session, and with none — the session fields are purely additive metadata.
func TestSessionPolicyDoesNotChangeUsageTelemetry(t *testing.T) {
	const usageBody = `{"id":"ok","usage":{"input_tokens":100,"output_tokens":20,` +
		`"cache_read_input_tokens":60,"cache_creation_input_tokens":15}}`

	type figures struct {
		in, out, read, create int64
	}
	run := func(t *testing.T, policy string, headers map[string]string) figures {
		t.Helper()
		metrics.Reset()
		sink := &sessionSink{body: usageBody}
		srv := newSessionSink(t, sink)
		setupSessionPolicyRoute(t, "m", "X-Opencode-Session", policy, srv.URL)
		forwardWithHolder(t, "m", "/messages", false, headers)

		evs, total := metrics.Events(metrics.EventFilter{})
		if total == 0 {
			t.Fatal("no event recorded")
		}
		ev := evs[0]
		if ev.Usage == nil {
			t.Fatal("usage breakdown missing; cache telemetry regressed")
		}
		var read, create int64
		if ev.Usage.CacheReadInputTokens != nil {
			read = *ev.Usage.CacheReadInputTokens
		}
		if ev.Usage.CacheCreationInputTokens != nil {
			create = *ev.Usage.CacheCreationInputTokens
		}
		return figures{in: ev.InputTokens, out: ev.OutputTokens, read: read, create: create}
	}

	// Canonical Anthropic input is the raw figure PLUS the cache read and cache
	// creation counts (100 + 60 + 15), which is the existing normalization this
	// test exists to pin down. The figures must be identical across all four
	// session variants — that identity, not the constants themselves, is what
	// proves session work left usage extraction alone.
	want := figures{in: 175, out: 20, read: 60, create: 15}
	cases := []struct {
		name    string
		policy  string
		headers map[string]string
	}{
		{"real session", "", map[string]string{HeaderClaudeCodeSession: "conv-1"}},
		{"generic session", "", map[string]string{HeaderSessionAffinity: "conv-2"}},
		{"synthetic session", config.SessionMissingPolicyRaw, nil},
		{"no session", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(t, tc.policy, tc.headers); got != want {
				t.Errorf("usage figures = %+v, want %+v: session work must not touch usage extraction", got, want)
			}
		})
	}
}

// ─── Z/AA/AF: config policy, round-trip, defaults ───────────────────────────

// Z. A policy that could never take effect is refused at the write boundary.
func TestValidateSessionPolicyRejectsSyntheticWithoutHeader(t *testing.T) {
	bad := config.UpstreamProvider{Name: "p", SessionMissingPolicy: config.SessionMissingPolicyRaw}
	if err := bad.ValidateSessionPolicy(); err == nil {
		t.Error("synthetic_request with no sessionHeader must be rejected")
	}
	ok := config.UpstreamProvider{Name: "p", SessionMissingPolicy: config.SessionMissingPolicyRaw, SessionHeader: "X-Opencode-Session"}
	if err := ok.ValidateSessionPolicy(); err != nil {
		t.Errorf("valid pair rejected: %v", err)
	}
	// Passthrough never needs a header.
	plain := config.UpstreamProvider{Name: "p"}
	if err := plain.ValidateSessionPolicy(); err != nil {
		t.Errorf("passthrough rejected: %v", err)
	}
}

// AF. Unknown raw values normalize to passthrough, so a typo or a config written
// by a newer build preserves the provider's existing behavior instead of
// inventing one. Load is tolerant on purpose; the admin path is where bad input
// is refused.
func TestParseSessionMissingPolicyIsTolerant(t *testing.T) {
	cases := map[string]config.SessionMissingPolicy{
		"":                   config.SessionMissingPassthrough,
		"   ":                config.SessionMissingPassthrough,
		"passthrough":        config.SessionMissingPassthrough,
		"nonsense":           config.SessionMissingPassthrough,
		"synthetic":          config.SessionMissingPassthrough,
		"synthetic_request":  config.SessionMissingSyntheticRequest,
		"SYNTHETIC_REQUEST":  config.SessionMissingSyntheticRequest,
		" synthetic_request": config.SessionMissingSyntheticRequest,
	}
	for raw, want := range cases {
		if got := config.ParseSessionMissingPolicy(raw); got != want {
			t.Errorf("ParseSessionMissingPolicy(%q) = %v, want %v", raw, got, want)
		}
	}
	if got := config.SessionMissingSyntheticRequest.String(); got != config.SessionMissingPolicyRaw {
		t.Errorf("String() = %q, want %q", got, config.SessionMissingPolicyRaw)
	}
	if got := config.SessionMissingPassthrough.String(); got != "" {
		t.Errorf("passthrough String() = %q, want empty", got)
	}
}

// The zero value must never read as conversation scope: a zero-valued session
// claiming to group a conversation is the bug the ordering prevents.
func TestSessionScopeZeroValueIsUnknown(t *testing.T) {
	var s clientSession
	if s.Scope != scopeUnknown {
		t.Errorf("zero Scope = %v, want scopeUnknown", s.Scope)
	}
	if s.Scope == scopeConversation {
		t.Error("zero Scope must not equal scopeConversation")
	}
	if got := s.Scope.String(); got != "" {
		t.Errorf("unknown scope String() = %q, want empty", got)
	}
	if scopeConversation.String() != "conversation" || scopeRequest.String() != "request" {
		t.Error("scope labels must be the stable wire spellings")
	}
}

// AA. The field survives the admin round-trip. A provider list GET must expose it
// even when empty, or the next whole-list save silently clears it for every
// provider the operator did not open.
func TestSessionMissingPolicyAdminRoundTrip(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	ups := []config.UpstreamProvider{
		{ID: "up-0", Name: "with-policy", BaseURL: "http://x", Enabled: true,
			SessionHeader: "X-Opencode-Session", SessionMissingPolicy: config.SessionMissingPolicyRaw},
		{ID: "up-1", Name: "default", BaseURL: "http://y", Enabled: true},
	}
	if err := config.UpdateUpstreamConfig(ups, nil); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	h := &Handler{}
	rec := httptest.NewRecorder()
	h.apiGetUpstreams(rec, httptest.NewRequest(http.MethodGet, "/admin/api/upstreams", nil))
	if rec.Code != 200 {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var got struct {
		Providers []map[string]interface{} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v (%s)", err, rec.Body.String())
	}
	if len(got.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(got.Providers))
	}
	for _, p := range got.Providers {
		if _, ok := p["sessionMissingPolicy"]; !ok {
			t.Errorf("provider %v omits sessionMissingPolicy; a whole-list save would clear it", p["name"])
		}
	}
	if p := got.Providers[0]; p["sessionMissingPolicy"] != config.SessionMissingPolicyRaw {
		t.Errorf("stored policy = %v, want %q", p["sessionMissingPolicy"], config.SessionMissingPolicyRaw)
	}

	// POST the list straight back (what the UI does) and confirm nothing drifted.
	payload, err := json.Marshal(map[string]interface{}{"providers": got.Providers, "routes": []interface{}{}})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	postRec := httptest.NewRecorder()
	h.apiUpdateUpstreams(postRec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams", strings.NewReader(string(payload))))
	if postRec.Code != 200 {
		t.Fatalf("POST status = %d, want 200; body = %s", postRec.Code, postRec.Body.String())
	}
	stored, _ := config.GetUpstreamConfig()
	byID := map[string]config.UpstreamProvider{}
	for _, p := range stored {
		byID[p.ID] = p
	}
	if got := byID["up-0"].SessionMissingPolicyResolved(); got != config.SessionMissingSyntheticRequest {
		t.Errorf("up-0 policy after round-trip = %v, want synthetic_request", got)
	}
	if got := byID["up-1"].SessionMissingPolicyResolved(); got != config.SessionMissingPassthrough {
		t.Errorf("up-1 policy after round-trip = %v, want passthrough (defaults must not change)", got)
	}
}

// Z (API surface). The admin write path refuses the incoherent pair rather than
// storing a setting that does nothing.
func TestAdminUpdateUpstreamsRejectsSyntheticWithoutHeader(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	body := `{"providers":[{"id":"up-0","name":"p","baseUrl":"http://x","enabled":true,` +
		`"sessionMissingPolicy":"synthetic_request"}],"routes":[]}`
	rec := httptest.NewRecorder()
	h := &Handler{}
	h.apiUpdateUpstreams(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams", strings.NewReader(body)))
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if stored, _ := config.GetUpstreamConfig(); len(stored) != 0 {
		t.Errorf("providers stored = %d, want 0: a refused save must not persist", len(stored))
	}
}
