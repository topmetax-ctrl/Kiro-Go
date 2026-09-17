package proxy

// Managed conversation session (Kiro-Session) tests.
//
// Third layer on top of upstream_session_affinity_test.go (native carriers, body
// fallback) and upstream_session_policy_test.go (generic carriers, provider
// missing-session policy). What this file pins:
//
//   - Kiro-Session is a DOWNSTREAM protocol. The gateway offers a token on the
//     response; only a client that sends it back gets cross-turn identity. The
//     header itself never reaches an upstream — the canonical value travels
//     solely through the provider's configured SessionHeader.
//   - Telemetry does not overstate what happened: a freshly issued token is
//     kiro-issued/request (offered, adoption unproven), an echoed one is
//     kiro-managed/conversation (adoption proven). These are the labels an
//     operator uses to tell a working protocol from an ignored one, so a token
//     that was never sent back must not read as conversation continuity.
//   - Any real client identity outranks a managed token, so a client can migrate
//     to its own conversation id without a stale gateway token pinning it.
//   - One logical request mints ONE id. Managed issuance and the provider
//     synthetic fallback share a single holder, so the gateway can never answer
//     the client with one id while the upstream sees another.
//   - Off by default: with the feature disabled every pre-existing behavior is
//     bit-for-bit what it was, including the synthetic fallback.

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

// forwardManaged forwards one request with a generated-session holder wired the
// way serveInference wires it for real traffic: the recorder's own header map is
// the publication target, and managed carries the sampled config flag.
func forwardManaged(t *testing.T, clientModel, subPath string, stream, managed bool, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"model":"` + clientModel + `","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1"+subPath, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	r = withRequestGeneratedSession(r, rec.Header(), managed)
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(body), clientModel, stream, subPath, subPath == "/messages", "") {
		t.Fatal("expected route to match and forward")
	}
	return rec
}

// issuedToken returns the managed token the response offered, failing when none
// was offered.
func issuedToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	got := rec.Header().Get(HeaderKiroSession)
	if got == "" {
		t.Fatal("no Kiro-Session on the response, want an issued token")
	}
	if !canonicalUUID(got) {
		t.Fatalf("Kiro-Session = %q, want a canonical UUID", got)
	}
	return got
}

// wantSessionMapped asserts whether the recorded event claims an upstream header
// actually carried the identity. Issuance and mapping are independent: a token
// can be offered to the client while no upstream header exists to carry it.
func wantSessionMapped(t *testing.T, want bool) {
	t.Helper()
	evs, _ := metrics.Events(metrics.EventFilter{})
	if len(evs) == 0 {
		t.Fatal("no recorded event")
	}
	if evs[0].SessionMapped != want {
		t.Errorf("event SessionMapped = %v, want %v", evs[0].SessionMapped, want)
	}
}

// wantSessionEvent asserts the single recorded forward event's session labels.
func wantSessionEvent(t *testing.T, wantSource, wantScope string) {
	t.Helper()
	evs, total := metrics.Events(metrics.EventFilter{})
	if total != 1 || len(evs) != 1 {
		t.Fatalf("recorded %d events, want exactly 1", total)
	}
	if evs[0].SessionSource != wantSource || evs[0].SessionScope != wantScope {
		t.Errorf("event session = %q/%q, want %q/%q",
			evs[0].SessionSource, evs[0].SessionScope, wantSource, wantScope)
	}
}

// ─── A: feature off changes nothing ─────────────────────────────────────────

// With managed sessions disabled, a sessionless request behaves exactly as
// before: the provider policy still supplies its request-scoped synthetic
// identity, and nothing is offered to the client. Deploying the binary must not
// alter an existing deployment.
func TestManagedOffKeepsSyntheticFallback(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "synthetic_request", srv.URL)

	rec := forwardManaged(t, "m", "/messages", false, false, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(HeaderKiroSession); got != "" {
		t.Errorf("Kiro-Session = %q, want none when the feature is off", got)
	}
	if got := sink.get(0, HeaderOpencodeSession); !canonicalUUID(got) {
		t.Errorf("mapped session = %q, want the synthetic UUID (unchanged behavior)", got)
	}
	wantSessionEvent(t, sessionSourceSyntheticRequest, "request")
}

// With managed sessions off and the provider on the default passthrough policy,
// a sessionless request stays sessionless — no token, no invented identity.
func TestManagedOffPassthroughStaysSessionless(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	rec := forwardManaged(t, "m", "/messages", false, false, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(HeaderKiroSession); got != "" {
		t.Errorf("Kiro-Session = %q, want none", got)
	}
	if got := sink.get(0, HeaderOpencodeSession); got != "" {
		t.Errorf("mapped session = %q, want none", got)
	}
	wantSessionEvent(t, sessionSourceNone, "")
}

// ─── B/C/D/E: first issuance ────────────────────────────────────────────────

// A sessionless request with the feature on is offered a canonical UUID, that
// same value reaches the provider, and telemetry reports kiro-issued/request:
// the token exists but the client has not proven it will use it.
func TestManagedIssuesTokenAndUsesItUpstream(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	rec := forwardManaged(t, "m", "/messages", false, true, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	id := issuedToken(t, rec)
	if got := sink.get(0, HeaderOpencodeSession); got != id {
		t.Errorf("mapped session = %q, want the issued token %q", got, id)
	}
	if got := sink.get(0, HeaderKiroSession); got != "" {
		t.Errorf("upstream saw Kiro-Session = %q; the token is a downstream contract only", got)
	}
	wantSessionEvent(t, sessionSourceKiroIssued, "request")
	wantSessionMapped(t, true)
}

// Managed issuance does not depend on the provider's missing-session policy:
// the token has a second channel (the response header), so it is worth minting
// even for a provider that maps no session at all. A provider without a
// SessionHeader still receives no session header of any kind.
func TestManagedIssuesWithoutProviderSessionHeader(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", "", "", srv.URL)

	rec := forwardManaged(t, "m", "/messages", false, true, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	_ = issuedToken(t, rec)
	for _, name := range []string{
		HeaderClaudeCodeSession, HeaderOpencodeSession,
		HeaderSessionAffinity, HeaderSessionID, HeaderSessionIDBare, HeaderKiroSession,
	} {
		if got := sink.get(0, name); got != "" {
			t.Errorf("upstream %s = %q, want empty when the provider maps no session", name, got)
		}
	}
	wantSessionEvent(t, sessionSourceKiroIssued, "request")
	// mapped=false is the honest reading: an identity exists and the client was
	// offered it, but no upstream header carried it, so this request gained no
	// upstream affinity at all. Telemetry must not imply otherwise.
	wantSessionMapped(t, false)
}

// V/W/X: the token is issued at the shared inference boundary, so all three
// endpoint families and both wire modes get it.
func TestManagedIssuanceCoversEndpointsAndStreaming(t *testing.T) {
	for _, subPath := range []string{"/messages", "/chat/completions", "/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", subPath, stream), func(t *testing.T) {
				metrics.Reset()
				sink := &sessionSink{}
				srv := newSessionSink(t, sink)
				setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

				rec := forwardManaged(t, "m", subPath, stream, true, nil)
				id := issuedToken(t, rec)
				if got := sink.get(0, HeaderOpencodeSession); got != id {
					t.Errorf("mapped session = %q, want %q", got, id)
				}
			})
		}
	}
}

// T: for a streaming response the header must be committed before the first body
// byte, or the client can never read it. The recorder captures the header map as
// the client would see it once the status line is written.
func TestManagedTokenPrecedesStreamBody(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{body: "event: message_start\ndata: {}\n\n"}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	rec := forwardManaged(t, "m", "/messages", true, true, nil)
	if rec.Body.Len() == 0 {
		t.Fatal("expected a streamed body, got none")
	}
	if got := rec.Header().Get(HeaderKiroSession); !canonicalUUID(got) {
		t.Errorf("Kiro-Session = %q after streaming, want a canonical UUID committed with the header", got)
	}
}

// ─── F/G: the echo is what creates conversation identity ────────────────────

// A client that sends a token back gets it used as canonical identity, reported
// as kiro-managed/conversation — the only state that claims cross-turn identity —
// and re-offered on the response so it does not have to remember turn one.
func TestManagedEchoBecomesConversationIdentity(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	const token = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	rec := forwardManaged(t, "m", "/messages", false, true, map[string]string{
		HeaderKiroSession: token,
	})
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := sink.get(0, HeaderOpencodeSession); got != token {
		t.Errorf("mapped session = %q, want the echoed token %q", got, token)
	}
	if got := rec.Header().Get(HeaderKiroSession); got != token {
		t.Errorf("response Kiro-Session = %q, want the same token re-offered", got)
	}
	wantSessionEvent(t, sessionSourceKiroManaged, "conversation")
}

// An echoed token is accepted even with the feature since turned off: the client
// holds a real conversation identity now, and dropping it mid-conversation would
// break affinity for a client that did exactly what was asked of it. Disabling
// stops new issuance, not honoring what is already in flight.
func TestManagedEchoHonoredWhenIssuanceDisabled(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	const token = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	forwardManaged(t, "m", "/messages", false, false, map[string]string{
		HeaderKiroSession: token,
	})
	if got := sink.get(0, HeaderOpencodeSession); got != token {
		t.Errorf("mapped session = %q, want the echoed token %q", got, token)
	}
	wantSessionEvent(t, sessionSourceKiroManaged, "conversation")
}

// ─── Kiro-Session is ingress-only ───────────────────────────────────────────

// The header the gateway owns must never appear on an upstream request, for
// either an echoed token or a freshly issued one. sessionHeaderCarriers drives
// both ingress resolution and egress reconciliation, and this is the first
// carrier where those two sets deliberately differ.
func TestManagedTokenNeverReachesUpstream(t *testing.T) {
	const token = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"echoed", map[string]string{HeaderKiroSession: token}},
		{"issued", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.Reset()
			sink := &sessionSink{}
			srv := newSessionSink(t, sink)
			setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

			forwardManaged(t, "m", "/messages", false, true, tc.headers)
			if got := sink.get(0, HeaderKiroSession); got != "" {
				t.Errorf("upstream Kiro-Session = %q, want empty (downstream contract only)", got)
			}
			// Nor promoted into any native or generic spelling the client never used.
			for _, name := range []string{
				HeaderClaudeCodeSession, HeaderSessionAffinity, HeaderSessionID, HeaderSessionIDBare,
			} {
				if got := sink.get(0, name); got != "" {
					t.Errorf("upstream %s = %q, want empty", name, got)
				}
			}
		})
	}
}

// Kiro-Session is not in the carrier list at all — the structural guarantee the
// egress reconciliation loop depends on.
func TestKiroSessionAbsentFromCarriers(t *testing.T) {
	for _, c := range sessionHeaderCarriers {
		if http.CanonicalHeaderKey(c.Header) == http.CanonicalHeaderKey(HeaderKiroSession) {
			t.Fatalf("%s must not be a session header carrier: that list also drives upstream reconciliation", HeaderKiroSession)
		}
	}
	if sessionSourceHeader(sessionSourceKiroManaged) != "" {
		t.Error("sessionSourceHeader(kiro-managed) must be empty so the web-search replay path cannot echo it upstream")
	}
}

// ─── H/I/J/K: every real identity outranks a managed token ──────────────────

// A client migrating from managed sessions to its own conversation id must not be
// pinned to the stale token, so every header carrier wins over Kiro-Session and
// no token is offered on the response.
func TestRealCarrierBeatsManagedToken(t *testing.T) {
	const token = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	for _, carrier := range sessionHeaderCarriers {
		t.Run(carrier.Header, func(t *testing.T) {
			metrics.Reset()
			sink := &sessionSink{}
			srv := newSessionSink(t, sink)
			setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

			rec := forwardManaged(t, "m", "/messages", false, true, map[string]string{
				carrier.Header:    "real-conv",
				HeaderKiroSession: token,
			})
			if got := sink.get(0, HeaderOpencodeSession); got != "real-conv" {
				t.Errorf("mapped session = %q, want real-conv (a real carrier outranks a managed token)", got)
			}
			if got := rec.Header().Get(HeaderKiroSession); got != "" {
				t.Errorf("response Kiro-Session = %q, want none when the client owns an identity", got)
			}
			wantSessionEvent(t, carrier.Source, "conversation")
		})
	}
}

// K: the strict body fallback also outranks a managed token. This is the case
// that forces publication to happen after resolution rather than at the request
// boundary: metadata.user_id is only attached once the handler has parsed the
// body, so a boundary-time decision would offer a token to a Claude Code
// conversation that already has an identity.
func TestBodyMetadataBeatsManagedToken(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	body := bodyWithMetadataUserID(t, `{"session_id":"from-body"}`)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set(HeaderKiroSession, "3f2504e0-4f89-11d3-9a0c-0305e82c3301")
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	r = withRequestGeneratedSession(r, rec.Header(), true)
	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := sink.get(0, HeaderOpencodeSession); got != "from-body" {
		t.Errorf("mapped session = %q, want from-body (verified body identity outranks a managed token)", got)
	}
	if got := rec.Header().Get(HeaderKiroSession); got != "" {
		t.Errorf("response Kiro-Session = %q, want none when the body already carries an identity", got)
	}
}

// A sessionless request whose identity lives only in the body must not be offered
// a token either — the same publication-ordering guarantee, with no competing
// Kiro-Session inbound.
func TestBodyMetadataSuppressesIssuance(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	body := bodyWithMetadataUserID(t, `{"session_id":"body-only"}`)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	r = withRequestGeneratedSession(r, rec.Header(), true)
	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)

	if got := sink.get(0, HeaderOpencodeSession); got != "body-only" {
		t.Errorf("mapped session = %q, want body-only", got)
	}
	if got := rec.Header().Get(HeaderKiroSession); got != "" {
		t.Errorf("response Kiro-Session = %q, want none", got)
	}
}

// ─── L/M: a malformed token is absent, never relayed ────────────────────────

// Kiro-Go issues canonical UUIDs and accepts nothing else, because it authored
// the value: an opaque 256-byte token would be indistinguishable from a client's
// own carrier. A malformed value is treated as absent — so a fresh valid token is
// issued — and the bad value never travels.
func TestMalformedManagedTokenIsAbsent(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"not a uuid", "conversation-42"},
		{"oversized", strings.Repeat("a", maxSessionTokenLen+1)},
		{"control chars", "3f2504e0-4f89-11d3-9a0c-0305e82c33\r\n"},
		{"uuid with suffix", "3f2504e0-4f89-11d3-9a0c-0305e82c3301-extra"},
		{"truncated uuid", "3f2504e0-4f89-11d3-9a0c"},
		{"non-hex", "zzzzzzzz-4f89-11d3-9a0c-0305e82c3301"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.Reset()
			sink := &sessionSink{}
			srv := newSessionSink(t, sink)
			setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

			rec := forwardManaged(t, "m", "/messages", false, true, map[string]string{
				HeaderKiroSession: tc.value,
			})
			id := issuedToken(t, rec)
			if id == tc.value {
				t.Fatal("issued token equals the malformed inbound value")
			}
			got := sink.get(0, HeaderOpencodeSession)
			if got != id {
				t.Errorf("mapped session = %q, want the freshly issued token %q", got, id)
			}
			if strings.Contains(got, strings.TrimSpace(tc.value)) && tc.value != "" {
				t.Errorf("malformed value %q leaked upstream in %q", tc.value, got)
			}
			wantSessionEvent(t, sessionSourceKiroIssued, "request")
		})
	}
}

// A malformed token with the feature OFF falls through to the provider policy,
// exactly as a sessionless request does. The bad value is not relayed.
func TestMalformedManagedTokenWithIssuanceOff(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "synthetic_request", srv.URL)

	rec := forwardManaged(t, "m", "/messages", false, false, map[string]string{
		HeaderKiroSession: "not-a-uuid",
	})
	if got := rec.Header().Get(HeaderKiroSession); got != "" {
		t.Errorf("Kiro-Session = %q, want none when issuance is off", got)
	}
	got := sink.get(0, HeaderOpencodeSession)
	if !canonicalUUID(got) {
		t.Errorf("mapped session = %q, want the synthetic UUID", got)
	}
	if got == "not-a-uuid" {
		t.Error("malformed inbound token was relayed upstream")
	}
	wantSessionEvent(t, sessionSourceSyntheticRequest, "request")
}

// ─── N: a non-cooperative client stays request-level ────────────────────────

// A client that ignores the token gets a different one next turn, which is why a
// freshly issued token is request-scoped. Managed mode cannot manufacture
// affinity for a client that will not participate.
func TestNonCooperativeClientGetsDistinctTokens(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	first := issuedToken(t, forwardManaged(t, "m", "/messages", false, true, nil))
	second := issuedToken(t, forwardManaged(t, "m", "/messages", false, true, nil))
	if first == second {
		t.Fatal("two independent requests were issued the same token; issuance must be per logical request")
	}
	if a, b := sink.get(0, HeaderOpencodeSession), sink.get(1, HeaderOpencodeSession); a == b {
		t.Errorf("upstream saw the same session %q twice, want one per request", a)
	}
}

// ─── one generated id per logical request ───────────────────────────────────

// The bug two holders would allow: the client is answered with one id while the
// upstream is sent another. Managed issuance and the provider synthetic fallback
// share a single holder, and issuance is ordered first, so the fallback is
// unreachable and the two values cannot diverge.
func TestManagedIssuanceSupersedesSyntheticFallback(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "synthetic_request", srv.URL)

	rec := forwardManaged(t, "m", "/messages", false, true, nil)
	id := issuedToken(t, rec)
	if got := sink.get(0, HeaderOpencodeSession); got != id {
		t.Fatalf("upstream session = %q but the client was offered %q; one request must mint one id", got, id)
	}
	wantSessionEvent(t, sessionSourceKiroIssued, "request")
}

// The holder is the unit of identity: one request yields one value however many
// reasons ask, and a different request yields a different one.
func TestGeneratedHolderMintsOncePerRequest(t *testing.T) {
	r := withRequestGeneratedSession(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), nil, true)
	first := requestGeneratedSessionID(r.Context())
	if !canonicalUUID(first) {
		t.Fatalf("generated id = %q, want a canonical UUID", first)
	}
	for i := 0; i < 3; i++ {
		if got := requestGeneratedSessionID(r.Context()); got != first {
			t.Fatalf("call %d = %q, want the stable %q", i, got, first)
		}
	}
	if got := issuedManagedSessionID(r.Context()); got != first {
		t.Errorf("managed issuance = %q, want the same holder value %q", got, first)
	}
	other := withRequestGeneratedSession(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), nil, true)
	if second := requestGeneratedSessionID(other.Context()); second == first {
		t.Error("two requests share a generated id; the holder must be request-local")
	}
}

// No holder means no generated identity at all — the guard that keeps paths
// outside serveInference (admin, non-inference) from gaining one silently.
func TestNoHolderMeansNoManagedSession(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if got := requestGeneratedSessionID(r.Context()); got != "" {
		t.Errorf("generated id = %q without a holder, want empty", got)
	}
	if got := issuedManagedSessionID(r.Context()); got != "" {
		t.Errorf("managed issuance = %q without a holder, want empty", got)
	}
}

// Issuance requires the flag: a holder attached with managed=false never mints
// for the managed reason, even though the same holder would serve a provider
// fallback.
func TestIssuanceRequiresEnabledFlag(t *testing.T) {
	r := withRequestGeneratedSession(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), nil, false)
	if got := issuedManagedSessionID(r.Context()); got != "" {
		t.Errorf("managed issuance = %q with the feature off, want empty", got)
	}
}

// Publication happens once per logical request even under concurrent resolution,
// and a nil target simply disables it rather than panicking.
func TestPublishIsOnceAndNilSafe(t *testing.T) {
	rec := httptest.NewRecorder()
	r := withRequestGeneratedSession(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), rec.Header(), true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = issuedManagedSessionID(r.Context()) }()
	}
	wg.Wait()
	if got := rec.Header().Values(HeaderKiroSession); len(got) != 1 {
		t.Errorf("Kiro-Session values = %v, want exactly one", got)
	}

	noResp := withRequestGeneratedSession(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), nil, true)
	if got := issuedManagedSessionID(noResp.Context()); !canonicalUUID(got) {
		t.Errorf("issuance with no publication target = %q, want the id to still mint", got)
	}
}

// ─── O/P: conversations never bleed ─────────────────────────────────────────

// Two conversations under one API key stay separate, interleaved and concurrent.
// This is the property API-key, IP or connection-based affinity cannot provide,
// and the reason none of them is used.
func TestManagedConversationsIsolated(t *testing.T) {
	const convA = "aaaaaaaa-4f89-11d3-9a0c-0305e82c3301"
	const convB = "bbbbbbbb-4f89-11d3-9a0c-0305e82c3301"

	t.Run("interleaved", func(t *testing.T) {
		metrics.Reset()
		sink := &sessionSink{}
		srv := newSessionSink(t, sink)
		setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

		want := []string{convA, convB, convA, convB}
		for _, token := range want {
			forwardManaged(t, "m", "/messages", false, true, map[string]string{HeaderKiroSession: token})
		}
		for i, expect := range want {
			if got := sink.get(i, HeaderOpencodeSession); got != expect {
				t.Errorf("request %d mapped session = %q, want %q", i, got, expect)
			}
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		metrics.Reset()
		sink := &sessionSink{}
		srv := newSessionSink(t, sink)
		setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

		var wg sync.WaitGroup
		counts := make([]int, 2)
		var mu sync.Mutex
		for i := 0; i < 12; i++ {
			token := convA
			idx := 0
			if i%2 == 1 {
				token, idx = convB, 1
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := httptest.NewRecorder()
				body := `{"model":"m","messages":[]}`
				r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				r.Header.Set(HeaderKiroSession, token)
				r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
				r = withRequestGeneratedSession(r, rec.Header(), true)
				h := &Handler{}
				if h.tryForwardUpstream(r, rec, []byte(body), "m", false, "/messages", true, "") {
					mu.Lock()
					counts[idx]++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if counts[0] != 6 || counts[1] != 6 {
			t.Fatalf("forwarded %v requests, want 6 per conversation", counts)
		}
		seen := map[string]int{}
		for i := 0; i < sink.count(); i++ {
			seen[sink.get(i, HeaderOpencodeSession)]++
		}
		if seen[convA] != 6 || seen[convB] != 6 {
			t.Errorf("upstream sessions = %v, want 6 of each conversation and nothing else", seen)
		}
	})
}

// ─── R/S: one logical request, one identity, everywhere ─────────────────────

// Failover to a second target keeps the SAME issued identity: retrying is still
// one client turn, and a new id per attempt would defeat the affinity the token
// exists to create.
func TestManagedIdentityStableAcrossFailover(t *testing.T) {
	metrics.Reset()
	bad := &sessionSink{status: 500}
	badSrv := newSessionSink(t, bad)
	good := &sessionSink{}
	goodSrv := newSessionSink(t, good)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", badSrv.URL, goodSrv.URL)

	rec := forwardManaged(t, "m", "/messages", false, true, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after failover", rec.Code)
	}
	id := issuedToken(t, rec)
	if got := bad.get(0, HeaderOpencodeSession); got != id {
		t.Errorf("first attempt session = %q, want %q", got, id)
	}
	if got := good.get(0, HeaderOpencodeSession); got != id {
		t.Errorf("failover attempt session = %q, want the same %q", got, id)
	}
}

// S: every round of the local web-search loop is the same client turn, so all of
// them carry the one issued identity, and the token is offered to the client
// exactly once no matter how many rounds ran.
func TestManagedIdentityStableAcrossWebSearchRounds(t *testing.T) {
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
		WebSearchStrategy: "local",
		SessionHeader:     HeaderOpencodeSession,
	}
	route := config.ModelRoute{
		ID: "route-local", Model: "claude-local-managed", Enabled: true,
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

	body := nativeWebSearchRequestBody("claude-local-managed", "What is the latest Go release?")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	r = withRequestGeneratedSession(r, rec.Header(), true)

	h := &Handler{}
	h.handleClaudeMessagesInternal(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	id := issuedToken(t, rec)
	if reqs := upstream.requests(); len(reqs) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(reqs))
	}
	for i := 0; i < 2; i++ {
		if got := upstream.header(i, HeaderOpencodeSession); got != id {
			t.Errorf("round %d session = %q, want the issued %q: all rounds are one logical request", i, got, id)
		}
		if got := upstream.header(i, HeaderKiroSession); got != "" {
			t.Errorf("round %d leaked %s upstream", i, HeaderKiroSession)
		}
	}
	if got := rec.Header().Values(HeaderKiroSession); len(got) != 1 {
		t.Errorf("Kiro-Session values = %v, want exactly one across both rounds", got)
	}
	wantSessionEvent(t, sessionSourceKiroIssued, "request")
}

// ─── AC: cache and usage telemetry untouched ────────────────────────────────

// Managed sessions create conversation identity, not a cache. Asserted
// differentially — the same upstream payload with the feature on and off must
// produce identical usage — so this pins "this feature changes nothing" rather
// than restating the normalizer's own arithmetic, which is tested elsewhere and
// is free to evolve without dragging this test with it.
func TestManagedIssuanceLeavesUsageTelemetryAlone(t *testing.T) {
	const upstreamUsage = `{"id":"ok","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":5}}`

	usageWith := func(managed bool) metrics.EventUsage {
		t.Helper()
		metrics.Reset()
		sink := &sessionSink{body: upstreamUsage}
		srv := newSessionSink(t, sink)
		setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

		forwardManaged(t, "m", "/messages", false, managed, nil)

		evs, total := metrics.Events(metrics.EventFilter{})
		if total != 1 {
			t.Fatalf("recorded %d events, want 1", total)
		}
		if evs[0].Usage == nil {
			t.Fatal("event carries no usage, want the upstream figures")
		}
		return *evs[0].Usage
	}

	off, on := usageWith(false), usageWith(true)
	if mustMarshal(t, on) != mustMarshal(t, off) {
		t.Errorf("usage differs with managed sessions enabled:\n off = %s\n  on = %s",
			mustMarshal(t, off), mustMarshal(t, on))
	}
	// And a cache figure the upstream never reported stays unreported: issuing a
	// session must never be able to look like a cache hit.
	if on.CacheCreationInputTokens != nil {
		t.Errorf("CacheCreationInputTokens = %d, want unreported (nil)", *on.CacheCreationInputTokens)
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ─── Y: the id itself is never recorded ─────────────────────────────────────

// The token is client conversation state. Source and scope explain what happened;
// the value must appear nowhere in the event, raw or hashed.
func TestManagedTokenAbsentFromMetrics(t *testing.T) {
	metrics.Reset()
	sink := &sessionSink{}
	srv := newSessionSink(t, sink)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", srv.URL)

	const token = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	forwardManaged(t, "m", "/messages", false, true, map[string]string{HeaderKiroSession: token})
	issued := forwardManaged(t, "m", "/messages", false, true, nil)
	issuedID := issuedToken(t, issued)

	evs, _ := metrics.Events(metrics.EventFilter{})
	blob, err := json.Marshal(evs)
	if err != nil {
		t.Fatalf("marshal events: %v", err)
	}
	for _, secret := range []string{token, issuedID} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("session id %q appears in the metrics JSON: %s", secret, blob)
		}
	}
}

// ─── Z: CORS must permit the round trip, not half of it ─────────────────────

// The echo protocol needs both CORS halves. Expose-Headers lets a browser READ
// the token the gateway offered; Allow-Headers lets it SEND that token back on
// the next turn. With only Expose, a browser client reads a token it can never
// use — the protocol would work in curl and every native SDK and fail silently
// in exactly one environment, which is the worst kind of gap to ship.
func TestManagedTokenAllowedAndExposedByCORS(t *testing.T) {
	mustInitConfig(t)
	for _, method := range []string{http.MethodOptions, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(method, "/v1/messages", strings.NewReader("{}"))
			r.Header.Set("Origin", "https://example.test")
			r.Header.Set("Access-Control-Request-Headers", HeaderKiroSession)
			(&Handler{}).ServeHTTP(rec, r)
			for _, h := range []string{"Access-Control-Allow-Headers", "Access-Control-Expose-Headers"} {
				got := strings.ToLower(rec.Header().Get(h))
				if !strings.Contains(got, strings.ToLower(HeaderKiroSession)) {
					t.Errorf("%s = %q, want it to list %s", h, rec.Header().Get(h), HeaderKiroSession)
				}
			}
		})
	}
}

// ─── AA: the authentication boundary ────────────────────────────────────────

// A request that fails authentication is never offered a token. The holder is
// attached after authenticate() returns a request, so a rejected caller cannot
// collect a conversation identity — which matters because an unauthenticated
// caller handed a token would be handed a cache-affinity hint it has no right
// to, and could probe for one by spraying keys.
func TestAuthFailureOffersNoManagedToken(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: "main", Key: "sk-good", Enabled: true}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	requireAuth(t)
	if err := config.UpdateManagedSessionsEnabled(true); err != nil {
		t.Fatalf("enable managed sessions: %v", err)
	}

	for _, tc := range []struct{ name, key string }{
		{"missing key", ""},
		{"wrong key", "sk-wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`))
			r.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				r.Header.Set("Authorization", "Bearer "+tc.key)
			}
			(&Handler{}).ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get(HeaderKiroSession); got != "" {
				t.Errorf("unauthenticated request was offered %q, want no token", got)
			}
		})
	}
}

// The other half: once a request has authenticated and a token has been
// published, a later failure must not retract it. The token is published into
// the shared header map before any status line is written, so a sanitized error
// response still carries it — the client keeps a usable conversation identity
// and its retry lands on the same affinity instead of starting over.
func TestManagedTokenSurvivesPostIssuanceError(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: "main", Key: "sk-good", Enabled: true}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	requireAuth(t)
	if err := config.UpdateManagedSessionsEnabled(true); err != nil {
		t.Fatalf("enable managed sessions: %v", err)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	r.Header.Set("Authorization", "Bearer sk-good")

	h := &Handler{}
	var issued string
	h.serveInference(rec, r, "claude", h.authenticateForClaude, func(w http.ResponseWriter, ar *http.Request) {
		// Stands in for resolution: the same call effectiveSessionForProvider makes.
		issued = issuedManagedSessionID(ar.Context())
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", "upstream failed")
	})

	if !canonicalUUID(issued) {
		t.Fatalf("issued token = %q, want a canonical UUID", issued)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := rec.Header().Get(HeaderKiroSession); got != issued {
		t.Errorf("Kiro-Session after the error = %q, want the issued %q", got, issued)
	}
}

// The realistic version of the same invariant on the forward path: the upstream
// answers 403, the client gets a sanitized 502, and the token still stands.
func TestManagedTokenSurvivesSanitizedUpstreamError(t *testing.T) {
	metrics.Reset()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"account suspended","code":"ACCOUNT_SUSPENDED"}}`))
	}))
	t.Cleanup(upstream.Close)
	setupSessionPolicyRoute(t, "m", HeaderOpencodeSession, "", upstream.URL)

	rec := forwardManaged(t, "m", "/messages", false, true, nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected the upstream failure to surface, got 200: %s", rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "account suspended") {
		t.Errorf("provider detail leaked to the client: %s", body)
	}
	issuedToken(t, rec)
}
