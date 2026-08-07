package proxy

// The Kiro-pool sentinel as a forward target.
//
// A pool target is not an upstream to relay to — it is the instruction "stop
// walking upstreams and let the caller's normal Kiro dispatch handle this". Two
// consequences are what these tests pin down:
//
//   - tryForwardUpstream must return false at the sentinel WITHOUT writing to
//     the client, so handler.go falls through to the account pool exactly as it
//     does for an unrouted model.
//   - the sentinel is therefore a hard end to the chain, so cooldown reordering
//     must not move anything across it in either direction.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/metrics"
)

// setupPoolRoute wires one route from a target spec. An entry that equals
// metrics.KiroPoolID becomes the pool sentinel (no provider is created for it);
// anything else is treated as an upstream base URL and gets its own provider at
// the next ascending priority, so slice order is try order.
func setupPoolRoute(t *testing.T, clientModel string, specs ...string) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var ups []config.UpstreamProvider
	var targets []config.RouteTarget
	for i, spec := range specs {
		if spec == metrics.KiroPoolID {
			targets = append(targets, config.RouteTarget{
				UpstreamID: metrics.KiroPoolID, Priority: i, Weight: 1, Enabled: true,
			})
			continue
		}
		id := "up-" + string(rune('a'+i))
		ups = append(ups, config.UpstreamProvider{
			ID: id, Name: "provider-" + string(rune('a'+i)), BaseURL: spec, ApiKey: "s", Enabled: true,
		})
		targets = append(targets, config.RouteTarget{
			UpstreamID: id, Priority: i, Weight: 1, Enabled: true,
		})
	}
	route := config.ModelRoute{ID: "route-1", Model: clientModel, Targets: targets, Enabled: true}
	if err := config.UpdateUpstreamConfig(ups, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
}

// forwardExpectingFallThrough asserts tryForwardUpstream declined the request,
// which is how it signals "use the Kiro pool". The recorder is returned so the
// caller can also verify nothing was written.
func forwardExpectingFallThrough(t *testing.T, clientModel string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"model":"` + clientModel + `","messages":[],"stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	if h.tryForwardUpstream(r, rec, []byte(body), clientModel, false, "/messages", true, "") {
		t.Fatalf("expected fall-through to the Kiro pool, but the forwarder claimed the request (body=%q)", rec.Body.String())
	}
	return rec
}

// A route whose only target is the pool must behave exactly like an unrouted
// model: decline, write nothing, leave the response for the pool to produce.
func TestForwardPoolOnlyRouteFallsThrough(t *testing.T) {
	metrics.Reset()
	setupPoolRoute(t, "pool-only", metrics.KiroPoolID)

	rec := forwardExpectingFallThrough(t, "pool-only")
	if rec.Body.Len() != 0 {
		t.Errorf("nothing may be written before fall-through, got %q", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status must be untouched, got %d", rec.Code)
	}
}

// The headline case: an upstream primary with the pool as its backup. The
// upstream is tried first, and only its failure hands the request to the pool.
func TestForwardTriesUpstreamBeforeFallingBackToPool(t *testing.T) {
	metrics.Reset()

	var hits int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer primary.Close()

	setupPoolRoute(t, "up-then-pool", primary.URL, metrics.KiroPoolID)

	rec := forwardExpectingFallThrough(t, "up-then-pool")
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("primary upstream hits = %d, want 1 (it must be tried before the pool)", got)
	}
	// The 503 body must NOT reach the client: the pool is going to answer instead,
	// so surfacing the upstream's error would corrupt a request that still succeeds.
	if rec.Body.Len() != 0 {
		t.Errorf("upstream failure leaked to the client: %q", rec.Body.String())
	}
}

// A successful primary means the pool is never reached — "pool as backup" must
// not cost anything on the happy path.
func TestForwardPoolBackupUnusedWhenPrimarySucceeds(t *testing.T) {
	metrics.Reset()

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-upstream"}`))
	}))
	defer primary.Close()

	setupPoolRoute(t, "pool-unused", primary.URL, metrics.KiroPoolID)

	rec := httptest.NewRecorder()
	body := `{"model":"pool-unused","messages":[],"stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(body), "pool-unused", false, "/messages", true, "") {
		t.Fatal("a healthy primary must serve the request itself")
	}
	if !strings.Contains(rec.Body.String(), "from-upstream") {
		t.Errorf("want the upstream body, got %q", rec.Body.String())
	}
}

// Reaching the sentinel ends the walk, so an upstream configured BELOW it is
// unreachable. Asserted so the behavior is a documented consequence rather than
// an accident — the UI marks such rows for the same reason.
func TestForwardStopsAtPoolSentinel(t *testing.T) {
	metrics.Reset()

	var afterHits int64
	after := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&afterHits, 1)
		_, _ = w.Write([]byte(`{"id":"should-never-run"}`))
	}))
	defer after.Close()

	setupPoolRoute(t, "pool-first", metrics.KiroPoolID, after.URL)

	forwardExpectingFallThrough(t, "pool-first")
	if got := atomic.LoadInt64(&afterHits); got != 0 {
		t.Errorf("target below the pool sentinel was contacted %d time(s); it must be unreachable", got)
	}
}

// Cooldown must not promote the pool past a tripped upstream. Doing so would
// skip that upstream permanently — the chain ends at the sentinel — silently
// converting "pool is the backup" into "pool is the only backend".
func TestCooldownDoesNotPromotePoolPastColdUpstream(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("up-a", forwardCooldownThreshold, now.UnixMilli())

	targets := targetsFor("up-a", metrics.KiroPoolID)
	demoted := applyForwardCooldown(targets, now)

	if demoted != 0 {
		t.Errorf("demoted = %d, want 0: there is nothing ahead of the sentinel to reorder", demoted)
	}
	if got := providerOrder(targets); !sameOrder(got, "up-a", metrics.KiroPoolID) {
		t.Errorf("order = %v, want the cold upstream still ahead of the pool", got)
	}
}

// The pool's own failure streak is real — kiro_pool_metrics.go records pool
// traffic under this ID — so without the barrier it would be classified like any
// provider and shuffled on the strength of pool health.
func TestCooldownIgnoresPoolStreak(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures(metrics.KiroPoolID, forwardCooldownThreshold*3, now.UnixMilli())

	targets := targetsFor(metrics.KiroPoolID, "up-a")
	demoted := applyForwardCooldown(targets, now)

	if demoted != 0 {
		t.Errorf("demoted = %d, want 0", demoted)
	}
	if got := providerOrder(targets); !sameOrder(got, metrics.KiroPoolID, "up-a") {
		t.Errorf("order = %v: a tripped pool must not be demoted, its position is structural", got)
	}
}

// Within the upstream prefix cooldown still works normally; only crossing the
// sentinel is forbidden. This is the case that proves the barrier did not simply
// disable the feature for any route mentioning the pool.
func TestCooldownReordersPrefixButKeepsPoolLast(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("up-a", forwardCooldownThreshold, now.UnixMilli())

	targets := targetsFor("up-a", "up-b", metrics.KiroPoolID)
	demoted := applyForwardCooldown(targets, now)

	if demoted != 1 {
		t.Fatalf("demoted = %d, want 1 (the cold up-a)", demoted)
	}
	if got := providerOrder(targets); !sameOrder(got, "up-b", "up-a", metrics.KiroPoolID) {
		t.Errorf("order = %v, want [up-b up-a %s]", got, metrics.KiroPoolID)
	}
}

// A sentinel in the middle bounds the reorder to the prefix ahead of it: the
// unreachable tail must keep its configured position, because moving the pool is
// how the operator revives that tail.
func TestCooldownLeavesTailBelowPoolUntouched(t *testing.T) {
	metrics.Reset()
	now := time.Now()
	recordFailures("up-a", forwardCooldownThreshold, now.UnixMilli())

	targets := targetsFor("up-a", "up-b", metrics.KiroPoolID, "up-d")
	applyForwardCooldown(targets, now)

	if got := providerOrder(targets); !sameOrder(got, "up-b", "up-a", metrics.KiroPoolID, "up-d") {
		t.Errorf("order = %v, want the tail after the sentinel left in place", got)
	}
}
