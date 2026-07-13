package search

import (
	"testing"
	"time"
)

func TestSearchCacheKeyStableAndSensitive(t *testing.T) {
	base := Request{
		Query:      "Latest Go Version",
		Language:   "en",
		SafeSearch: 1,
		Categories: []string{"general"},
		MaxResults: 5,
	}
	k1 := SearchCacheKey(base, "free-first")

	// Same params (query case/whitespace normalized) → same key.
	same := base
	same.Query = "  latest go version  "
	if SearchCacheKey(same, "free-first") != k1 {
		t.Fatalf("key should be invariant to query case/whitespace")
	}

	// Different safesearch → different key.
	diff := base
	diff.SafeSearch = 2
	if SearchCacheKey(diff, "free-first") == k1 {
		t.Fatalf("key must change with safesearch")
	}

	// Different domain filter → different key.
	dom := base
	dom.AllowedDomains = []string{"go.dev"}
	if SearchCacheKey(dom, "free-first") == k1 {
		t.Fatalf("key must change with domain filter")
	}

	// Different routing mode → different key.
	if SearchCacheKey(base, "paid") == k1 {
		t.Fatalf("key must change with routing mode")
	}
}

func TestLRUCachePutGetTTL(t *testing.T) {
	c := newLRUSearchCache(10)
	resp := Response{Provider: "searxng", Results: goodResults(2)}
	c.Put("k", resp, time.Minute)

	got, ok := c.Get("k")
	if !ok || got.Provider != "searxng" || len(got.Results) != 2 {
		t.Fatalf("expected cached hit, got ok=%v %+v", ok, got)
	}

	// Expired entry is a miss.
	c.Put("k2", resp, time.Nanosecond)
	time.Sleep(time.Millisecond)
	if _, ok := c.Get("k2"); ok {
		t.Fatalf("expired entry should be a miss")
	}

	// Non-positive TTL is not stored.
	c.Put("k3", resp, 0)
	if _, ok := c.Get("k3"); ok {
		t.Fatalf("zero-TTL entry should not be stored")
	}
}

func TestLRUCacheEvictsOldest(t *testing.T) {
	c := newLRUSearchCache(2)
	c.Put("a", Response{Provider: "a"}, time.Minute)
	c.Put("b", Response{Provider: "b"}, time.Minute)
	// Touch "a" so "b" becomes the LRU victim.
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("a should be present")
	}
	c.Put("c", Response{Provider: "c"}, time.Minute)
	if _, ok := c.Get("b"); ok {
		t.Fatalf("b should have been evicted as LRU")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("a should survive (recently used)")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatalf("c should be present")
	}
}

func TestNoopCacheNeverStores(t *testing.T) {
	var c SearchCache = noopSearchCache{}
	c.Put("k", Response{Provider: "x"}, time.Minute)
	if _, ok := c.Get("k"); ok {
		t.Fatalf("noop cache must never return a hit")
	}
}
