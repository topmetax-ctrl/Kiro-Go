package proxy

// Cooldown policy for forward targets.
//
// These tests drive the policy through metrics.Record — the same path the real
// request flow uses — rather than by poking a private counter, because the whole
// design point of forward_cooldown.go is that metrics owns the streak state.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/metrics"
)

// recordFailures pushes n failed events for a provider so its streak crosses
// (or approaches) the cooldown threshold.
func recordFailures(providerID string, n int, atMs int64) {
	for i := 0; i < n; i++ {
		metrics.Record(metrics.Event{
			TimeMs:     atMs,
			ProviderID: providerID,
			Status:     503,
			Ok:         false,
			ErrorMsg:   "simulated",
		})
	}
}

func targetsFor(ids ...string) []config.ResolvedTarget {
	out := make([]config.ResolvedTarget, 0, len(ids))
	for i, id := range ids {
		out = append(out, config.ResolvedTarget{
			Target:   config.RouteTarget{UpstreamID: id, Priority: i, Weight: 1, Enabled: true},
			Provider: config.UpstreamProvider{ID: id, Name: id, Enabled: true},
		})
	}
	return out
}

func providerOrder(targets []config.ResolvedTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Provider.ID)
	}
	return out
}

func sameOrder(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A provider with no recorded traffic must never be treated as cold: a freshly
// configured target has to get its first chance.
func TestCooldownIgnoresUnknownProvider(t *testing.T) {
	metrics.Reset()
	if forwardTargetInCooldown("never-seen", time.Now()) {
		t.Fatal("unknown provider reported in cooldown")
	}
}

// Below the threshold nothing is demoted — an isolated blip must not cost a
// provider its position.
func TestCooldownRequiresThreshold(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("cd-a", forwardCooldownThreshold-1, now.UnixMilli())

	if forwardTargetInCooldown("cd-a", now) {
		t.Fatalf("provider tripped at %d failures, threshold is %d",
			forwardCooldownThreshold-1, forwardCooldownThreshold)
	}
}

func TestCooldownTripsAtThreshold(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("cd-b", forwardCooldownThreshold, now.UnixMilli())

	if !forwardTargetInCooldown("cd-b", now) {
		t.Fatalf("provider not tripped after %d consecutive failures", forwardCooldownThreshold)
	}
}

// The window is measured from the LAST failure, which is what gives half-open
// behavior for free: once it elapses the target is eligible again.
func TestCooldownHalfOpensAfterWindow(t *testing.T) {
	metrics.Reset()
	failedAt := time.Now().Add(-forwardCooldownWindow - time.Second)
	recordFailures("cd-c", forwardCooldownThreshold, failedAt.UnixMilli())

	if forwardTargetInCooldown("cd-c", time.Now()) {
		t.Fatal("provider still cold after the window elapsed")
	}
}

// A success zeroes the streak in metrics, which must immediately close the
// circuit — no separate reset bookkeeping.
func TestCooldownClearedBySuccess(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("cd-d", forwardCooldownThreshold, now.UnixMilli())
	if !forwardTargetInCooldown("cd-d", now) {
		t.Fatal("expected provider to be cold before the success")
	}

	metrics.Record(metrics.Event{
		TimeMs: now.UnixMilli(), ProviderID: "cd-d", Status: 200, Ok: true,
	})

	if forwardTargetInCooldown("cd-d", now) {
		t.Fatal("a success did not close the circuit")
	}
}

// Client cancellations are not a reliability signal, so they must never trip a
// provider. This mirrors the guarantee metrics.Record already makes.
func TestCooldownIgnoresCancellations(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	for i := 0; i < forwardCooldownThreshold+2; i++ {
		metrics.Record(metrics.Event{
			TimeMs: now.UnixMilli(), ProviderID: "cd-e",
			Status: 499, Ok: false, Canceled: true,
		})
	}

	if forwardTargetInCooldown("cd-e", now) {
		t.Fatal("client cancellations tripped the circuit")
	}
}

// The core reordering: a cold primary loses its slot to a healthy fallback.
func TestApplyCooldownDemotesColdPrimary(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("cd-p", forwardCooldownThreshold, now.UnixMilli())

	targets := targetsFor("cd-p", "cd-q")
	demoted := applyForwardCooldown(targets, now)

	if demoted != 1 {
		t.Fatalf("demoted = %d, want 1", demoted)
	}
	if got := providerOrder(targets); !sameOrder(got, "cd-q", "cd-p") {
		t.Fatalf("order = %v, want [cd-q cd-p]", got)
	}
}

// Demotion must never DROP a target: the cold provider stays in the list as a
// last resort. Dropping it could leave a route with nothing to try, which would
// fall through to the Kiro pool and answer from a different backend entirely.
func TestApplyCooldownNeverDropsTargets(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("cd-r", forwardCooldownThreshold, now.UnixMilli())

	targets := targetsFor("cd-r", "cd-s", "cd-t")
	before := len(targets)
	applyForwardCooldown(targets, now)

	if len(targets) != before {
		t.Fatalf("target count changed %d -> %d", before, len(targets))
	}
	seen := map[string]bool{}
	for _, id := range providerOrder(targets) {
		seen[id] = true
	}
	for _, want := range []string{"cd-r", "cd-s", "cd-t"} {
		if !seen[want] {
			t.Fatalf("target %s was dropped: %v", want, providerOrder(targets))
		}
	}
}

// When every target is cold there is no better order to pick, so the operator's
// configured priority is left alone.
func TestApplyCooldownKeepsOrderWhenAllCold(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("cd-u", forwardCooldownThreshold, now.UnixMilli())
	recordFailures("cd-v", forwardCooldownThreshold, now.UnixMilli())

	targets := targetsFor("cd-u", "cd-v")
	if demoted := applyForwardCooldown(targets, now); demoted != 0 {
		t.Fatalf("demoted = %d, want 0 when everything is cold", demoted)
	}
	if got := providerOrder(targets); !sameOrder(got, "cd-u", "cd-v") {
		t.Fatalf("order = %v, want configured order preserved", got)
	}
}

// A lone target is always tried, however bad its streak.
func TestApplyCooldownSingleTargetUntouched(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("cd-w", forwardCooldownThreshold*3, now.UnixMilli())

	targets := targetsFor("cd-w")
	if demoted := applyForwardCooldown(targets, now); demoted != 0 {
		t.Fatalf("demoted = %d, want 0 for a single target", demoted)
	}
	if got := providerOrder(targets); !sameOrder(got, "cd-w") {
		t.Fatalf("order = %v", got)
	}
}

// Relative order within each group survives, so priority still decides among
// targets of equal health.
func TestApplyCooldownPreservesRelativeOrder(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("cold-1", forwardCooldownThreshold, now.UnixMilli())
	recordFailures("cold-2", forwardCooldownThreshold, now.UnixMilli())

	// Configured order interleaves cold and healthy.
	targets := targetsFor("cold-1", "warm-1", "cold-2", "warm-2")
	if demoted := applyForwardCooldown(targets, now); demoted != 2 {
		t.Fatalf("demoted = %d, want 2", demoted)
	}
	if got := providerOrder(targets); !sameOrder(got, "warm-1", "warm-2", "cold-1", "cold-2") {
		t.Fatalf("order = %v, want [warm-1 warm-2 cold-1 cold-2]", got)
	}
}

// End-to-end: once the primary has failed enough to trip, subsequent requests
// must go straight to the backup instead of paying its round-trip again.
//
// This is the whole point of the cooldown layer, so it is asserted on the real
// forwarding path rather than on applyForwardCooldown in isolation.
func TestForwardSkipsCooledDownPrimaryEndToEnd(t *testing.T) {
	metrics.Reset()

	var primaryHits, backupHits int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&primaryHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backupHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-backup"}`))
	}))
	defer backup.Close()

	setupMultiTargetRoute(t, "cool-e2e", primary.URL, backup.URL)

	// Drive the primary past the threshold. Each request fails over to the backup,
	// so every one of them succeeds from the client's point of view.
	for i := 0; i < forwardCooldownThreshold; i++ {
		rec := forwardOnce(t, "cool-e2e", false)
		if !strings.Contains(rec.Body.String(), "from-backup") {
			t.Fatalf("request %d: expected backup to serve, got %q", i, rec.Body.String())
		}
	}

	hitsBefore := atomic.LoadInt64(&primaryHits)
	if hitsBefore != int64(forwardCooldownThreshold) {
		t.Fatalf("primary hits before cooldown = %d, want %d", hitsBefore, forwardCooldownThreshold)
	}

	// The primary is now tripped: this request must not touch it at all.
	rec := forwardOnce(t, "cool-e2e", false)
	if !strings.Contains(rec.Body.String(), "from-backup") {
		t.Fatalf("expected backup to serve, got %q", rec.Body.String())
	}
	if got := atomic.LoadInt64(&primaryHits); got != hitsBefore {
		t.Fatalf("primary was contacted while in cooldown: hits %d -> %d", hitsBefore, got)
	}
	if got := atomic.LoadInt64(&backupHits); got != int64(forwardCooldownThreshold)+1 {
		t.Fatalf("backup hits = %d, want %d", got, forwardCooldownThreshold+1)
	}
}

// A route whose ONLY target is in cooldown must still be attempted. Dropping it
// would turn a routed model into an unrouted one, which falls through to the Kiro
// pool and answers from a completely different backend — a silent, confusing
// behavior change that the demote-never-drop rule exists to prevent.
func TestForwardStillTriesSoleColdTarget(t *testing.T) {
	metrics.Reset()

	var hits int64
	only := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		if n <= int64(forwardCooldownThreshold) {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"recovered"}`))
	}))
	defer only.Close()

	setupMultiTargetRoute(t, "cool-solo", only.URL)

	for i := 0; i < forwardCooldownThreshold; i++ {
		forwardOnce(t, "cool-solo", false)
	}
	// Tripped, but it is the only candidate: it must still be contacted, and the
	// recovery must reach the client.
	rec := forwardOnce(t, "cool-solo", false)
	if !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("sole cold target was not retried; body = %q", rec.Body.String())
	}
}

// End-to-end: once the preferred provider has tripped, subsequent requests must
// stop paying it a round-trip. This is the whole point of the cooldown — the
// failover tests already prove correctness without it, so what is measured here
// is the COST, i.e. that the dead provider stops being contacted.
func TestCooldownStopsContactingDeadPrimary(t *testing.T) {
	metrics.Reset()

	var primaryHits, backupHits int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&primaryHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backupHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer backup.Close()

	setupMultiTargetRoute(t, "cool-e2e", primary.URL, backup.URL)

	// Drive enough failures to trip the primary, then keep going.
	const total = 8
	for i := 0; i < total; i++ {
		rec := forwardOnce(t, "cool-e2e", false)
		if got := rec.Body.String(); got != `{"id":"ok"}` {
			t.Fatalf("request %d: client body = %q, want the backup's response", i, got)
		}
	}

	ph := atomic.LoadInt64(&primaryHits)
	bh := atomic.LoadInt64(&backupHits)

	// Every request must have been served, all by the backup.
	if bh != total {
		t.Errorf("backup served %d of %d requests", bh, total)
	}
	// The primary should have been contacted only until it tripped. Allow the
	// threshold plus a small margin (a half-open probe is legitimate), but it must
	// be far below one hit per request — that would mean cooldown did nothing.
	if ph > forwardCooldownThreshold+1 {
		t.Errorf("primary contacted %d times over %d requests; cooldown is not suppressing it (threshold %d)",
			ph, total, forwardCooldownThreshold)
	}
	if ph == 0 {
		t.Error("primary never contacted; it should be tried until it trips")
	}
	t.Logf("primary contacted %d/%d requests, backup served %d", ph, total, bh)
}
