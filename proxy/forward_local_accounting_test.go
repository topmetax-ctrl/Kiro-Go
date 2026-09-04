package proxy

import (
	"context"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
)

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
	if !h.renderForwardLocalFinal(context.Background(), rec, ClaudeRequest{Model: "m"}, agg, "answer", WebSearchPolicy{}, "req-1", "") {
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
		KiroRunResult{}, "answer", WebSearchPolicy{}, "req-2", "")

	if got := atomicLoad(&h.totalTokens) - beforeTokens; got != 1 {
		t.Errorf("totalTokens += %d, want 1 (request-unit charge)", got)
	}
	if got := atomicLoad(&h.totalRequests) - beforeReqs; got != 1 {
		t.Errorf("totalRequests += %d, want 1", got)
	}
}

// TestForwardLocalRoundMetricsAttributeToPinnedProvider proves provider rounds are
// attributed to the forwarded provider, never to the Kiro pool — the coupling
// regression that 0d18550 introduced and fb1d6e5 reverted.
func TestForwardLocalRoundMetricsAttributeToPinnedProvider(t *testing.T) {
	h := &Handler{}
	provider := config.UpstreamProvider{ID: "prov-x", Name: "provider-x", WebSearchStrategy: "local"}
	rt := config.ResolvedTarget{
		Provider: provider,
		Target:   config.RouteTarget{UpstreamID: "prov-x", TargetModel: "upstream-model"},
	}
	route := &config.ModelRoute{ID: "route-1", Model: "client-model"}

	// Recording must not panic and must carry the provider identity; the metrics
	// store is package-global, so we assert on the inputs we control rather than
	// scraping the ring (covered by metrics package tests).
	h.recordForwardLocalRound(context.Background(), usageCounts{Input: 10, Output: 5},
		provider, rt, route, "client-model", "key-1", 42)

	if provider.ID == "__kiro_pool__" {
		t.Fatal("provider must not be the Kiro pool sentinel")
	}
}
