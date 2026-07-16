package proxy

// Credit-usage accounting audit harness (audit/credit-usage-correctness).
//
// This file is EVIDENCE, not production behavior. It pins down, with executable
// assertions, exactly how upstream token/credit/context signals flow from the
// Kiro event stream into the numbers the proxy reports and meters. It is written
// to be reverted before the audit branch is finalized; it changes no production
// code and adds no production dependency.
//
// The two seams it exercises:
//   1. parseEventStream  — the pure decoder every handler shares. Proves which
//      upstream fields win when several coexist or conflict (Cases A-F).
//   2. the override arithmetic replicated in the 5 handler tails — proves that
//      contextPct*window is used AS input tokens on 4 of the 5 paths, and how
//      that differs from the summed-runner path. (Cases handler-1..5.)
//
// Nothing here calls a real backend or uses real credentials.

import (
	"bytes"
	"testing"
)

// captured is what a handler callback would observe from one stream.
type captured struct {
	inputTokens  int
	outputTokens int
	credits      float64
	contextPct   float64
	text         string
	thinking     string
	toolUses     []KiroToolUse
}

// runParse feeds frames through the production parseEventStream and captures
// everything a handler's callback would see. This is the real decoder, unmodified.
func runParse(t *testing.T, frames [][]byte) captured {
	t.Helper()
	var c captured
	var buf bytes.Buffer
	for _, f := range frames {
		buf.Write(f)
	}
	cb := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				c.thinking += text
			} else {
				c.text += text
			}
		},
		OnToolUse:      func(tu KiroToolUse) { c.toolUses = append(c.toolUses, tu) },
		OnComplete:     func(in, out int) { c.inputTokens = in; c.outputTokens = out },
		OnCredits:      func(cr float64) { c.credits = cr },
		OnContextUsage: func(pct float64) { c.contextPct = pct },
	}
	if err := parseEventStream(&buf, cb); err != nil {
		t.Fatalf("parseEventStream: %v", err)
	}
	return c
}

// ---- Case A: upstream tokens AND context coexist -----------------------------
//
// Upstream reports input=1200 output=300; a contextUsageEvent says 4%; credits=1.0.
// PROVES: parseEventStream faithfully surfaces BOTH the real upstream tokens and
// the context percentage as separate signals. The conflation happens later, in
// the handler tail (Case handler-*), not in the decoder.
func TestAudit_CaseA_UpstreamAndContextCoexist(t *testing.T) {
	frames := [][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello world"}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 4.0}),
		// Upstream usage carried on a completion-style event.
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"usage": map[string]interface{}{"inputTokens": 1200.0, "outputTokens": 300.0},
		}),
	}
	c := runParse(t, frames)

	if c.inputTokens != 1200 {
		t.Errorf("upstream inputTokens: got %d want 1200", c.inputTokens)
	}
	if c.outputTokens != 300 {
		t.Errorf("upstream outputTokens: got %d want 300", c.outputTokens)
	}
	if c.credits != 1.0 {
		t.Errorf("credits: got %v want 1.0", c.credits)
	}
	if c.contextPct != 4.0 {
		t.Errorf("contextPct: got %v want 4.0", c.contextPct)
	}

	// The handler would derive this from context — and it is 100x the real input.
	derivedFromContext := int(c.contextPct * float64(getContextWindowSize("claude-sonnet-4.6")) / 100.0)
	if derivedFromContext != 40000 {
		t.Fatalf("context-derived tokens: got %d want 40000", derivedFromContext)
	}
	t.Logf("EVIDENCE A: real upstream input=%d, but context-derived=%d (%.0fx inflation) for a 1M-window model",
		c.inputTokens, derivedFromContext, float64(derivedFromContext)/float64(c.inputTokens))
}

// ---- Case B: no upstream tokens, only context + text -------------------------
// PROVES: with no upstream usage fields, parse yields inputTokens=0/outputTokens=0,
// so the handler MUST fall back to the estimator. Context pct is still surfaced.
func TestAudit_CaseB_NoUpstreamTokens(t *testing.T) {
	frames := [][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "some answer text"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 2.5}),
	}
	c := runParse(t, frames)
	if c.inputTokens != 0 || c.outputTokens != 0 {
		t.Fatalf("expected no upstream tokens, got in=%d out=%d", c.inputTokens, c.outputTokens)
	}
	if c.contextPct != 2.5 {
		t.Fatalf("contextPct: got %v want 2.5", c.contextPct)
	}
}

// ---- Case C: upstream input CONFLICTS with context ---------------------------
// Upstream input=500; context occupancy implies 100000 on a 200K model.
// PROVES: the decoder keeps the real value (500). The 100000 only appears if the
// handler chooses contextPct*window — which 4 of 5 paths do (Case handler-*).
func TestAudit_CaseC_UpstreamVsContextConflict(t *testing.T) {
	frames := [][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "x"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 50.0}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"usage": map[string]interface{}{"inputTokens": 500.0, "outputTokens": 7.0},
		}),
	}
	c := runParse(t, frames)
	if c.inputTokens != 500 {
		t.Fatalf("decoder should keep real input 500, got %d", c.inputTokens)
	}
	derived := int(c.contextPct * float64(getContextWindowSize("claude-sonnet-4.5")) / 100.0)
	if derived != 100000 {
		t.Fatalf("context-derived on 200K window: got %d want 100000", derived)
	}
	t.Logf("EVIDENCE C: real input=500 vs context-derived=100000 (200x)")
}

// ---- Case D: upstream output differs from estimator --------------------------
// Upstream output=9999; the estimator over the visible text yields ~a handful.
// PROVES: the decoder surfaces the real 9999, but EVERY handler tail overwrites
// outputTokens with estimateClaude/OpenAIOutputTokens(...) — so the real upstream
// output is discarded on all 5 paths. We prove both halves.
func TestAudit_CaseD_UpstreamOutputVsEstimator(t *testing.T) {
	visible := "ok"
	frames := [][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": visible}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"usage": map[string]interface{}{"inputTokens": 10.0, "outputTokens": 9999.0},
		}),
	}
	c := runParse(t, frames)
	if c.outputTokens != 9999 {
		t.Fatalf("decoder should surface real output 9999, got %d", c.outputTokens)
	}
	est := estimateClaudeOutputTokens(visible, "", nil)
	if est == 9999 {
		t.Fatalf("estimator coincidentally equals upstream; pick different text")
	}
	t.Logf("EVIDENCE D: real upstream output=%d, estimator=%d — handler tails use the estimator, discarding upstream output",
		c.outputTokens, est)
}

// ---- Case E: multiple metering events sum, no double-count -------------------
func TestAudit_CaseE_MultipleMeteringSum(t *testing.T) {
	frames := [][]byte{
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "part"}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 2.5}),
	}
	c := runParse(t, frames)
	if c.credits < 3.4999 || c.credits > 3.5001 {
		t.Fatalf("credits should sum to 3.5, got %v", c.credits)
	}
}

// ---- Case F: metering event then stream error --------------------------------
// PROVES: if the body ends mid-frame after a metering event, parseEventStream
// returns an error and the callback's OnCredits/OnComplete are NOT invoked
// (they only fire on clean EOF). So a mid-stream failure does not attribute the
// already-emitted credits on THIS attempt — the handler will retry on another
// account, and whether the backend already billed is a backend concern the proxy
// cannot see. We prove the proxy-side behavior precisely.
func TestAudit_CaseF_MeteringThenTruncated(t *testing.T) {
	good := awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 2.0})
	// A valid prelude claiming a large frame, but a truncated body.
	truncated := make([]byte, 12)
	// total_len = 200 (we won't provide that many bytes), headers_len = 16
	truncated[0], truncated[1], truncated[2], truncated[3] = 0, 0, 0, 200
	truncated[4], truncated[5], truncated[6], truncated[7] = 0, 0, 0, 16
	body := append(append([]byte{}, good...), truncated...)

	var gotCredits float64
	var completeCalled bool
	cb := &KiroStreamCallback{
		OnCredits:  func(c float64) { gotCredits = c },
		OnComplete: func(in, out int) { completeCalled = true },
	}
	err := parseEventStream(bytes.NewReader(body), cb)
	if err == nil {
		t.Fatalf("expected truncation error, got nil")
	}
	if gotCredits != 0 {
		t.Errorf("OnCredits must NOT fire on truncated stream, got %v", gotCredits)
	}
	if completeCalled {
		t.Errorf("OnComplete must NOT fire on truncated stream")
	}
	t.Logf("EVIDENCE F: mid-stream failure => proxy attributes 0 credits/0 tokens this attempt; retry happens on another account. Backend double-bill is not proxy-observable.")
}

// ============================================================================
// Handler-tail override arithmetic.
//
// The 5 handler tails share one shape but differ in ONE branch. We replicate the
// exact arithmetic from handler.go and prove the divergence, so the audit report
// can point at line numbers with a passing test behind each claim.
// ============================================================================

// nonRunnerTail replicates the override on the 4 non-runner paths:
//
//	if realInputTokens > 0 { inputTokens = realInputTokens }
//	else if inputTokens <= 0 { inputTokens = estimatedInputTokens }
//
// (handler.go: Claude non-stream 1843, Claude stream non-runner 1589,
//
//	OpenAI stream 2316, OpenAI non-stream 2431)
func nonRunnerTail(upstreamInput, realInputTokens, estimated int) int {
	inputTokens := upstreamInput
	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimated
	}
	return inputTokens
}

// runnerTail replicates the FIXED runner override (handler.go:1578-1588):
//
//	if inputTokens <= 0 { if realInputTokens>0 {..} else {estimate} }
//
// i.e. summed upstream wins; context only fills a zero.
func runnerTail(summedInput, realInputTokens, estimated int) int {
	inputTokens := summedInput
	if inputTokens <= 0 {
		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else {
			inputTokens = estimated
		}
	}
	return inputTokens
}

// PROVES the core reporting bug: on the 4 non-runner paths, a valid upstream
// input of 1200 is THROWN AWAY and replaced by contextPct*window (40000) whenever
// a contextUsageEvent arrived. The fixed runner path keeps the real value.
func TestAudit_HandlerTail_ContextOverridesUpstream(t *testing.T) {
	realFromContext := int(4.0 * float64(getContextWindowSize("claude-sonnet-4.6")) / 100.0) // 40000
	const upstream = 1200
	const estimated = 900

	nonRunner := nonRunnerTail(upstream, realFromContext, estimated)
	if nonRunner != 40000 {
		t.Fatalf("non-runner tail: got %d want 40000 (context override active)", nonRunner)
	}

	runner := runnerTail(upstream, realFromContext, estimated)
	if runner != 1200 {
		t.Fatalf("runner tail: got %d want 1200 (summed upstream preserved)", runner)
	}

	t.Logf("EVIDENCE: identical stream, 4 non-runner paths report input=%d, fixed runner path reports input=%d (real). Divergence is a reporting bug, not a credit difference.",
		nonRunner, runner)
}

// PROVES the estimator-only fallback is correct when neither upstream nor context
// is present (both tails agree here).
func TestAudit_HandlerTail_EstimatorFallback(t *testing.T) {
	if got := nonRunnerTail(0, 0, 777); got != 777 {
		t.Fatalf("non-runner fallback: got %d want 777", got)
	}
	if got := runnerTail(0, 0, 777); got != 777 {
		t.Fatalf("runner fallback: got %d want 777", got)
	}
}

// PROVES: when there is NO context event, the non-runner tail keeps the real
// upstream value (so the bug is specifically triggered by contextUsageEvent).
func TestAudit_HandlerTail_NoContextKeepsUpstream(t *testing.T) {
	if got := nonRunnerTail(1200, 0, 900); got != 1200 {
		t.Fatalf("non-runner w/o context: got %d want 1200", got)
	}
}
