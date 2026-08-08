package config

// Route resolution: turning a client model name into an ordered list of upstream
// candidates to try.
//
// The ordering contract is what the forwarder relies on for failover:
//
//	tier by tier (Priority ascending), and within a tier the weighted pick comes
//	first, followed by that tier's remaining targets.
//
// So element 0 is "the provider this request should normally use" and the rest is
// "what to try if it fails" — the forwarder walks the slice and never has to know
// about priorities or weights itself.

import (
	"sort"
	"strings"
	"sync/atomic"
)

// maxTargetWeight clamps Weight when expanding a tier for weighted selection.
// Weights are operator-supplied and only meaningful as ratios, so a cap costs
// nothing in expressiveness while bounding the expansion slice.
const maxTargetWeight = 1000

// routeRRCounter drives weighted selection within a tier.
//
// A counter rather than math/rand: selection runs on every forwarded request, and
// a deterministic sequence is both reproducible in tests and free of the
// long-running-server seeding pitfalls noted in proxy/websearch.go. Distribution
// over many requests matches the configured weights either way; only the
// per-request choice is predictable, which no security property depends on.
//
// One global counter (not per-route) is intentional: per-route counters would
// need their own map and lock for no observable gain, since the weights of
// unrelated routes are independent anyway.
var routeRRCounter uint64

// ResolvedTarget pairs a selected RouteTarget with its resolved provider, so the
// caller does not have to look the provider up again.
type ResolvedTarget struct {
	Target   RouteTarget
	Provider UpstreamProvider
}

// ResolveRoute finds the enabled ModelRoute matching the given client model name
// (exact match after trimming, same as the pre-multi-target behavior) and returns
// it with its eligible targets in try-order.
//
// A target is eligible when it is enabled AND its provider exists and is enabled.
// Returns (nil, nil) when no route matches or the matching route has no eligible
// target — callers then fall through to the default Kiro pool. Note the two are
// deliberately indistinguishable to the caller: a route whose every provider is
// disabled must behave exactly like an unrouted model, which is what keeps the
// loop-safety invariant on ModelRoute intact.
func ResolveRoute(model string) (*ModelRoute, []ResolvedTarget) {
	cfgLock.RLock()
	if cfg == nil {
		cfgLock.RUnlock()
		return nil, nil
	}
	target := strings.TrimSpace(model)
	if target == "" {
		cfgLock.RUnlock()
		return nil, nil
	}

	var matched *ModelRoute
	var eligible []ResolvedTarget

	for i := range cfg.ModelRoutes {
		r := cfg.ModelRoutes[i]
		if !r.Enabled || strings.TrimSpace(r.Model) != target {
			continue
		}
		for _, t := range r.Targets {
			if !t.Enabled || strings.TrimSpace(t.UpstreamID) == "" {
				continue
			}
			// The Kiro Pool sentinel is a special target that does not match any
			// configured upstream. When it appears, create a synthetic ResolvedTarget
			// so the forwarder recognizes it and falls through to the pool instead of
			// surfacing an "upstream not found" error. This lets the pool participate
			// in multi-target failover: 9aws P0 -> xpiki P0 -> Kiro Pool P1.
			//
			// Matched on the TRIMMED id, like the emptiness check above. A hand-edited
			// bundle carrying " __kiro_pool__" passes import validation (which trims),
			// so comparing the raw value here would drop the target and silently
			// delete the operator's pool fallback.
			if IsKiroPoolTarget(t.UpstreamID) {
				// Normalize the stored id so every later comparison — the forwarder's
				// sentinel check, the cooldown barrier — sees the canonical form.
				t.UpstreamID = KiroPoolTargetID
				eligible = append(eligible, ResolvedTarget{
					Target: t,
					Provider: UpstreamProvider{
						ID:      KiroPoolTargetID,
						Name:    KiroPoolTargetName,
						Enabled: true,
					},
				})
				continue
			}
			for j := range cfg.Upstreams {
				up := cfg.Upstreams[j]
				if up.ID == t.UpstreamID && up.Enabled {
					eligible = append(eligible, ResolvedTarget{Target: t, Provider: up})
					break
				}
			}
		}
		if len(eligible) > 0 {
			routeCopy := r
			matched = &routeCopy
			break
		}
		// This route matched by name but has nothing usable. Keep scanning:
		// FindEnabledRoute behaved the same way, so a duplicate route entry with
		// a live provider is still reachable.
	}
	cfgLock.RUnlock()

	if matched == nil || len(eligible) == 0 {
		return nil, nil
	}
	return matched, orderTargets(eligible)
}

// orderTargets sorts eligible targets into try-order: Priority ascending, and
// within each Priority tier the weighted pick first followed by the rest.
func orderTargets(in []ResolvedTarget) []ResolvedTarget {
	// Stable so that equal (Priority, Weight) targets keep their configured
	// order — operator intent, and it makes tests deterministic.
	sort.SliceStable(in, func(a, b int) bool {
		return in[a].Target.Priority < in[b].Target.Priority
	})

	// Advance once per resolution, not once per tier, so the counter does not
	// couple the choices made in different tiers of the same request.
	n := atomic.AddUint64(&routeRRCounter, 1) - 1

	out := make([]ResolvedTarget, 0, len(in))
	for start := 0; start < len(in); {
		end := start + 1
		for end < len(in) && in[end].Target.Priority == in[start].Target.Priority {
			end++
		}
		out = append(out, weightedOrder(in[start:end], n)...)
		start = end
	}
	return out
}

// weightedOrder returns one Priority tier with the weighted pick first, then the
// tier's remaining targets (so failover still covers the whole tier).
//
// THE POOL SENTINEL NEVER WINS THE WEIGHTED PICK
//
// Reaching a pool target ends the upstream walk: proxy/upstream_forward.go returns
// false there and the request falls through to the account pool. So if the
// sentinel were hoisted to the front of a tier it shares with a real upstream,
// that upstream would not be tried at all — a route configured "u1 and pool at
// equal priority" would send a weight-proportional share of traffic straight past
// the paid upstream. The sentinel is therefore held at the END of its tier: it can
// be the tier's fallback, never its pick.
func weightedOrder(tier []ResolvedTarget, n uint64) []ResolvedTarget {
	if len(tier) <= 1 {
		return tier
	}

	// Split the sentinel out before any weighting so it cannot be selected. Order
	// among several (a pathological config) is preserved.
	relayable := make([]ResolvedTarget, 0, len(tier))
	pools := make([]ResolvedTarget, 0, 1)
	for _, t := range tier {
		if t.Provider.ID == KiroPoolTargetID {
			pools = append(pools, t)
			continue
		}
		relayable = append(relayable, t)
	}
	if len(pools) > 0 {
		// With nothing else in the tier there is no choice to make; otherwise weight
		// the relayable targets among themselves and append the pool as last resort.
		return append(weightedOrder(relayable, n), pools...)
	}

	// Expand each target Weight times; the pick is an index into the expansion,
	// which is what makes selection proportional to Weight.
	expanded := make([]int, 0, len(tier))
	for i := range tier {
		w := tier[i].Target.Weight
		if w <= 0 {
			w = 1 // Unset weight means "an equal share", not "never pick me".
		}
		if w > maxTargetWeight {
			w = maxTargetWeight
		}
		for k := 0; k < w; k++ {
			expanded = append(expanded, i)
		}
	}

	first := expanded[int(n%uint64(len(expanded)))]

	out := make([]ResolvedTarget, 0, len(tier))
	out = append(out, tier[first])
	for i := range tier {
		if i != first {
			out = append(out, tier[i])
		}
	}
	return out
}

// FindEnabledRoute returns the route matching the given client model name and the
// single provider it would be forwarded to first.
//
// Deprecated: this is the pre-multi-target entry point and discards every backup
// target, so a caller using it gets no failover. Use ResolveRoute. Retained
// because it is the documented single-destination lookup and remains correct for
// callers that only need to answer "would this model be forwarded, and where".
func FindEnabledRoute(model string) (*ModelRoute, *UpstreamProvider) {
	route, targets := ResolveRoute(model)
	if route == nil || len(targets) == 0 {
		return nil, nil
	}
	// Surface the selected target's rewrite on the returned route, so callers
	// reading route.TargetModel (the legacy field) still see the right value
	// rather than whatever the pre-migration config happened to hold.
	route.TargetModel = targets[0].Target.TargetModel
	route.UpstreamID = targets[0].Provider.ID
	up := targets[0].Provider
	return route, &up
}
