package search

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SearchCache is a bounded, TTL'd cache of normalized provider responses keyed
// by the full search parameter set. It exists to avoid re-hitting a provider
// (and, for Tavily, re-spending credits) for an identical query within a short
// window. The interface admits a Redis backend for multi-replica deployments
// without touching the orchestrator.
type SearchCache interface {
	Get(key string) (Response, bool)
	Put(key string, value Response, ttl time.Duration)
}

// SearchCacheKey builds the deterministic cache key from every parameter that
// changes the result set. Two requests with the same key are interchangeable.
func SearchCacheKey(req Request, routingMode string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(strings.TrimSpace(req.Query)))
	b.WriteByte('|')
	b.WriteString(strings.ToLower(strings.TrimSpace(req.Language)))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(req.SafeSearch))
	b.WriteByte('|')
	b.WriteString(strings.Join(sortedLower(req.Categories), ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(sortedLower(req.AllowedDomains), ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(sortedLower(req.BlockedDomains), ","))
	b.WriteByte('|')
	b.WriteString(strings.ToLower(strings.TrimSpace(req.TimeRange)))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(req.MaxResults))
	b.WriteByte('|')
	b.WriteString(routingMode)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func sortedLower(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// lruSearchCache is an in-memory LRU with per-entry TTL.
type lruSearchCache struct {
	mu       sync.Mutex
	maxItems int
	ll       *list.List               // front = most recently used
	items    map[string]*list.Element // key -> element
}

type cacheEntry struct {
	key       string
	value     Response
	expiresAt time.Time
}

func newLRUSearchCache(maxItems int) *lruSearchCache {
	if maxItems <= 0 {
		maxItems = 1000
	}
	return &lruSearchCache{
		maxItems: maxItems,
		ll:       list.New(),
		items:    make(map[string]*list.Element, maxItems),
	}
}

func (c *lruSearchCache) Get(key string) (Response, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Response{}, false
	}
	ent := el.Value.(*cacheEntry)
	if time.Now().After(ent.expiresAt) {
		c.removeElement(el)
		return Response{}, false
	}
	c.ll.MoveToFront(el)
	return ent.value, true
}

func (c *lruSearchCache) Put(key string, value Response, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		ent := el.Value.(*cacheEntry)
		ent.value = value
		ent.expiresAt = time.Now().Add(ttl)
		c.ll.MoveToFront(el)
		return
	}
	ent := &cacheEntry{key: key, value: value, expiresAt: time.Now().Add(ttl)}
	el := c.ll.PushFront(ent)
	c.items[key] = el
	for c.ll.Len() > c.maxItems {
		c.removeElement(c.ll.Back())
	}
}

func (c *lruSearchCache) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.items, el.Value.(*cacheEntry).key)
}

// noopSearchCache disables caching (used when config.cache.enabled is false).
type noopSearchCache struct{}

func (noopSearchCache) Get(string) (Response, bool)         { return Response{}, false }
func (noopSearchCache) Put(string, Response, time.Duration) {}
