package proxy

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTavilyLiveSearch hits the real Tavily API. It is skipped unless both
// TAVILY_API_KEY and KIRO_WEBSEARCH_INTEGRATION=1 are set, so it never runs in
// CI by default and never needs a checked-in key.
//
//	TAVILY_API_KEY=tvly-... KIRO_WEBSEARCH_INTEGRATION=1 \
//	  go test ./proxy -run TestTavilyLiveSearch -v
func TestTavilyLiveSearch(t *testing.T) {
	if os.Getenv("KIRO_WEBSEARCH_INTEGRATION") != "1" {
		t.Skip("set KIRO_WEBSEARCH_INTEGRATION=1 to run live web-search integration tests")
	}
	if strings.TrimSpace(os.Getenv("TAVILY_API_KEY")) == "" {
		t.Skip("set TAVILY_API_KEY to run live Tavily integration test")
	}

	provider := NewTavilyProvider("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := provider.Search(ctx, SearchRequest{
		Query:       "latest stable Go release version",
		MaxResults:  5,
		SearchDepth: "basic",
	})
	if err != nil {
		t.Fatalf("live tavily search failed: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatalf("expected at least one live result")
	}
	// At least one result must carry a usable http(s) URL — evidence the model
	// can cite.
	var haveURL bool
	for _, r := range resp.Results {
		if strings.HasPrefix(r.URL, "http://") || strings.HasPrefix(r.URL, "https://") {
			haveURL = true
			break
		}
	}
	if !haveURL {
		t.Fatalf("expected at least one http(s) source URL, got %+v", resp.Results)
	}
	t.Logf("live tavily returned %d results for query", len(resp.Results))
}

// TestSearXNGLiveSearch hits a real SearXNG instance (the free primary). It is
// skipped unless both KIRO_WEBSEARCH_INTEGRATION=1 and SEARXNG_BASE_URL are set,
// so it never runs in CI by default and needs no paid credentials. Point it at
// the docker-compose SearXNG (JSON format must be enabled in settings.yml):
//
//	SEARXNG_BASE_URL=http://localhost:8080 KIRO_WEBSEARCH_INTEGRATION=1 \
//	  go test ./proxy -run TestSearXNGLiveSearch -v
//
// (When running against the compose instance, temporarily publish its port or
// run this from a container on the websearch network — production omits the port
// mapping on purpose.)
func TestSearXNGLiveSearch(t *testing.T) {
	if os.Getenv("KIRO_WEBSEARCH_INTEGRATION") != "1" {
		t.Skip("set KIRO_WEBSEARCH_INTEGRATION=1 to run live web-search integration tests")
	}
	base := strings.TrimSpace(os.Getenv("SEARXNG_BASE_URL"))
	if base == "" {
		t.Skip("set SEARXNG_BASE_URL to run live SearXNG integration test")
	}

	provider, err := NewSearXNGProvider(base)
	if err != nil {
		t.Fatalf("build searxng provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := provider.Search(ctx, SearchRequest{
		Query:      "latest stable Go release version",
		MaxResults: 5,
		SafeSearch: 1,
		Categories: []string{"general"},
	})
	if err != nil {
		t.Fatalf("live searxng search failed (is JSON format enabled in settings.yml?): %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatalf("expected at least one live result from searxng")
	}
	var haveURL bool
	for _, r := range resp.Results {
		if strings.HasPrefix(r.URL, "http://") || strings.HasPrefix(r.URL, "https://") {
			haveURL = true
			break
		}
	}
	if !haveURL {
		t.Fatalf("expected at least one http(s) source URL, got %+v", resp.Results)
	}
	t.Logf("live searxng returned %d results for query", len(resp.Results))
}

// TestSearXNGLiveOrchestratorPipeline exercises the whole free-first pipeline —
// router → SearXNG → quality gate → rerank → LRU cache — against a real SearXNG
// instance, then re-runs the same query to prove the process cache serves it
// without a second provider call. Gated the same way as the other live tests.
//
//	SEARXNG_BASE_URL=http://localhost:18899 KIRO_WEBSEARCH_INTEGRATION=1 \
//	  go test ./proxy -run TestSearXNGLiveOrchestratorPipeline -v
func TestSearXNGLiveOrchestratorPipeline(t *testing.T) {
	if os.Getenv("KIRO_WEBSEARCH_INTEGRATION") != "1" {
		t.Skip("set KIRO_WEBSEARCH_INTEGRATION=1 to run live web-search integration tests")
	}
	base := strings.TrimSpace(os.Getenv("SEARXNG_BASE_URL"))
	if base == "" {
		t.Skip("set SEARXNG_BASE_URL to run live SearXNG integration test")
	}

	provider, err := NewSearXNGProvider(base)
	if err != nil {
		t.Fatalf("build searxng provider: %v", err)
	}
	router := newProviderRouter([]providerEntry{{provider: provider}}, newHeuristicQualityEvaluator(3), false)
	cache := newLRUSearchCache(16)
	orch := newSearchOrchestrator(router, cache, newHeuristicReranker(), 15*time.Second, 8, "free-first", 5*time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := SearchRequest{Query: "latest stable Go release version", MaxResults: 8, SafeSearch: 1, Categories: []string{"general"}}

	resp1, meta1, err := orch.Search(ctx, req)
	if err != nil {
		t.Fatalf("orchestrator search failed: %v", err)
	}
	if meta1.CacheHit {
		t.Fatalf("first call must not be a cache hit")
	}
	if meta1.Provider != "searxng" {
		t.Fatalf("expected searxng to serve, got %q", meta1.Provider)
	}
	if len(resp1.Results) == 0 {
		t.Fatalf("expected results from the pipeline")
	}
	if !meta1.Quality.Acceptable {
		t.Fatalf("expected acceptable quality, got %q", meta1.Quality.Reason)
	}
	// Reranked set must be capped to maxFinalResults.
	if len(resp1.Results) > 8 {
		t.Fatalf("result set not capped to 8, got %d", len(resp1.Results))
	}

	// Second identical call: must be served from cache, no provider round-trip.
	resp2, meta2, err := orch.Search(ctx, req)
	if err != nil {
		t.Fatalf("second orchestrator search failed: %v", err)
	}
	if !meta2.CacheHit {
		t.Fatalf("second identical call must be a cache hit")
	}
	if len(resp2.Results) != len(resp1.Results) {
		t.Fatalf("cache returned a different result set: %d vs %d", len(resp2.Results), len(resp1.Results))
	}
	t.Logf("pipeline ok: %d results via %s, second call cache_hit=%v", len(resp1.Results), meta1.Provider, meta2.CacheHit)
}

// TestLiveRunnerFeedsRealSearchIntoContinuation is the closest end-to-end proof
// available without a live Kiro account. It drives the real ConversationRunner
// with:
//   - a scripted KiroRoundCaller that emits a genuine web_search tool_use on
//     round 1 (as the Kiro backend does) and a final text answer on round 2, and
//   - the REAL web_search executor backed by the REAL free-first orchestrator
//     hitting the live SearXNG instance.
//
// It asserts that the runner ran an actual internet search and fed a structured
// tool_result — built from live go.dev content — back into round 2's payload,
// linked to the exact tool_use ID, with the untrusted-data framing. This is the
// whole server-tool loop minus the Kiro HTTP hop.
//
//	SEARXNG_BASE_URL=http://localhost:18899 KIRO_WEBSEARCH_INTEGRATION=1 \
//	  go test ./proxy -run TestLiveRunnerFeedsRealSearchIntoContinuation -v
func TestLiveRunnerFeedsRealSearchIntoContinuation(t *testing.T) {
	if os.Getenv("KIRO_WEBSEARCH_INTEGRATION") != "1" {
		t.Skip("set KIRO_WEBSEARCH_INTEGRATION=1 to run live web-search integration tests")
	}
	base := strings.TrimSpace(os.Getenv("SEARXNG_BASE_URL"))
	if base == "" {
		t.Skip("set SEARXNG_BASE_URL to run live SearXNG integration test")
	}

	provider, err := NewSearXNGProvider(base)
	if err != nil {
		t.Fatalf("build searxng provider: %v", err)
	}
	router := newProviderRouter([]providerEntry{{provider: provider}}, newHeuristicQualityEvaluator(3), false)
	orch := newSearchOrchestrator(router, newLRUSearchCache(16), newHeuristicReranker(), 15*time.Second, 8, "free-first", 5*time.Minute)
	executor := newWebSearchExecutor(orch)

	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: searchRound("latest stable Go release version", "tool-live-1", 100, 10, 0.01)},
		{result: textRound("The latest stable Go release is documented on go.dev [1].", 130, 25, 0.02)},
	}}
	runner := newRunnerWithDeps(caller, executor)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	out, err := runner.Run(ctx, nil, basePayload(), testPolicy())
	if err != nil {
		t.Fatalf("runner failed: %v", err)
	}

	// The runner must have performed exactly one real search round and two Kiro
	// rounds (search + finalize), and surfaced live sources.
	if out.SearchRounds != 1 || out.SearchCalls != 1 {
		t.Fatalf("expected 1 search round/call, got rounds=%d calls=%d", out.SearchRounds, out.SearchCalls)
	}
	if caller.calls != 2 {
		t.Fatalf("expected 2 Kiro rounds, got %d", caller.calls)
	}
	if len(out.Sources) == 0 {
		t.Fatalf("expected live sources from the search")
	}

	// Round 2's payload must carry the structured tool_result built from the live
	// search, keyed to the round-1 tool_use ID, with the untrusted-data framing.
	if len(caller.seen) != 2 {
		t.Fatalf("expected 2 payloads seen, got %d", len(caller.seen))
	}
	p2 := caller.seen[1]
	uctx := p2.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if uctx == nil || len(uctx.ToolResults) != 1 {
		t.Fatalf("round 2 missing structured tool_result: %+v", uctx)
	}
	tr := uctx.ToolResults[0]
	if tr.ToolUseID != "tool-live-1" {
		t.Fatalf("tool_result not keyed to the live tool_use: %q", tr.ToolUseID)
	}
	body := ""
	if len(tr.Content) > 0 {
		body = tr.Content[0].Text
	}
	if !strings.Contains(body, "WEB_SEARCH_RESULTS") || !strings.Contains(body, "untrusted external web content") {
		t.Fatalf("tool_result missing untrusted-data framing: %q", body)
	}
	if !strings.Contains(body, "http") {
		t.Fatalf("tool_result carries no source URL from the live search: %q", body)
	}
	t.Logf("live runner E2E ok: %d sources, round-2 tool_result %d bytes keyed to %s",
		len(out.Sources), len(body), tr.ToolUseID)
}
