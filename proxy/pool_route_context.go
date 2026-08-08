package proxy

// Route attribution for pool-served requests.
//
// A forwarded request is recorded with RouteID set (see forwardToTarget), which
// is what feeds the dashboard's per-route table. Pool-served requests used to
// need no such thing: before the pool could appear as a route target, "a route
// matched" and "an upstream answered" were the same statement, so every route
// with traffic had a provider event carrying its id.
//
// Kiro-Pool-as-target breaks that equivalence. A route whose only reachable
// target is the pool sentinel is served entirely by the account pool, and the
// pool's own metrics carry no route id — so the operator configures a pool
// fallback, sends traffic through it, and sees a route sitting at zero
// requests. The route looks dead when it is in fact working.
//
// The route id cannot be recovered at record time from the model string: by
// then req.Model has been rewritten by resolveClaudeThinkingModeAndEffort (the
// thinking suffix is stripped), so it no longer matches the route key it was
// resolved with. It has to be captured before that rewrite, which is what
// withPoolRouteContext does, and carried on the request context — the one
// channel the pool dispatch already threads all the way down.

import (
	"context"
	"net/http"

	"kiro-go/config"
)

type poolRouteContextKey struct{}

// poolRouteIDFor returns the id of the route that model matched, but only when
// that route can actually reach the Kiro pool.
//
// The sentinel check is the whole point. A route pointing exclusively at real
// upstreams never reaches the pool — tryForwardUpstream either commits a
// response or reports the failure itself — so pool traffic under such a model
// means the route did not match at all (disabled, or no eligible targets), and
// attributing it to that route would invent traffic the route never carried.
// Requiring a sentinel keeps the attribution to configs where falling through
// is the operator's stated intent.
func poolRouteIDFor(model string) string {
	route, targets := config.ResolveRoute(model)
	if route == nil {
		return ""
	}
	for _, rt := range targets {
		if config.IsKiroPoolTarget(rt.Provider.ID) {
			return route.ID
		}
	}
	return ""
}

// withPoolRouteContext tags r with the route being served by the pool, so the
// metrics funnel can attribute the outcome. It must be called with the RAW
// client model, before any thinking-suffix rewrite, and only after
// tryForwardUpstream has declined the request.
//
// Returns r unchanged when the model names no pool-reachable route, which is
// the ordinary case: most pool traffic has no route at all.
func withPoolRouteContext(r *http.Request, model string) *http.Request {
	routeID := poolRouteIDFor(model)
	if routeID == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), poolRouteContextKey{}, routeID))
}

// poolRouteIDFromContext returns the route id attached by withPoolRouteContext,
// or empty when the request was not served on behalf of a route. Empty is the
// signal metrics.Record uses to skip per-route aggregation entirely, so an
// untagged request costs nothing.
func poolRouteIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(poolRouteContextKey{}).(string); ok {
		return v
	}
	return ""
}
