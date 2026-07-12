package proxy

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/pool"
)

// tmTestPool builds a pool with n accounts whose tokens are already stale (so
// EnsureFresh will trigger a refresh).
func tmTestPool(t *testing.T, n int) *pool.AccountPool {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	accts := make([]config.Account, 0, n)
	for i := 0; i < n; i++ {
		accts = append(accts, config.Account{
			ID:           fmt.Sprintf("acct-%d", i),
			AccessToken:  "old",
			RefreshToken: "old-ref",
			ExpiresAt:    time.Now().Add(-time.Hour).Unix(), // already expired → stale
			Enabled:      true,
			Weight:       1,
		})
	}
	return pool.NewTestPool(accts...)
}

func TestEnsureFreshCoalescesConcurrentRefreshes(t *testing.T) {
	p := tmTestPool(t, 1)
	var idpCalls int64
	refresh := func(a *config.Account) (string, string, int64, string, error) {
		atomic.AddInt64(&idpCalls, 1)
		time.Sleep(20 * time.Millisecond) // widen the coalescing window
		return "new-tok", "new-ref", time.Now().Add(time.Hour).Unix(), "", nil
	}
	persist := func(id, at, rt string, exp int64) error { return nil }
	tm := NewTokenManager(p, refresh, persist)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acc, err := tm.EnsureFresh("acct-0")
			if err != nil {
				t.Errorf("EnsureFresh: %v", err)
				return
			}
			if acc.AccessToken != "new-tok" {
				t.Errorf("expected refreshed token, got %q", acc.AccessToken)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&idpCalls); got != 1 {
		t.Fatalf("expected exactly 1 IdP call for 100 concurrent refreshes, got %d", got)
	}
}

func TestEnsureFreshDifferentAccountsRunInParallel(t *testing.T) {
	const n = 8
	p := tmTestPool(t, n)
	var concurrent int64
	var maxConcurrent int64
	refresh := func(a *config.Account) (string, string, int64, string, error) {
		c := atomic.AddInt64(&concurrent, 1)
		for {
			m := atomic.LoadInt64(&maxConcurrent)
			if c <= m || atomic.CompareAndSwapInt64(&maxConcurrent, m, c) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&concurrent, -1)
		return "new", "new-ref", time.Now().Add(time.Hour).Unix(), "", nil
	}
	tm := NewTokenManager(p, refresh, func(id, at, rt string, exp int64) error { return nil })

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := tm.EnsureFresh(fmt.Sprintf("acct-%d", i)); err != nil {
				t.Errorf("EnsureFresh acct-%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if atomic.LoadInt64(&maxConcurrent) < 2 {
		t.Fatalf("expected parallel refresh across accounts, max concurrency was %d", maxConcurrent)
	}
}

func TestEnsureFreshRejectsEmptyToken(t *testing.T) {
	p := tmTestPool(t, 1)
	refresh := func(a *config.Account) (string, string, int64, string, error) {
		return "", "", time.Now().Add(time.Hour).Unix(), "", nil // empty access token
	}
	tm := NewTokenManager(p, refresh, func(id, at, rt string, exp int64) error { return nil })
	if _, err := tm.EnsureFresh("acct-0"); err == nil {
		t.Fatal("expected error for empty access token, got nil")
	}
	if tm.State("acct-0") != TokenReauthRequired {
		t.Fatalf("expected ReauthRequired, got %s", tm.State("acct-0"))
	}
}

func TestEnsureFreshPersistFailureKeepsTokenAndDegrades(t *testing.T) {
	p := tmTestPool(t, 1)
	refresh := func(a *config.Account) (string, string, int64, string, error) {
		return "new-tok", "new-ref", time.Now().Add(time.Hour).Unix(), "", nil
	}
	var persistCalls int64
	persist := func(id, at, rt string, exp int64) error {
		atomic.AddInt64(&persistCalls, 1)
		return fmt.Errorf("disk full")
	}
	tm := NewTokenManager(p, refresh, persist)

	acc, err := tm.EnsureFresh("acct-0")
	if err != nil {
		t.Fatalf("EnsureFresh should not error on persist failure: %v", err)
	}
	// The rotated token must NOT be dropped: it is published to the pool.
	if acc.AccessToken != "new-tok" {
		t.Fatalf("expected rotated token published despite persist failure, got %q", acc.AccessToken)
	}
	if got := p.GetByID("acct-0").AccessToken; got != "new-tok" {
		t.Fatalf("pool should hold rotated token, got %q", got)
	}
	if tm.State("acct-0") != TokenPersistenceDegraded {
		t.Fatalf("expected PersistenceDegraded, got %s", tm.State("acct-0"))
	}
}

func TestRetryDegradedPersistRecovers(t *testing.T) {
	p := tmTestPool(t, 1)
	refresh := func(a *config.Account) (string, string, int64, string, error) {
		return "new-tok", "new-ref", time.Now().Add(time.Hour).Unix(), "", nil
	}
	var fail atomic.Bool
	fail.Store(true)
	persist := func(id, at, rt string, exp int64) error {
		if fail.Load() {
			return fmt.Errorf("transient")
		}
		return nil
	}
	tm := NewTokenManager(p, refresh, persist)

	if _, err := tm.EnsureFresh("acct-0"); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if tm.State("acct-0") != TokenPersistenceDegraded {
		t.Fatalf("expected degraded, got %s", tm.State("acct-0"))
	}
	// Recover the persistence backend, then flush.
	fail.Store(false)
	tm.FlushPending()
	if tm.State("acct-0") != TokenHealthy {
		t.Fatalf("expected Healthy after successful flush, got %s", tm.State("acct-0"))
	}
}

func TestEnsureFreshSkipsWhenAlreadyFresh(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := pool.NewTestPool(config.Account{
		ID:          "acct-0",
		AccessToken: "good",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(), // fresh
		Enabled:     true,
	})
	var idpCalls int64
	refresh := func(a *config.Account) (string, string, int64, string, error) {
		atomic.AddInt64(&idpCalls, 1)
		return "x", "y", 0, "", nil
	}
	tm := NewTokenManager(p, refresh, func(id, at, rt string, exp int64) error { return nil })
	acc, err := tm.EnsureFresh("acct-0")
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if acc.AccessToken != "good" {
		t.Fatalf("expected unchanged token, got %q", acc.AccessToken)
	}
	if atomic.LoadInt64(&idpCalls) != 0 {
		t.Fatalf("expected no IdP call for a fresh token, got %d", idpCalls)
	}
}
