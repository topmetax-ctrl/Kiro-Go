package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

func BenchmarkRoute(b *testing.B) {
	primary := &scriptedProvider{name: providerSearXNG, resp: SearchResponse{Results: goodResults(5)}, state: ProviderHealthy}
	router := newProviderRouter([]providerEntry{{provider: primary}}, nil, false)
	ctx := context.Background()
	req := SearchRequest{Query: "golang concurrency", MaxResults: 5}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = router.Route(ctx, req)
	}
}

// benchProfile builds a profile with `breakpoints` cache breakpoints, each above
// the default min-cacheable threshold, seeded so fingerprints are distinct per
// (salt, index).
func benchProfile(salt string, breakpoints int) *promptCacheProfile {
	bps := make([]promptCacheBreakpoint, 0, breakpoints)
	for i := 0; i < breakpoints; i++ {
		fp := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", salt, i)))
		bps = append(bps, promptCacheBreakpoint{
			Fingerprint:      fp,
			CumulativeTokens: 2048 * (i + 1),
			TTL:              5 * time.Minute,
		})
	}
	return &promptCacheProfile{
		Breakpoints:      bps,
		TotalInputTokens: 2048 * (breakpoints + 1),
		Model:            "claude-sonnet-4.5",
	}
}

// seedTracker fills the tracker with `accounts` accounts each holding
// `entriesPer` live entries, to expose the cost of the global prune scan.
func seedTracker(accounts, entriesPer int) *promptCacheTracker {
	t := newPromptCacheTracker(time.Hour)
	for a := 0; a < accounts; a++ {
		acct := fmt.Sprintf("acct-%d", a)
		t.Update(acct, benchProfile(acct, entriesPer))
	}
	return t
}

func BenchmarkPromptCacheUpdate(b *testing.B) {
	for _, sz := range []struct {
		accounts, entries int
	}{{1, 4}, {8, 16}, {64, 16}} {
		t := seedTracker(sz.accounts, sz.entries)
		prof := benchProfile("hot", 4)
		b.Run(fmt.Sprintf("accounts=%d/entries=%d", sz.accounts, sz.entries), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				t.Update("acct-0", prof)
			}
		})
	}
}

func BenchmarkPromptCacheCompute(b *testing.B) {
	for _, sz := range []struct {
		accounts, entries int
	}{{1, 4}, {8, 16}, {64, 16}} {
		t := seedTracker(sz.accounts, sz.entries)
		prof := benchProfile("acct-0", sz.entries) // matches seeded entries → hit path
		b.Run(fmt.Sprintf("accounts=%d/entries=%d", sz.accounts, sz.entries), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = t.Compute("acct-0", prof)
			}
		})
	}
}

func BenchmarkPromptCacheComputeMiss(b *testing.B) {
	t := seedTracker(64, 16)
	prof := benchProfile("nomatch", 4)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = t.Compute("acct-0", prof)
	}
}

func BenchmarkPromptCacheParallel(b *testing.B) {
	t := seedTracker(64, 16)
	prof := benchProfile("acct-0", 16)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = t.Compute("acct-0", prof)
			t.Update("acct-0", prof)
		}
	})
}
