package proxy

import (
	"context"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

// The accounting contract for a multi-round local web_search request, asserted
// against BOTH stores that count it:
//
//	handler atomics (totalRequests/successRequests/totalTokens) — client requests
//	metrics store (Overall/ProviderDetail)                      — provider outcomes
//
// For one client request that ran 3 provider rounds of 100 in / 50 out:
//
//	logical client requests    1
//	successful client requests 1
//	metrics requests           1   <- NOT 3
//	metrics modelRounds        3
//	input tokens               300 (counted once, in each store)
//	output tokens              150
//	total tokens               450 (never 900)
//
// The two stores are disjoint by construction — metrics.Record never touches the
// handler atomics and recordSuccess never calls metrics.Record — so the tests
// below check each one independently rather than assuming one implies the other.

// threeRoundConsumption is 3 rounds of 100 in / 50 out on an unpriced provider.
func threeRoundConsumption(p config.UpstreamProvider) forwardLocalConsumption {
	var c forwardLocalConsumption
	for i := 0; i < 3; i++ {
		c.addRound(usageCounts{Input: ptr(100), Output: ptr(50)}, p, 40)
	}
	return c
}

// TestForwardLocalAccountsOneLogicalRequestForManyRounds is the P1 regression:
// a local loop that ran three provider rounds is ONE client request. The token
// figures are the aggregate across rounds (provider consumption), but the
// request counters advance by exactly one.
//
// It calls renderForwardLocalFinal directly — the single completion point where
// logical accounting now lives — with an aggregate that stands in for three
// rounds of 100/50 tokens.
func TestForwardLocalAccountsOneLogicalRequestForManyRounds(t *testing.T) {
	h := &Handler{}
	before := struct{ reqs, success, tokens int64 }{
		atomicLoad(&h.totalRequests),
		atomicLoad(&h.successRequests),
		atomicLoad(&h.totalTokens),
	}

	agg := KiroRunResult{
		// three rounds: 100+100+100 in, 50+50+50 out
		TotalInputTokens:  300,
		TotalOutputTokens: 150,
		SearchCalls:       2,
		BackendExecutions: 2,
		Searches: []WebSearchInvocation{
			{ToolUseID: "srvtoolu_1", Query: "a"},
			{ToolUseID: "srvtoolu_2", Query: "b"},
		},
	}
	rec := httptest.NewRecorder()
	if !h.renderForwardLocalFinal(context.Background(), rec, ClaudeRequest{Model: "m"}, agg, "answer", WebSearchPolicy{}, "req-1", "", "prov-x") {
		t.Fatal("renderForwardLocalFinal must own the response")
	}

	gotReqs := atomicLoad(&h.totalRequests) - before.reqs
	gotSuccess := atomicLoad(&h.successRequests) - before.success
	gotTokens := atomicLoad(&h.totalTokens) - before.tokens

	if gotReqs != 1 {
		t.Errorf("totalRequests += %d, want 1 (one client request, not one per round)", gotReqs)
	}
	if gotSuccess != 1 {
		t.Errorf("successRequests += %d, want 1", gotSuccess)
	}
	if gotTokens != 450 {
		t.Errorf("totalTokens += %d, want 450 (sum of all rounds)", gotTokens)
	}
}

// TestForwardLocalChargesRequestUnitWhenUpstreamReportsNoUsage pins the fallback:
// an upstream that reports no usage at all still advances the key's quota by one
// request unit rather than recording a free request.
func TestForwardLocalChargesRequestUnitWhenUpstreamReportsNoUsage(t *testing.T) {
	h := &Handler{}
	beforeTokens := atomicLoad(&h.totalTokens)
	beforeReqs := atomicLoad(&h.totalRequests)

	rec := httptest.NewRecorder()
	h.renderForwardLocalFinal(context.Background(), rec, ClaudeRequest{Model: "m"},
		KiroRunResult{}, "answer", WebSearchPolicy{}, "req-2", "", "prov-x")

	if got := atomicLoad(&h.totalTokens) - beforeTokens; got != 1 {
		t.Errorf("totalTokens += %d, want 1 (request-unit charge)", got)
	}
	if got := atomicLoad(&h.totalRequests) - beforeReqs; got != 1 {
		t.Errorf("totalRequests += %d, want 1", got)
	}
}

// TestForwardLocalMetricsRecordOneRequestForManyRounds is the metrics-store half
// of the accounting contract, and the regression the per-round Event design would
// have broken: three model rounds must appear as ONE request with modelRounds=3,
// not as three requests.
func TestForwardLocalMetricsRecordOneRequestForManyRounds(t *testing.T) {
	metrics.Reset()
	h := &Handler{}
	provider := config.UpstreamProvider{ID: "prov-x", Name: "provider-x", WebSearchStrategy: "local"}
	rt := config.ResolvedTarget{
		Provider: provider,
		Target:   config.RouteTarget{UpstreamID: "prov-x", TargetModel: "upstream-model"},
	}
	route := &config.ModelRoute{ID: "route-1", Model: "client-model"}

	h.recordForwardLocalRequest(context.Background(), threeRoundConsumption(provider),
		provider, rt, route, "client-model", "key-1")

	ov := metrics.Overall()
	if ov.Requests != 1 {
		t.Errorf("metrics requests = %d, want 1 (three rounds are one request)", ov.Requests)
	}
	if ov.Success != 1 {
		t.Errorf("metrics success = %d, want 1", ov.Success)
	}
	if ov.ModelRounds != 3 {
		t.Errorf("metrics modelRounds = %d, want 3 (provider consumption dimension)", ov.ModelRounds)
	}
	if ov.InputTokens != 300 || ov.OutputTokens != 150 {
		t.Errorf("metrics tokens = %d/%d, want 300/150 (summed once, never doubled)", ov.InputTokens, ov.OutputTokens)
	}

	d, ok := metrics.ProviderDetailFor("prov-x", 60)
	if !ok {
		t.Fatal("pinned provider missing from metrics")
	}
	if d.Requests != 1 || d.ModelRounds != 3 {
		t.Errorf("provider requests/modelRounds = %d/%d, want 1/3", d.Requests, d.ModelRounds)
	}
	if d.InputTokens != 300 || d.OutputTokens != 150 {
		t.Errorf("provider tokens = %d/%d, want 300/150", d.InputTokens, d.OutputTokens)
	}
}

// TestForwardLocalTokensCountedOnceAcrossBothStores runs the complete terminal
// sequence — one provider Event, then the single completion point — and proves the
// 900-token double count is impossible: each store sees 450 exactly once, and the
// metrics store still reports one request.
func TestForwardLocalTokensCountedOnceAcrossBothStores(t *testing.T) {
	metrics.Reset()
	h := &Handler{}
	provider := config.UpstreamProvider{ID: "prov-x", Name: "provider-x"}
	rt := config.ResolvedTarget{Provider: provider, Target: config.RouteTarget{UpstreamID: "prov-x"}}
	route := &config.ModelRoute{ID: "route-1"}

	h.recordForwardLocalRequest(context.Background(), threeRoundConsumption(provider),
		provider, rt, route, "client-model", "key-1")
	rec := httptest.NewRecorder()
	h.renderForwardLocalFinal(context.Background(), rec, ClaudeRequest{Model: "client-model"},
		KiroRunResult{TotalInputTokens: 300, TotalOutputTokens: 150}, "answer",
		WebSearchPolicy{}, "req-3", "", provider.ID)

	if got := atomicLoad(&h.totalTokens); got != 450 {
		t.Errorf("handler totalTokens = %d, want 450 (not 900)", got)
	}
	if got := atomicLoad(&h.totalRequests); got != 1 {
		t.Errorf("handler totalRequests = %d, want 1", got)
	}
	ov := metrics.Overall()
	if got := ov.InputTokens + ov.OutputTokens; got != 450 {
		t.Errorf("metrics tokens = %d, want 450 (not 900)", got)
	}
	if ov.Requests != 1 {
		t.Errorf("metrics requests = %d, want 1", ov.Requests)
	}
}

// TestForwardLocalFailureKeepsConsumptionAndDoesNotFakeSuccess covers the failure
// case the user called out: rounds consumed before a later-round failure must
// still be attributed, the outcome must be a failure (never a success), and tool
// usage already performed must be counted exactly once.
func TestForwardLocalFailureKeepsConsumptionAndDoesNotFakeSuccess(t *testing.T) {
	metrics.Reset()
	metrics.ResetToolStats()
	h := &Handler{}
	provider := config.UpstreamProvider{ID: "prov-x", Name: "provider-x"}
	rt := config.ResolvedTarget{Provider: provider, Target: config.RouteTarget{UpstreamID: "prov-x"}}
	route := &config.ModelRoute{ID: "route-1"}

	// Two rounds consumed, one search executed, then the third round fails 429.
	var consumed forwardLocalConsumption
	consumed.addRound(usageCounts{Input: ptr(100), Output: ptr(50)}, provider, 40)
	consumed.addRound(usageCounts{Input: ptr(100), Output: ptr(50)}, provider, 40)
	agg := KiroRunResult{
		SearchCalls:         1,
		BackendExecutions:   1,
		ExecutionsByBackend: map[string]int{"searxng": 1},
		Providers:           []string{"searxng"},
	}

	h.finishForwardLocalFailure(context.Background(), consumed, agg, provider, rt, route,
		"client-model", "key-1", "req-4", 429, "rate limited")

	ov := metrics.Overall()
	if ov.Requests != 1 || ov.Failed != 1 {
		t.Errorf("metrics requests/failed = %d/%d, want 1/1", ov.Requests, ov.Failed)
	}
	if ov.Success != 0 {
		t.Errorf("metrics success = %d, want 0 (no fake success)", ov.Success)
	}
	if ov.ModelRounds != 2 {
		t.Errorf("metrics modelRounds = %d, want 2 (consumption incurred before the failure)", ov.ModelRounds)
	}
	if ov.InputTokens != 200 || ov.OutputTokens != 100 {
		t.Errorf("metrics tokens = %d/%d, want 200/100 (not lost)", ov.InputTokens, ov.OutputTokens)
	}
	if got := atomicLoad(&h.failedRequests); got != 1 {
		t.Errorf("handler failedRequests = %d, want 1", got)
	}
	if got := atomicLoad(&h.successRequests); got != 0 {
		t.Errorf("handler successRequests = %d, want 0", got)
	}

	// Tool usage counted exactly once, attributed to the backend that ran.
	st := metrics.ToolStatsFor(metrics.ToolKindWebSearch)
	if st.Uses != 1 || st.Executions != 1 {
		t.Errorf("tool uses/executions = %d/%d, want 1/1", st.Uses, st.Executions)
	}
	if st.ByBackend["searxng"] != 1 {
		t.Errorf("byBackend[searxng] = %d, want 1", st.ByBackend["searxng"])
	}
}

// TestForwardLocalCanceledRequestIsNotAFailure pins the client-disconnect rule the
// forward path already follows: a cancellation is neither success nor failure.
func TestForwardLocalCanceledRequestIsNotAFailure(t *testing.T) {
	metrics.Reset()
	h := &Handler{}
	provider := config.UpstreamProvider{ID: "prov-x", Name: "provider-x"}
	rt := config.ResolvedTarget{Provider: provider}

	h.finishForwardLocalFailure(context.Background(), forwardLocalConsumption{}, KiroRunResult{},
		provider, rt, &config.ModelRoute{ID: "r"}, "m", "k", "req-5", 499, "client canceled")

	ov := metrics.Overall()
	if ov.Canceled != 1 {
		t.Errorf("metrics canceled = %d, want 1", ov.Canceled)
	}
	if ov.Failed != 0 || ov.Success != 0 {
		t.Errorf("metrics failed/success = %d/%d, want 0/0", ov.Failed, ov.Success)
	}
	if got := atomicLoad(&h.failedRequests); got != 0 {
		t.Errorf("handler failedRequests = %d, want 0 (a disconnect is not a failure)", got)
	}
}

// TestForwardLocalRoundMetricsAttributeToPinnedProvider proves provider rounds are
// attributed to the forwarded provider, never to the Kiro pool — the coupling
// regression that 0d18550 introduced and fb1d6e5 reverted.
func TestForwardLocalRoundMetricsAttributeToPinnedProvider(t *testing.T) {
	metrics.Reset()
	h := &Handler{}
	provider := config.UpstreamProvider{ID: "prov-x", Name: "provider-x", WebSearchStrategy: "local"}
	rt := config.ResolvedTarget{
		Provider: provider,
		Target:   config.RouteTarget{UpstreamID: "prov-x", TargetModel: "upstream-model"},
	}
	route := &config.ModelRoute{ID: "route-1", Model: "client-model"}

	h.recordForwardLocalRequest(context.Background(), threeRoundConsumption(provider),
		provider, rt, route, "client-model", "key-1")

	assertKiroPoolUntouched(t, "local strategy round attribution")
	d, ok := metrics.ProviderDetailFor("prov-x", 60)
	if !ok {
		t.Fatal("pinned provider missing from metrics")
	}
	if d.ProviderName != "provider-x" {
		t.Errorf("provider name = %q, want provider-x", d.ProviderName)
	}
}
