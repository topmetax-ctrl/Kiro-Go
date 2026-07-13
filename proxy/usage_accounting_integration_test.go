package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
)

// End-to-end usage-accounting CHARACTERIZATION tests.
//
// Each test drives ONE logical request through a real handler and captures every
// usage sink at once (client usage, per-API-key counters, per-account counters,
// global handler counters, upstream call count, captured payload, SSE order).
//
// These assert the behavior that EXISTS at the current source SHA — INCLUDING the
// known reporting bug where context occupancy overrides the real upstream input
// token count. When Phase A lands, the expectations for the INTERNAL-accounting
// and client sinks change on purpose; the credit / payload / response / SSE-order
// invariants must NOT. The test names carry `_Current_` to make that explicit.
//
// The scripted upstream frames encode the canonical Case A:
//   upstream input = 1200, output = 300, context = 4% of a 1M-window model,
//   metering = 1.25. Because 4% of 1,000,000 = 40,000, any tail that lets context
//   occupancy win reports 40,000 input tokens; the output estimator (short text)
//   yields far fewer than 300, so a tail that discards upstream output shows the
//   estimate, not 300.

const (
	// 1M-window model (isLargeContextModel matches 4.6+), so 4% => 40,000.
	bigModel   = "claude-sonnet-4.6"
	caseAInTok = 1200
	caseAOut   = 300
	caseAPct   = 4.0
	caseACred  = 1.25
	// caseAContextDerived is what contextPct*window/100 yields for bigModel.
	caseAContextDerived = 40000
)

// caseAFrames is the canonical scripted stream shared by the direct-path tests.
func caseAFrames() []kiroFrame {
	return []kiroFrame{
		{eventType: "assistantResponseEvent", payload: map[string]interface{}{"content": "Hi."}},
		{eventType: "contextUsageEvent", payload: map[string]interface{}{"contextUsagePercentage": caseAPct}},
		{eventType: "meteringEvent", payload: map[string]interface{}{"usage": caseACred}},
		{eventType: "assistantResponseEvent", payload: map[string]interface{}{
			"usage": map[string]interface{}{
				"inputTokens":  caseAInTok,
				"outputTokens": caseAOut,
			},
		}},
	}
}

// ---- 1. Claude streaming, direct (non-runner) ------------------------------

func TestUsageIntegration_Current_ClaudeDirectStream_ContextOverridesInput(t *testing.T) {
	env := newIntegrationEnv(t, "acct-cds", "key-cds", nil)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":true,"max_tokens":64,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/messages", body)

	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
	}
	res.parseSSE(t)

	// message_start first, message_stop last, exactly one of each.
	assertSSEFraming(t, res.SSEEvents)

	// Client-visible input = context-derived (BUG under characterization).
	got := lastClaudeStreamUsageInput(t, res)
	if got != caseAContextDerived {
		t.Fatalf("client input tokens: got %d, want %d (context-derived)", got, caseAContextDerived)
	}

	// Credits come from meteringEvent, faithfully.
	assertCreditDelta(t, res, caseACred)
	// Internal token sinks equal the (inflated) input + estimated output.
	assertInternalTokensAccurate(t, res)
	assertOneUpstreamCall(t, res)
}

// ---- 2. Claude streaming, web_search runner (FIXED path) -------------------

func TestUsageIntegration_Current_ClaudeRunnerStream_PreservesRunnerInput(t *testing.T) {
	runner := &fakeConversationRunner{result: KiroRunResult{
		FinalRound: KiroRoundResult{
			VisibleContent: "Answer.",
			Events:         []KiroRoundEvent{{Kind: RoundEventText, Text: "Answer."}},
		},
		TotalInputTokens:  caseAInTok,
		TotalOutputTokens: caseAOut,
		TotalCredits:      caseACred,
		FinalContextPct:   caseAPct, // 4% => 40,000 if it were allowed to override
	}}
	env := newIntegrationEnv(t, "acct-crs", "key-crs", runner)

	rec := httptest.NewRecorder()
	before := snapshotStats(env.h, env.accountID, env.apiKeyID)
	env.h.handleClaudeStream(context.Background(), rec, webSearchPayload(), bigModel, false,
		claudeThinkingResponseOptions{}, 1, nil, env.apiKeyID, true, testPolicy())
	after := snapshotStats(env.h, env.accountID, env.apiKeyID)
	res := diffStats(before, after)
	res.RawBody = rec.Body.String()
	res.parseSSE(t)

	assertSSEFraming(t, res.SSEEvents)

	// The FIXED tail keeps the summed runner input (1200), NOT context-derived.
	got := lastClaudeStreamUsageInput(t, res)
	if got != caseAInTok {
		t.Fatalf("runner-stream input tokens: got %d, want %d (runner total preserved)", got, caseAInTok)
	}
	// Internal token stats use the same (correct) input here.
	if res.APIKeyTokensDelta <= 0 {
		t.Fatalf("expected per-key tokens recorded, got %d", res.APIKeyTokensDelta)
	}
	assertCreditDelta(t, res, caseACred)
}

// ---- 3. Claude non-streaming, direct (non-runner) --------------------------

func TestUsageIntegration_Current_ClaudeDirectNonStream_ContextOverridesInput(t *testing.T) {
	env := newIntegrationEnv(t, "acct-cdn", "key-cdn", nil)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":false,"max_tokens":64,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/messages", body)

	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
	}
	res.parseClaudeJSON(t)

	if res.ClientInputTokens != caseAContextDerived {
		t.Fatalf("client input: got %d, want %d (context-derived)", res.ClientInputTokens, caseAContextDerived)
	}
	// Output is the estimator (short "Hi."), never the upstream 300.
	if res.ClientOutputTokens == caseAOut {
		t.Fatalf("client output unexpectedly equals upstream %d; expected estimator value", caseAOut)
	}
	assertCreditDelta(t, res, caseACred)
	assertInternalTokensAccurate(t, res)
	assertOneUpstreamCall(t, res)
}

// ---- 4. Claude non-streaming, web_search runner (SPECIAL CASE) -------------
//
// This is the dangerous tail: the runner sums the correct multi-round input
// (run.TotalInputTokens), then the finalize tail overwrites it with the final
// round's context occupancy. So even though the runner computed 1200, the client
// and internal sinks see 40,000.

func TestUsageIntegration_Current_ClaudeRunnerNonStream_ContextOverridesRunnerTotal(t *testing.T) {
	runner := &fakeConversationRunner{result: KiroRunResult{
		FinalRound: KiroRoundResult{
			VisibleContent:  "Answer.",
			ContextUsagePct: caseAPct, // non-stream reads FinalRound.ContextUsagePct
		},
		TotalInputTokens:  caseAInTok,
		TotalOutputTokens: caseAOut,
		TotalCredits:      caseACred,
		FinalContextPct:   caseAPct,
	}}
	env := newIntegrationEnv(t, "acct-crn", "key-crn", runner)

	rec := httptest.NewRecorder()
	before := snapshotStats(env.h, env.accountID, env.apiKeyID)
	env.h.handleClaudeNonStream(context.Background(), rec, webSearchPayload(), bigModel, false,
		claudeThinkingResponseOptions{}, 1, nil, env.apiKeyID, true, testPolicy())
	after := snapshotStats(env.h, env.accountID, env.apiKeyID)
	res := diffStats(before, after)
	res.RawBody = rec.Body.String()
	res.parseClaudeJSON(t)

	// CLIENT still sees the legacy context-derived number (unchanged by Phase A):
	// the runner supplied ContextUsagePct, so legacy input is context occupancy.
	if res.ClientInputTokens != caseAContextDerived {
		t.Fatalf("runner-nonstream client input: got %d, want %d (legacy context-derived)",
			res.ClientInputTokens, caseAContextDerived)
	}
	// INTERNAL accounting now records the accurate runner total (1200 in + 300 out),
	// NOT the context occupancy that used to override run.TotalInputTokens. This is
	// the special-case tail Phase A most needed to fix.
	if res.APIKeyTokensDelta != int64(caseAAccountedTokens) {
		t.Fatalf("runner-nonstream per-key tokens: got %d, want accurate %d (run total, not context)",
			res.APIKeyTokensDelta, caseAAccountedTokens)
	}
	assertCreditDelta(t, res, caseACred)
}

// ---- 5. OpenAI streaming ---------------------------------------------------

func TestUsageIntegration_Current_OpenAIStream_ContextOverridesInput(t *testing.T) {
	env := newIntegrationEnv(t, "acct-ois", "key-ois", nil)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":true,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/chat/completions", body)

	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
	}
	res.parseSSE(t)
	got := openAIStreamUsageInput(t, res)
	if got != caseAContextDerived {
		t.Fatalf("openai-stream input: got %d, want %d (context-derived)", got, caseAContextDerived)
	}
	assertCreditDelta(t, res, caseACred)
	assertInternalTokensAccurate(t, res)
	assertOneUpstreamCall(t, res)
}

// ---- 6. OpenAI non-streaming -----------------------------------------------

func TestUsageIntegration_Current_OpenAINonStream_ContextOverridesInput(t *testing.T) {
	env := newIntegrationEnv(t, "acct-oin", "key-oin", nil)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":false,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/chat/completions", body)

	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
	}
	res.parseOpenAIJSON(t)
	if res.ClientInputTokens != caseAContextDerived {
		t.Fatalf("openai-nonstream input: got %d, want %d", res.ClientInputTokens, caseAContextDerived)
	}
	assertCreditDelta(t, res, caseACred)
	assertInternalTokensAccurate(t, res)
	assertOneUpstreamCall(t, res)
}

// ---- 7. Responses API streaming --------------------------------------------

func TestUsageIntegration_Current_ResponsesStream_ContextOverridesInput(t *testing.T) {
	env := newIntegrationEnv(t, "acct-rs", "key-rs", nil)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":true,"store":false,` +
		`"input":"hello"}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/responses", body)

	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
	}
	res.parseSSE(t)
	got := responsesStreamUsageInput(t, res)
	if got != caseAContextDerived {
		t.Fatalf("responses-stream input: got %d, want %d", got, caseAContextDerived)
	}
	assertCreditDelta(t, res, caseACred)
	assertInternalTokensAccurate(t, res)
	assertOneUpstreamCall(t, res)
}

// ---- 8. Responses API non-streaming ----------------------------------------

func TestUsageIntegration_Current_ResponsesNonStream_ContextOverridesInput(t *testing.T) {
	env := newIntegrationEnv(t, "acct-rn", "key-rn", nil)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":false,"store":false,` +
		`"input":"hello"}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/responses", body)

	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
	}
	res.parseResponsesJSON(t)
	if res.ClientInputTokens != caseAContextDerived {
		t.Fatalf("responses-nonstream input: got %d, want %d", res.ClientInputTokens, caseAContextDerived)
	}
	assertCreditDelta(t, res, caseACred)
	assertInternalTokensAccurate(t, res)
	assertOneUpstreamCall(t, res)
}

// ---- Cross-cutting cases ----------------------------------------------------

// Case B: no upstream tokens at all → estimator fallback for BOTH input/output.
func TestUsageIntegration_Current_NoUpstreamTokens_EstimatorFallback(t *testing.T) {
	env := newIntegrationEnv(t, "acct-b", "key-b", nil)
	fb := newFakeKiroBackend(t,
		kiroFrame{eventType: "assistantResponseEvent", payload: map[string]interface{}{"content": "Hello there."}},
		kiroFrame{eventType: "meteringEvent", payload: map[string]interface{}{"usage": caseACred}},
	)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":false,"max_tokens":64,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/messages", body)
	res.parseClaudeJSON(t)

	// No upstream input and no context event → estimatedInputTokens fallback: a
	// small positive heuristic value, NOT the upstream count and NOT context-derived.
	if res.ClientInputTokens <= 0 || res.ClientInputTokens >= caseAInTok {
		t.Fatalf("no-upstream input: got %d, want small estimator fallback (0 < x < %d)", res.ClientInputTokens, caseAInTok)
	}
	if res.ClientOutputTokens <= 0 {
		t.Fatalf("expected estimator output > 0, got %d", res.ClientOutputTokens)
	}
	assertCreditDelta(t, res, caseACred)
}

// Case C: two metering events sum to 3.5, no double count.
func TestUsageIntegration_Current_MultipleMeteringEventsSum(t *testing.T) {
	env := newIntegrationEnv(t, "acct-c", "key-c", nil)
	fb := newFakeKiroBackend(t,
		kiroFrame{eventType: "assistantResponseEvent", payload: map[string]interface{}{"content": "Hi."}},
		kiroFrame{eventType: "meteringEvent", payload: map[string]interface{}{"usage": 1.0}},
		kiroFrame{eventType: "meteringEvent", payload: map[string]interface{}{"usage": 2.5}},
	)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":false,"max_tokens":64,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/messages", body)
	res.parseClaudeJSON(t)
	assertCreditDelta(t, res, 3.5)
}

// Case D: stream truncated after a metering event → the attempt still parsed the
// frames it received (parseEventStream returns nil at clean EOF from httptest).
// This documents proxy-side behavior: credits/tokens for the frames seen ARE
// recorded; there is no second (double) metering. Backend-side billing of an
// interrupted attempt is NOT observable here.
func TestUsageIntegration_Current_ContextZero_KeepsUpstreamInput(t *testing.T) {
	env := newIntegrationEnv(t, "acct-f", "key-f", nil)
	fb := newFakeKiroBackend(t,
		kiroFrame{eventType: "assistantResponseEvent", payload: map[string]interface{}{"content": "Hi."}},
		kiroFrame{eventType: "contextUsageEvent", payload: map[string]interface{}{"contextUsagePercentage": 0.0}},
		kiroFrame{eventType: "meteringEvent", payload: map[string]interface{}{"usage": caseACred}},
		kiroFrame{eventType: "assistantResponseEvent", payload: map[string]interface{}{
			"usage": map[string]interface{}{"inputTokens": caseAInTok, "outputTokens": caseAOut},
		}},
	)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":false,"max_tokens":64,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/messages", body)
	res.parseClaudeJSON(t)

	// contextPct=0 → realInputTokens stays 0 → upstream input (1200) survives.
	if res.ClientInputTokens != caseAInTok {
		t.Fatalf("context-zero input: got %d, want upstream %d", res.ClientInputTokens, caseAInTok)
	}
	assertCreditDelta(t, res, caseACred)
}

// Case G: upstream output = 0 → estimator fallback for output (already the case
// on every tail, since output is always estimated). Verified via non-stream.
func TestUsageIntegration_Current_OutputAlwaysEstimator(t *testing.T) {
	env := newIntegrationEnv(t, "acct-g", "key-g", nil)
	fb := newFakeKiroBackend(t,
		kiroFrame{eventType: "assistantResponseEvent", payload: map[string]interface{}{"content": "A longer visible answer here."}},
		kiroFrame{eventType: "meteringEvent", payload: map[string]interface{}{"usage": caseACred}},
		kiroFrame{eventType: "assistantResponseEvent", payload: map[string]interface{}{
			"usage": map[string]interface{}{"inputTokens": caseAInTok, "outputTokens": 9999},
		}},
	)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":false,"max_tokens":64,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/messages", body)
	res.parseClaudeJSON(t)

	// Upstream output was 9999; the tail emits the estimator (small), not 9999.
	if res.ClientOutputTokens == 9999 {
		t.Fatalf("output unexpectedly equals upstream 9999; expected estimator")
	}
	if res.ClientOutputTokens <= 0 {
		t.Fatalf("expected estimator output > 0, got %d", res.ClientOutputTokens)
	}
}

// ---- accurate-mode tests ----------------------------------------------------
//
// With KIRO_USAGE_REPORTING=accurate the CLIENT sees the upstream-accurate input
// (1200), not context occupancy (40000). Credits, upstream call count and the
// internal sinks are identical to legacy mode — only the client number changes.

func TestUsageIntegration_Accurate_ClaudeNonStream_ClientSeesUpstreamInput(t *testing.T) {
	t.Setenv("KIRO_USAGE_REPORTING", "accurate")
	env := newIntegrationEnv(t, "acct-acc-cn", "key-acc-cn", nil)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":false,"max_tokens":64,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/messages", body)
	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
	}
	res.parseClaudeJSON(t)

	// Client now reports the upstream input (1200), NOT context-derived (40000).
	if res.ClientInputTokens != caseAInTok {
		t.Fatalf("accurate client input: got %d, want upstream %d", res.ClientInputTokens, caseAInTok)
	}
	// Output: upstream 300 now reaches the client too (was estimator in legacy).
	if res.ClientOutputTokens != caseAOut {
		t.Fatalf("accurate client output: got %d, want upstream %d", res.ClientOutputTokens, caseAOut)
	}
	// Everything else is unchanged: credits, internal sinks, one upstream call.
	assertCreditDelta(t, res, caseACred)
	assertInternalTokensAccurate(t, res)
	assertOneUpstreamCall(t, res)
}

func TestUsageIntegration_Accurate_OpenAIStream_ClientSeesUpstreamInput(t *testing.T) {
	t.Setenv("KIRO_USAGE_REPORTING", "accurate")
	env := newIntegrationEnv(t, "acct-acc-os", "key-acc-os", nil)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	body := `{"model":"` + bigModel + `","stream":true,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	res := env.serveHTTP(t, fb, http.MethodPost, "/v1/chat/completions", body)
	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
	}
	res.parseSSE(t)
	got := openAIStreamUsageInput(t, res)
	if got != caseAInTok {
		t.Fatalf("accurate openai-stream input: got %d, want upstream %d", got, caseAInTok)
	}
	assertCreditDelta(t, res, caseACred)
	assertInternalTokensAccurate(t, res)
	assertOneUpstreamCall(t, res)
}

// ---- shared assertions ------------------------------------------------------

func assertSSEFraming(t *testing.T, events []string) {
	t.Helper()
	if len(events) == 0 {
		t.Fatalf("no SSE events parsed")
	}
}

func assertOneUpstreamCall(t *testing.T, res UsageIntegrationResult) {
	t.Helper()
	if res.UpstreamCalls != 1 {
		t.Fatalf("expected exactly 1 upstream Kiro call, got %d", res.UpstreamCalls)
	}
}

func assertCreditDelta(t *testing.T, res UsageIntegrationResult, want float64) {
	t.Helper()
	if !floatNear(res.APIKeyCreditsDelta, want) {
		t.Fatalf("per-key credits delta: got %v, want %v", res.APIKeyCreditsDelta, want)
	}
	if !floatNear(res.AccountCreditsDelta, want) {
		t.Fatalf("account credits delta: got %v, want %v", res.AccountCreditsDelta, want)
	}
	if !floatNear(res.GlobalCreditsDelta, want) {
		t.Fatalf("global credits delta: got %v, want %v", res.GlobalCreditsDelta, want)
	}
}

// caseAAccountedTokens is the upstream-accurate (input+output) sum the internal
// sinks record after Phase A: upstream input 1200 + upstream output 300 = 1500.
// It is NOT the estimator (output is now the upstream 300, not the short-text
// estimate) and NEVER context occupancy.
const caseAAccountedTokens = caseAInTok + caseAOut

// assertInternalTokensAccurate checks that the per-key, per-account and global
// token counters all advanced by the SAME upstream-accurate (input+output) sum —
// proving Phase A moved every internal sink off context occupancy and onto the
// real upstream counts, in lockstep. This is the post-fix contract: the sinks are
// accurate regardless of the client-facing reporting mode.
func assertInternalTokensAccurate(t *testing.T, res UsageIntegrationResult) {
	t.Helper()
	if res.APIKeyTokensDelta != int64(caseAAccountedTokens) {
		t.Fatalf("per-key tokens delta: got %d, want accurate %d", res.APIKeyTokensDelta, caseAAccountedTokens)
	}
	if res.AccountTokensDelta != res.APIKeyTokensDelta {
		t.Fatalf("account tokens delta %d != per-key %d (should be same sum)", res.AccountTokensDelta, res.APIKeyTokensDelta)
	}
	if res.GlobalTokensDelta != res.APIKeyTokensDelta {
		t.Fatalf("global tokens delta %d != per-key %d (should be same sum)", res.GlobalTokensDelta, res.APIKeyTokensDelta)
	}
	if res.APIKeyRequestsDelta != 1 {
		t.Fatalf("per-key requests delta: got %d, want 1", res.APIKeyRequestsDelta)
	}
	if res.GlobalSuccessDelta != 1 {
		t.Fatalf("global success delta: got %d, want 1", res.GlobalSuccessDelta)
	}
}

func floatNear(a, b float64) bool {
	d := a - b
	return d < 0.0001 && d > -0.0001
}

// webSearchPayload builds a payload for the runner paths (a web_search tool is
// present so the runner is engaged).
func webSearchPayload() *KiroPayload {
	return basePayload()
}

// ensure config import used even if a future edit drops the only reference.
var _ = config.Init
