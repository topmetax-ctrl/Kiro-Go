package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
)

// The getters return independent snapshots, so a concurrent UpdateToken /
// UpdateStats / Reload can never be observed as a torn read on a returned value.
// Run these under `go test -race`.

func raceTestPool(n int) *AccountPool {
	accts := make([]config.Account, 0, n)
	for i := 0; i < n; i++ {
		accts = append(accts, config.Account{
			ID:           fmt.Sprintf("acct-%d", i),
			AccessToken:  "tok-gen0",
			RefreshToken: "ref-gen0",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
			Weight:       1,
			Enabled:      true,
		})
	}
	return newTestPool(accts...)
}

func TestGetterSnapshotIsolatedFromMutation(t *testing.T) {
	p := raceTestPool(4)
	snap := p.GetByID("acct-0")
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	// Mutating the pool must not change an already-returned snapshot.
	p.UpdateToken("acct-0", "tok-gen1", "ref-gen1", time.Now().Add(2*time.Hour).Unix())
	if snap.AccessToken != "tok-gen0" || snap.RefreshToken != "ref-gen0" {
		t.Fatalf("snapshot mutated by pool write: %+v", snap)
	}
	// And the pool must reflect the new value on a fresh read.
	fresh := p.GetByID("acct-0")
	if fresh.AccessToken != "tok-gen1" {
		t.Fatalf("expected fresh snapshot to see update, got %q", fresh.AccessToken)
	}
	// Snapshots are distinct allocations.
	if snap == fresh {
		t.Fatal("expected distinct snapshot pointers")
	}
}

func TestConcurrentGetVsUpdateToken(t *testing.T) {
	// UpdateStats/UpdateToken persist through config; initialize a temp config so
	// those writes have a backing store (this test targets the pool race, not
	// persistence).
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := raceTestPool(8)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: rotate tokens with a matching (token, refresh, expiry) generation.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gen := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				gen++
				id := fmt.Sprintf("acct-%d", gen%8)
				tok := fmt.Sprintf("tok-gen%d", gen)
				ref := fmt.Sprintf("ref-gen%d", gen)
				p.UpdateToken(id, tok, ref, time.Now().Add(time.Hour).Unix())
			}
		}()
	}

	// Also exercise UpdateStats and Reload concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.UpdateStats("acct-1", 10, 0.5)
		}
	}()

	// Readers: snapshots must be internally whole (never a nil-mid-copy panic).
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if a := p.GetNextExcluding(nil); a != nil {
					_ = a.AccessToken
					_ = a.RefreshToken
					_ = a.ExpiresAt
				}
				if a := p.GetByID("acct-2"); a != nil {
					_ = a.AccessToken
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestConcurrentGetVsReload(t *testing.T) {
	// Reload replaces the whole backing slice; a snapshot returned before Reload
	// must remain valid and unchanged.
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := raceTestPool(6)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.Reload()
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				_ = p.GetNextForModelExcluding("claude-sonnet-4.5", nil)
				_ = p.GetByID("acct-3")
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
