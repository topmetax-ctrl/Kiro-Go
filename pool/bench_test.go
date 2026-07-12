package pool

import (
	"fmt"
	"testing"

	"kiro-go/config"
)

func benchPool(n int) *AccountPool {
	accts := make([]config.Account, 0, n)
	for i := 0; i < n; i++ {
		accts = append(accts, config.Account{
			ID:          fmt.Sprintf("acct-%d", i),
			AccessToken: "tok",
			ExpiresAt:   0, // 0 => not treated as expiring
			Weight:      1,
		})
	}
	p := newTestPool(accts...)
	for i := 0; i < n; i++ {
		p.modelLists[fmt.Sprintf("acct-%d", i)] = map[string]bool{"claude-sonnet-4.5": true}
	}
	return p
}

func BenchmarkGetNextExcluding(b *testing.B) {
	for _, n := range []int{1, 8, 64} {
		p := benchPool(n)
		b.Run(fmt.Sprintf("accounts=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = p.GetNextExcluding(nil)
			}
		})
	}
}

func BenchmarkGetNextForModelExcluding(b *testing.B) {
	for _, n := range []int{1, 8, 64} {
		p := benchPool(n)
		b.Run(fmt.Sprintf("accounts=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = p.GetNextForModelExcluding("claude-sonnet-4.5", nil)
			}
		})
	}
}

func BenchmarkGetByID(b *testing.B) {
	p := benchPool(64)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = p.GetByID("acct-32")
	}
}

func BenchmarkGetNextExcludingParallel(b *testing.B) {
	p := benchPool(64)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = p.GetNextExcluding(nil)
		}
	})
}
