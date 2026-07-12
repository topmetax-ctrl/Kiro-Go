package proxy

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"
)

func fp(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func profileWithFingerprint(model string, f [32]byte, cumTokens, total int, ttl time.Duration) *promptCacheProfile {
	return &promptCacheProfile{
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: f, CumulativeTokens: cumTokens, TTL: ttl}},
		TotalInputTokens: total,
		Model:            model,
	}
}

func TestLRUMissThenCreationThenHit(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 100)
	f := fp("prefix-A")
	prof := profileWithFingerprint("claude-sonnet-4.5", f, 4000, 5000, time.Hour)

	// First Compute for the account: miss (no prior entries).
	u := tr.Compute("acct", prof)
	if u.CacheReadInputTokens != 0 {
		t.Fatalf("first request should have 0 cache read, got %d", u.CacheReadInputTokens)
	}
	// Store it.
	tr.Update("acct", prof)
	// Now a Compute with the same fingerprint hits.
	u = tr.Compute("acct", prof)
	if u.CacheReadInputTokens == 0 {
		t.Fatalf("expected a cache hit after Update, got read=0")
	}

	m, entries, _ := tr.Metrics()
	if m.Hits < 1 {
		t.Errorf("expected >=1 hit, got %d", m.Hits)
	}
	if m.Creations < 1 {
		t.Errorf("expected >=1 creation, got %d", m.Creations)
	}
	if entries != 1 {
		t.Errorf("expected 1 entry, got %d", entries)
	}
}

func TestLRUUpdateExistingDoesNotDuplicate(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 100)
	prof := profileWithFingerprint("claude-sonnet-4.5", fp("x"), 4000, 5000, time.Hour)
	tr.Update("acct", prof)
	tr.Update("acct", prof)
	_, entries, _ := tr.Metrics()
	if entries != 1 {
		t.Fatalf("re-updating same key must not duplicate, got %d entries", entries)
	}
}

func TestLRUCapacityEvictsLeastRecentlyUsed(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 2)
	pa := profileWithFingerprint("claude-sonnet-4.5", fp("A"), 4000, 5000, time.Hour)
	pb := profileWithFingerprint("claude-sonnet-4.5", fp("B"), 4000, 5000, time.Hour)
	pc := profileWithFingerprint("claude-sonnet-4.5", fp("C"), 4000, 5000, time.Hour)

	tr.Update("acct", pa)
	tr.Update("acct", pb)
	// Touch A so B becomes LRU.
	tr.Compute("acct", pa)
	// Insert C → evicts B (the least-recently-used).
	tr.Update("acct", pc)

	// A and C present, B evicted.
	if r := tr.Compute("acct", pa); r.CacheReadInputTokens == 0 {
		t.Errorf("A should still be present")
	}
	if r := tr.Compute("acct", pc); r.CacheReadInputTokens == 0 {
		t.Errorf("C should be present")
	}
	// B: reuse a fresh tracker check via a profile that only has B's fp.
	// After eviction, B must be a miss. But the account still has entries, so
	// Compute returns read=0 for B specifically.
	if r := tr.Compute("acct", pb); r.CacheReadInputTokens != 0 {
		t.Errorf("B should have been evicted (LRU), got read=%d", r.CacheReadInputTokens)
	}
	m, entries, capacity := tr.Metrics()
	if capacity != 2 {
		t.Errorf("capacity should be 2, got %d", capacity)
	}
	if entries > 2 {
		t.Errorf("entries must stay within capacity, got %d", entries)
	}
	if m.Evictions < 1 {
		t.Errorf("expected >=1 eviction, got %d", m.Evictions)
	}
}

func TestLRUTTLExpiry(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 100)
	prof := profileWithFingerprint("claude-sonnet-4.5", fp("x"), 4000, 5000, 20*time.Millisecond)
	tr.Update("acct", prof)
	time.Sleep(40 * time.Millisecond)
	// Expired: Compute for that fingerprint must not hit; the entry is pruned on access.
	r := tr.Compute("acct", prof)
	if r.CacheReadInputTokens != 0 {
		t.Fatalf("expired entry must not hit, got read=%d", r.CacheReadInputTokens)
	}
	m, _, _ := tr.Metrics()
	if m.ExpiredEvict < 1 {
		t.Errorf("expected an expired-eviction to be counted, got %d", m.ExpiredEvict)
	}
}

func TestLRUPerAccountIsolation(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 100)
	shared := fp("same-prefix")
	prof := profileWithFingerprint("claude-sonnet-4.5", shared, 4000, 5000, time.Hour)

	// Account A stores the prefix.
	tr.Update("acctA", prof)
	// Account B has the SAME fingerprint but its own (empty) namespace: first
	// request for B is a miss, never a hit on A's entry.
	r := tr.Compute("acctB", prof)
	if r.CacheReadInputTokens != 0 {
		t.Fatalf("account B must not hit account A's entry, got read=%d", r.CacheReadInputTokens)
	}
}

func TestLRUConcurrentComputeUpdate(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 1000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				acct := fmt.Sprintf("acct-%d", (g+i)%4)
				prof := profileWithFingerprint("claude-sonnet-4.5", fp(fmt.Sprintf("%d-%d", g, i%20)), 4000, 5000, time.Hour)
				tr.Compute(acct, prof)
				tr.Update(acct, prof)
			}
		}(g)
	}
	wg.Wait()
	// Bounded: never exceeds capacity.
	_, entries, capacity := tr.Metrics()
	if entries > capacity {
		t.Fatalf("entries %d exceeded capacity %d", entries, capacity)
	}
}

func TestLRUMemoryBoundedUnderManyAccounts(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 500)
	for a := 0; a < 100; a++ {
		for e := 0; e < 20; e++ {
			prof := profileWithFingerprint("claude-sonnet-4.5", fp(fmt.Sprintf("%d-%d", a, e)), 4000, 5000, time.Hour)
			tr.Update(fmt.Sprintf("acct-%d", a), prof)
		}
	}
	_, entries, capacity := tr.Metrics()
	if entries > capacity {
		t.Fatalf("global cap must bound total entries regardless of account count: %d > %d", entries, capacity)
	}
}
