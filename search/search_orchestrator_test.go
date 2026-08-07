package search

import (
	"context"
	"testing"
	"time"
)

func testOrchestrator(t *testing.T, entries []providerEntry, cache SearchCache) *searchOrchestrator {
	t.Helper()
	if cache == nil {
		cache = noopSearchCache{}
	}
	router := newProviderRouter(entries, newHeuristicQualityEvaluator(1), false)
	return newSearchOrchestrator(router, cache, newHeuristicReranker(), 0, 8, "free-first", time.Minute)
}

func TestOrchestratorReturnsRoutedResultsReranked(t *testing.T) {
	prov := &scriptedProvider{name: "searxng", resp: Response{Results: goodResults(5)}}
	o := testOrchestrator(t, []providerEntry{{provider: prov}}, nil)

	resp, meta, err := o.Search(context.Background(), Request{Query: "latest go version", MaxResults: 5})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if meta.Provider != "searxng" {
		t.Fatalf("provider=%q", meta.Provider)
	}
	if meta.CacheHit {
		t.Fatalf("first call must not be a cache hit")
	}
	if len(resp.Results) == 0 {
		t.Fatalf("expected results")
	}
}

func TestOrchestratorCacheHitSkipsProvider(t *testing.T) {
	prov := &scriptedProvider{name: "searxng", resp: Response{Results: goodResults(4)}}
	cache := newLRUSearchCache(10)
	o := testOrchestrator(t, []providerEntry{{provider: prov}}, cache)

	req := Request{Query: "cached query", MaxResults: 5}
	if _, _, err := o.Search(context.Background(), req); err != nil {
		t.Fatalf("first search: %v", err)
	}
	if prov.calls != 1 {
		t.Fatalf("expected 1 provider call, got %d", prov.calls)
	}

	_, meta, err := o.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if !meta.CacheHit {
		t.Fatalf("second identical search must be a cache hit")
	}
	if prov.calls != 1 {
		t.Fatalf("cache hit must not call the provider again, got %d calls", prov.calls)
	}
}

func TestOrchestratorEmptyResultsNotCached(t *testing.T) {
	prov := &scriptedProvider{name: "searxng", resp: Response{}} // no results
	cache := newLRUSearchCache(10)
	o := testOrchestrator(t, []providerEntry{{provider: prov}}, cache)

	req := Request{Query: "no hits", MaxResults: 5}
	if _, _, err := o.Search(context.Background(), req); err != nil {
		t.Fatalf("first: %v", err)
	}
	// A zero-result response is not cached, so the provider is called again.
	if _, meta, err := o.Search(context.Background(), req); err != nil {
		t.Fatalf("second: %v", err)
	} else if meta.CacheHit {
		t.Fatalf("empty result set must not be cached")
	}
	if prov.calls != 2 {
		t.Fatalf("expected 2 provider calls (no caching of empties), got %d", prov.calls)
	}
}
