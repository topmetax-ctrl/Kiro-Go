package proxy

// Route attribution for pool-served requests.
//
// The dashboard's per-route table is fed by metrics.Event.RouteID, and until the
// pool became a route target only forwarded requests ever carried one. These
// tests pin the two halves of the fix: a route that can reach the pool gets
// credited for the traffic the pool serves on its behalf, and a route that
// cannot reach the pool never does — otherwise a disabled route would be shown
// carrying requests it never touched.

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"kiro-go/metrics"
)

func routeStatFor(t *testing.T, routeID string) (metrics.RouteStat, bool) {
	t.Helper()
	for _, rs := range metrics.RouteStats() {
		if rs.RouteID == routeID {
			return rs, true
		}
	}
	return metrics.RouteStat{}, false
}

func TestPoolRouteIDForRequiresPoolTarget(t *testing.T) {
	setupPoolRoute(t, "claude-pool-only", metrics.KiroPoolID)
	if got := poolRouteIDFor("claude-pool-only"); got != "route-1" {
		t.Fatalf("pool-only route -> %q, want route-1", got)
	}
	// Whitespace is trimmed by ResolveRoute, so the raw client string still
	// resolves.
	if got := poolRouteIDFor("  claude-pool-only  "); got != "route-1" {
		t.Fatalf("padded model -> %q, want route-1", got)
	}
	// A model with no route at all: the overwhelmingly common case for pool
	// traffic, and it must stay free of attribution.
	if got := poolRouteIDFor("claude-unrouted"); got != "" {
		t.Fatalf("unrouted model -> %q, want empty", got)
	}
	if got := poolRouteIDFor(""); got != "" {
		t.Fatalf("empty model -> %q, want empty", got)
	}
}

func TestPoolRouteIDForSkipsUpstreamOnlyRoute(t *testing.T) {
	// This route never falls through to the pool: tryForwardUpstream either
	// commits a response or reports the failure itself. So pool traffic under
	// this model means the route did not match, and crediting it would invent
	// traffic.
	setupPoolRoute(t, "claude-up-only", "http://127.0.0.1:1")
	if got := poolRouteIDFor("claude-up-only"); got != "" {
		t.Fatalf("upstream-only route -> %q, want empty", got)
	}
}

func TestPoolRouteIDForFindsSentinelAfterUpstream(t *testing.T) {
	// The realistic shape: try a real upstream first, fall back to the pool.
	setupPoolRoute(t, "claude-mixed", "http://127.0.0.1:1", metrics.KiroPoolID)
	if got := poolRouteIDFor("claude-mixed"); got != "route-1" {
		t.Fatalf("mixed route -> %q, want route-1", got)
	}
}

func TestWithPoolRouteContextTagsOnlyPoolReachableRoutes(t *testing.T) {
	setupPoolRoute(t, "claude-pool-only", metrics.KiroPoolID)

	r := httptest.NewRequest("POST", "/v1/messages", nil)
	tagged := withPoolRouteContext(r, "claude-pool-only")
	if got := poolRouteIDFromContext(tagged.Context()); got != "route-1" {
		t.Fatalf("tagged request -> %q, want route-1", got)
	}

	// No route: the request must come back untouched rather than carrying an
	// empty key, so the ordinary path allocates nothing.
	plain := httptest.NewRequest("POST", "/v1/messages", nil)
	if got := withPoolRouteContext(plain, "claude-unrouted"); got != plain {
		t.Fatal("unrouted model must return the same *http.Request")
	}
	if got := poolRouteIDFromContext(plain.Context()); got != "" {
		t.Fatalf("untagged request -> %q, want empty", got)
	}
	if got := poolRouteIDFromContext(context.Background()); got != "" {
		t.Fatalf("bare context -> %q, want empty", got)
	}
	//nolint:staticcheck // a nil ctx is reachable from paths with no request scope
	if got := poolRouteIDFromContext(nil); got != "" {
		t.Fatalf("nil context -> %q, want empty", got)
	}
}

func TestPoolSuccessIsCreditedToRoute(t *testing.T) {
	setupPoolRoute(t, "claude-pool-only", metrics.KiroPoolID)
	metrics.Reset()
	h := &Handler{}

	r := withPoolRouteContext(httptest.NewRequest("POST", "/v1/messages", nil), "claude-pool-only")
	h.recordSuccessLogSplit(r.Context(), "claude", "claude-pool-only", "acct-1", 1200, 340, 2.5, 1500)

	rs, ok := routeStatFor(t, "route-1")
	if !ok {
		t.Fatalf("pool-served route missing from RouteStats: %+v", metrics.RouteStats())
	}
	if rs.Requests != 1 || rs.Success != 1 {
		t.Fatalf("requests/success = %d/%d, want 1/1", rs.Requests, rs.Success)
	}
	if rs.InputTokens != 1200 || rs.OutputTokens != 340 {
		t.Fatalf("tokens = %d/%d, want 1200/340", rs.InputTokens, rs.OutputTokens)
	}
	// The provider shown against the route must be the pool, not a stale
	// upstream id, so the table says where the traffic actually went.
	if rs.ProviderID != metrics.KiroPoolID {
		t.Fatalf("providerId = %q, want %q", rs.ProviderID, metrics.KiroPoolID)
	}
	if rs.ClientModel != "claude-pool-only" {
		t.Fatalf("clientModel = %q, want claude-pool-only", rs.ClientModel)
	}
}

func TestPoolFailureIsCreditedToRoute(t *testing.T) {
	setupPoolRoute(t, "claude-pool-only", metrics.KiroPoolID)
	metrics.Reset()
	h := &Handler{}

	r := withPoolRouteContext(httptest.NewRequest("POST", "/v1/messages", nil), "claude-pool-only")
	h.recordFailureWithDetails(r.Context(), "claude", "claude-pool-only", "acct-1", errors.New("quota exceeded"))

	rs, ok := routeStatFor(t, "route-1")
	if !ok {
		t.Fatalf("failed pool request missing from RouteStats: %+v", metrics.RouteStats())
	}
	if rs.Requests != 1 || rs.Failed != 1 {
		t.Fatalf("requests/failed = %d/%d, want 1/1", rs.Requests, rs.Failed)
	}
}

func TestUntaggedPoolTrafficLeavesRouteStatsEmpty(t *testing.T) {
	setupPoolRoute(t, "claude-pool-only", metrics.KiroPoolID)
	metrics.Reset()
	h := &Handler{}

	// Same model, but no route context attached — this is what an internal
	// caller with no request scope looks like. metrics.Record gates on a
	// non-empty RouteID, so nothing should be aggregated per-route.
	h.recordSuccessLogSplit(context.Background(), "claude", "claude-pool-only", "acct-1", 10, 5, 0, 100)

	if stats := metrics.RouteStats(); len(stats) != 0 {
		t.Fatalf("untagged pool traffic produced route stats: %+v", stats)
	}
	// It must still be counted as pool traffic, though.
	if d, ok := metrics.ProviderDetailFor(metrics.KiroPoolID, 60); !ok || d.Requests != 1 {
		t.Fatalf("pool provider detail = %+v (ok=%v), want 1 request", d, ok)
	}
}
