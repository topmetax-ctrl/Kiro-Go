package proxy

import (
	"context"
	"errors"
	"testing"
)

// scriptedProvider is a SearchProvider returning canned results/errors, with a
// controllable health state, for router tests.
type scriptedProvider struct {
	name  string
	resp  SearchResponse
	err   error
	calls int
	state ProviderHealthState
}

func (p *scriptedProvider) Name() string { return p.name }
func (p *scriptedProvider) Health() ProviderHealth {
	return ProviderHealth{State: p.state}
}
func (p *scriptedProvider) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	if err := ctx.Err(); err != nil {
		return SearchResponse{}, err
	}
	p.calls++
	if p.err != nil {
		return SearchResponse{}, p.err
	}
	r := p.resp
	r.Provider = p.name
	r.Query = req.Query
	return r, nil
}

func goodResults(n int) []SearchResult {
	out := make([]SearchResult, 0, n)
	hosts := []string{"a", "b", "c", "d", "e"}
	for i := 0; i < n; i++ {
		out = append(out, SearchResult{
			Title:   "T",
			URL:     "https://" + hosts[i%len(hosts)] + ".example/p",
			Content: "relevant topic content",
		})
	}
	return out
}

func TestRouterPrimarySuccessNoFallback(t *testing.T) {
	primary := &scriptedProvider{name: "searxng", resp: SearchResponse{Results: goodResults(3)}}
	fallback := &scriptedProvider{name: "tavily", resp: SearchResponse{Results: goodResults(3)}}
	r := newProviderRouter([]providerEntry{
		{provider: primary},
		{provider: fallback, paid: true, budgetAllow: func() bool { return true }},
	}, newHeuristicQualityEvaluator(2), false)

	resp, outcome, err := r.Route(context.Background(), SearchRequest{Query: "topic"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if outcome.Provider != "searxng" || outcome.FallbackUsed {
		t.Fatalf("expected primary success no fallback, got %+v", outcome)
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback must not be called when primary succeeds")
	}
	if len(resp.Results) != 3 {
		t.Fatalf("results=%d", len(resp.Results))
	}
}

func TestRouterFallsBackOnPrimaryLowQuality(t *testing.T) {
	// Primary returns 1 result but min is 3 → low quality → fall back.
	primary := &scriptedProvider{name: "searxng", resp: SearchResponse{Results: goodResults(1)}}
	fallback := &scriptedProvider{name: "tavily", resp: SearchResponse{Results: goodResults(3)}}
	r := newProviderRouter([]providerEntry{
		{provider: primary},
		{provider: fallback, paid: true, budgetAllow: func() bool { return true }},
	}, newHeuristicQualityEvaluator(3), false)

	_, outcome, err := r.Route(context.Background(), SearchRequest{Query: "topic"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if outcome.Provider != "tavily" || !outcome.FallbackUsed {
		t.Fatalf("expected fallback to tavily, got %+v", outcome)
	}
	if fallback.calls != 1 {
		t.Fatalf("fallback should have been called once, got %d", fallback.calls)
	}
}

func TestRouterFallsBackOnPrimaryError(t *testing.T) {
	primary := &scriptedProvider{name: "searxng", err: &SearchProviderError{Kind: SearchErrUpstream5xx, StatusCode: 500, Err: errors.New("boom")}}
	fallback := &scriptedProvider{name: "tavily", resp: SearchResponse{Results: goodResults(3)}}
	r := newProviderRouter([]providerEntry{
		{provider: primary},
		{provider: fallback, paid: true, budgetAllow: func() bool { return true }},
	}, newHeuristicQualityEvaluator(2), false)

	_, outcome, err := r.Route(context.Background(), SearchRequest{Query: "topic"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if outcome.Provider != "tavily" || !outcome.FallbackUsed {
		t.Fatalf("expected fallback after primary error, got %+v", outcome)
	}
}

func TestRouterFreeOnlySkipsPaidWhenBudgetExhausted(t *testing.T) {
	primary := &scriptedProvider{name: "searxng", resp: SearchResponse{Results: goodResults(1)}} // low quality
	fallback := &scriptedProvider{name: "tavily", resp: SearchResponse{Results: goodResults(3)}}
	// allowPaid=false and budget gate returns false → paid provider must be skipped.
	r := newProviderRouter([]providerEntry{
		{provider: primary},
		{provider: fallback, paid: true, budgetAllow: func() bool { return false }},
	}, newHeuristicQualityEvaluator(3), false)

	_, outcome, err := r.Route(context.Background(), SearchRequest{Query: "topic"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fallback.calls != 0 {
		t.Fatalf("paid fallback must be skipped when budget exhausted in free-only mode, calls=%d", fallback.calls)
	}
	// Router returns the primary's sub-threshold results rather than failing.
	if outcome.Provider != "searxng" {
		t.Fatalf("expected primary sub-threshold result, got %+v", outcome)
	}
}

func TestRouterAllowPaidUsesPaidEvenWhenBudgetGateFalse(t *testing.T) {
	primary := &scriptedProvider{name: "searxng", resp: SearchResponse{Results: goodResults(1)}}
	fallback := &scriptedProvider{name: "tavily", resp: SearchResponse{Results: goodResults(3)}}
	// allowPaid=true → budget gate is not consulted; paid provider is eligible.
	r := newProviderRouter([]providerEntry{
		{provider: primary},
		{provider: fallback, paid: true, budgetAllow: func() bool { return false }},
	}, newHeuristicQualityEvaluator(3), true)

	_, outcome, err := r.Route(context.Background(), SearchRequest{Query: "topic"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if outcome.Provider != "tavily" || fallback.calls != 1 {
		t.Fatalf("allowPaid should let router use paid provider, got %+v calls=%d", outcome, fallback.calls)
	}
}

func TestRouterSkipsUnhealthyPrimary(t *testing.T) {
	primary := &scriptedProvider{name: "searxng", state: ProviderUnhealthy, resp: SearchResponse{Results: goodResults(3)}}
	fallback := &scriptedProvider{name: "tavily", resp: SearchResponse{Results: goodResults(3)}}
	r := newProviderRouter([]providerEntry{
		{provider: primary},
		{provider: fallback, paid: true, budgetAllow: func() bool { return true }},
	}, newHeuristicQualityEvaluator(2), false)

	_, outcome, err := r.Route(context.Background(), SearchRequest{Query: "topic"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if primary.calls != 0 {
		t.Fatalf("unhealthy primary should be skipped when a healthy fallback exists, calls=%d", primary.calls)
	}
	if outcome.Provider != "tavily" {
		t.Fatalf("expected fallback, got %+v", outcome)
	}
}

func TestRouterTriesUnhealthyWhenOnlyOption(t *testing.T) {
	// Single unhealthy provider must still be tried (better than nothing).
	only := &scriptedProvider{name: "searxng", state: ProviderUnhealthy, resp: SearchResponse{Results: goodResults(3)}}
	r := newProviderRouter([]providerEntry{{provider: only}}, newHeuristicQualityEvaluator(2), false)
	_, _, err := r.Route(context.Background(), SearchRequest{Query: "topic"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if only.calls != 1 {
		t.Fatalf("sole unhealthy provider should still be tried, calls=%d", only.calls)
	}
}

func TestRouterContextCancelStops(t *testing.T) {
	primary := &scriptedProvider{name: "searxng", resp: SearchResponse{Results: goodResults(3)}}
	r := newProviderRouter([]providerEntry{{provider: primary}}, newHeuristicQualityEvaluator(2), false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := r.Route(ctx, SearchRequest{Query: "topic"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if primary.calls != 0 {
		t.Fatalf("no provider should be called under cancelled context")
	}
}

func TestRouterNoProvidersIsConfigError(t *testing.T) {
	r := newProviderRouter(nil, newHeuristicQualityEvaluator(2), false)
	_, _, err := r.Route(context.Background(), SearchRequest{Query: "topic"})
	var cfg *SearchConfigError
	if !errors.As(err, &cfg) {
		t.Fatalf("expected SearchConfigError for no providers, got %v", err)
	}
}
