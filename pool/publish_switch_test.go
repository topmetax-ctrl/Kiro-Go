package pool

import (
	"path/filepath"
	"sync"
	"testing"

	"kiro-go/config"
)

// TestPublishProfileSwitchAtomicCutover proves the reload+model-list update is
// atomic to concurrent readers: a reader that observes the NEW account snapshot
// must never see it paired with the OLD profile's model set. The test hammers
// GetNextForModel while a switch publishes, and checks the invariant "if the
// account is present with the new model, the routing set contains that model."
func TestPublishProfileSwitchAtomicCutover(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", Weight: 1}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := GetPool()
	p.Reload()
	p.SetModelList("acct-1", []string{"old-model"})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reader: continuously route for the NEW model. Every time it gets the
	// account back, the routing set said the account supports new-model — which is
	// only true after the switch published the new list. It must never observe a
	// torn state where the pool has reloaded but the model list is still the old
	// one (that would make GetNextForModel("new-model") return nil spuriously
	// while a separate GetByID already shows post-switch state).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Route by the new model; when non-nil the routing set must contain it.
			if acc := p.GetNextForModel("new-model"); acc != nil {
				ml := p.GetModelList("acct-1")
				found := false
				for _, m := range ml {
					if m == "new-model" {
						found = true
					}
				}
				if !found {
					t.Errorf("routed to acct-1 for new-model but its model set lacks it: %v", ml)
					return
				}
			}
		}
	}()

	// Writer: publish the switch (reload + new model list) under one lock. Runs on
	// this goroutine, then signals the reader to stop and waits for it. (Closing
	// stop before wg.Wait is essential: the reader loops until stop is closed, so
	// waiting for it first would deadlock.)
	for i := 0; i < 2000; i++ {
		p.PublishProfileSwitch("acct-1", []string{"new-model"})
	}
	close(stop)
	wg.Wait()

	// Final state is the new model list.
	ml := p.GetModelList("acct-1")
	if len(ml) != 1 || ml[0] != "new-model" {
		t.Fatalf("expected final model list [new-model], got %v", ml)
	}
}

// TestDeleteModelListRemovesRoutingSet checks the routing set is dropped and the
// account reverts to cold-start (optimistic-allow) behavior — GetModelList
// returns empty, and the delete is idempotent for unknown IDs.
func TestDeleteModelListRemovesRoutingSet(t *testing.T) {
	p := newTestPool(config.Account{ID: "acct-1", Enabled: true, Weight: 1})
	p.SetModelList("acct-1", []string{"m1", "m2"})
	if got := p.GetModelList("acct-1"); len(got) != 2 {
		t.Fatalf("expected 2 models seeded, got %v", got)
	}

	p.DeleteModelList("acct-1")
	if got := p.GetModelList("acct-1"); len(got) != 0 {
		t.Fatalf("expected empty model list after delete, got %v", got)
	}
	// Idempotent for an unknown ID.
	p.DeleteModelList("does-not-exist")
}
