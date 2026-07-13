package search

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
//	  go test ./search -run TestTavilyLiveSearch -v
func TestTavilyLiveSearch(t *testing.T) {
	if os.Getenv("KIRO_WEBSEARCH_INTEGRATION") != "1" {
		t.Skip("set KIRO_WEBSEARCH_INTEGRATION=1 to run live web-search integration tests")
	}
	if strings.TrimSpace(os.Getenv("TAVILY_API_KEY")) == "" {
		t.Skip("set TAVILY_API_KEY to run live Tavily integration test")
	}

	provider := NewTavilyProvider("", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := provider.Search(ctx, Request{
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
//	  go test ./search -run TestSearXNGLiveSearch -v
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

	provider, err := NewSearXNGProvider(base, nil)
	if err != nil {
		t.Fatalf("build searxng provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := provider.Search(ctx, Request{
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
//	  go test ./search -run TestSearXNGLiveOrchestratorPipeline -v
func TestSearXNGLiveOrchestratorPipeline(t *testing.T) {
	if os.Getenv("KIRO_WEBSEARCH_INTEGRATION") != "1" {
		t.Skip("set KIRO_WEBSEARCH_INTEGRATION=1 to run live web-search integration tests")
	}
	base := strings.TrimSpace(os.Getenv("SEARXNG_BASE_URL"))
	if base == "" {
		t.Skip("set SEARXNG_BASE_URL to run live SearXNG integration test")
	}

	provider, err := NewSearXNGProvider(base, nil)
	if err != nil {
		t.Fatalf("build searxng provider: %v", err)
	}
	router := newProviderRouter([]providerEntry{{provider: provider}}, newHeuristicQualityEvaluator(3), false)
	cache := newLRUSearchCache(16)
	orch := newSearchOrchestrator(router, cache, newHeuristicReranker(), 15*time.Second, 8, "free-first", 5*time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := Request{Query: "latest stable Go release version", MaxResults: 8, SafeSearch: 1, Categories: []string{"general"}}

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
