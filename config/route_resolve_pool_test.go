package config

// The Kiro-pool sentinel as a route target. It names the built-in account pool
// rather than a configured upstream, so ResolveRoute must synthesize a provider
// for it instead of looking one up — otherwise a route naming the pool would
// resolve to nothing and the operator's configured fallback would vanish.

import (
	"testing"

	"kiro-go/metrics"
)

// The sentinel is declared twice on purpose: config owns it as a routing concept
// (KiroPoolTargetID, next to the field that carries it) and metrics owns it as
// the provider id pool traffic is recorded under. metrics documents itself as
// importing neither config nor proxy, so neither package can reference the
// other's copy — which leaves nothing but a test to keep them equal.
//
// If they ever drift, routes stop matching the provider their traffic is filed
// under: the panel's health badge and the router's cooldown decision would be
// reading different providers, and every other test in this file would still
// pass because they all spell the sentinel via metrics.
func TestKiroPoolSentinelMatchesMetrics(t *testing.T) {
	if KiroPoolTargetID != metrics.KiroPoolID {
		t.Errorf("config.KiroPoolTargetID = %q, metrics.KiroPoolID = %q: the routing sentinel and the metrics provider id must be the same string",
			KiroPoolTargetID, metrics.KiroPoolID)
	}
	if KiroPoolTargetName != metrics.KiroPoolName {
		t.Errorf("config.KiroPoolTargetName = %q, metrics.KiroPoolName = %q",
			KiroPoolTargetName, metrics.KiroPoolName)
	}
}

func TestResolveRouteSynthesizesPoolTarget(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Name: "primary", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "claude-opus-5", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Enabled: true},
				{UpstreamID: metrics.KiroPoolID, Priority: 1, Enabled: true},
			},
		}},
	)
	route, targets := ResolveRoute("claude-opus-5")
	if route == nil {
		t.Fatal("expected a match")
	}
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}
	if targets[0].Provider.ID != "u1" {
		t.Fatalf("want the upstream first, got %q", targets[0].Provider.ID)
	}
	pool := targets[1]
	if pool.Provider.ID != metrics.KiroPoolID {
		t.Fatalf("want the pool sentinel second, got %q", pool.Provider.ID)
	}
	if pool.Provider.Name != metrics.KiroPoolName {
		t.Errorf("pool name = %q, want %q", pool.Provider.Name, metrics.KiroPoolName)
	}
	if !pool.Provider.Enabled {
		t.Error("the synthesized pool provider must be enabled; the pool is always available")
	}
	// A BaseURL would be actively dangerous: it is what forwardToTarget would
	// dial. The pool is never relayed to, so the field must stay empty.
	if pool.Provider.BaseURL != "" {
		t.Errorf("pool BaseURL = %q, want empty", pool.Provider.BaseURL)
	}
}

// The sentinel does not need any provider to exist. A brand-new install with
// zero upstreams must still be able to route a model explicitly at the pool.
func TestResolveRoutePoolOnlyRouteWithNoProviders(t *testing.T) {
	makeCfg(t,
		nil,
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{{UpstreamID: metrics.KiroPoolID, Enabled: true}},
		}},
	)
	route, targets := ResolveRoute("m")
	if route == nil || len(targets) != 1 {
		t.Fatalf("want a 1-target match, got (%v, %+v)", route, targets)
	}
	if targets[0].Provider.ID != metrics.KiroPoolID {
		t.Fatalf("want the pool sentinel, got %q", targets[0].Provider.ID)
	}
}

// Priority still decides ordering: the pool is not special-cased to the end, so
// an operator can put it first ("pool by default, upstream never") if they mean
// to. The sentinel's position is the operator's statement, not the resolver's.
func TestResolveRoutePoolHonorsPriority(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Name: "primary", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 3, Enabled: true},
				{UpstreamID: metrics.KiroPoolID, Priority: 0, Enabled: true},
			},
		}},
	)
	_, targets := ResolveRoute("m")
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}
	if targets[0].Provider.ID != metrics.KiroPoolID {
		t.Fatalf("want the pool first at priority 0, got %q", targets[0].Provider.ID)
	}
}

// A disabled sentinel target is filtered like any other, so an operator can turn
// the pool fallback off without deleting the row.
func TestResolveRouteDisabledPoolTargetFiltered(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Name: "primary", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Enabled: true},
				{UpstreamID: metrics.KiroPoolID, Priority: 1, Enabled: false},
			},
		}},
	)
	_, targets := ResolveRoute("m")
	if len(targets) != 1 || targets[0].Provider.ID != "u1" {
		t.Fatalf("want only the upstream target, got %+v", targets)
	}
}

// THE REGRESSION THIS GUARDS
//
// Reaching a pool target ends the upstream walk, so if the sentinel wins the
// weighted pick in a tier it shares with a real upstream, that upstream is never
// contacted — a weight-proportional share of traffic bypasses the paid provider
// entirely. The UI can produce this config (the "same tier as above" toggle), so
// the resolver has to hold the sentinel at the end of its tier.
//
// Resolved repeatedly because the pick is driven by a round-robin counter: a
// single call could pass by luck.
func TestResolveRoutePoolNeverWinsSharedTier(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Name: "primary", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Weight: 1, Enabled: true},
				{UpstreamID: metrics.KiroPoolID, Priority: 0, Weight: 1, Enabled: true},
			},
		}},
	)
	for i := 0; i < 40; i++ {
		_, targets := ResolveRoute("m")
		if len(targets) != 2 {
			t.Fatalf("iteration %d: want 2 targets, got %d", i, len(targets))
		}
		if targets[0].Provider.ID != "u1" {
			t.Fatalf("iteration %d: pool won the weighted pick; u1 would never be tried", i)
		}
		if targets[1].Provider.ID != metrics.KiroPoolID {
			t.Fatalf("iteration %d: want the pool last in its tier, got %q", i, targets[1].Provider.ID)
		}
	}
}

// A high weight on the sentinel must not buy it the pick either: it is excluded
// from the weighted expansion, not merely outnumbered in it.
func TestResolveRoutePoolWeightCannotWinTier(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Name: "primary", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Weight: 1, Enabled: true},
				{UpstreamID: metrics.KiroPoolID, Priority: 0, Weight: 999, Enabled: true},
			},
		}},
	)
	for i := 0; i < 40; i++ {
		_, targets := ResolveRoute("m")
		if targets[0].Provider.ID != "u1" {
			t.Fatalf("iteration %d: a weighted sentinel won the pick", i)
		}
	}
}

// Weighting among real upstreams must still work when a sentinel shares the tier:
// excluding the pool must not collapse the tier to a fixed order.
func TestResolveRouteWeightsStillRotateAlongsidePool(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "a", Enabled: true},
			{ID: "u2", Name: "b", Enabled: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Weight: 1, Enabled: true},
				{UpstreamID: "u2", Priority: 0, Weight: 1, Enabled: true},
				{UpstreamID: metrics.KiroPoolID, Priority: 0, Weight: 1, Enabled: true},
			},
		}},
	)
	seen := map[string]int{}
	for i := 0; i < 40; i++ {
		_, targets := ResolveRoute("m")
		if len(targets) != 3 {
			t.Fatalf("iteration %d: want 3 targets, got %d", i, len(targets))
		}
		if got := targets[2].Provider.ID; got != metrics.KiroPoolID {
			t.Fatalf("iteration %d: want the pool last, got %q", i, got)
		}
		seen[targets[0].Provider.ID]++
	}
	if seen["u1"] == 0 || seen["u2"] == 0 {
		t.Errorf("both upstreams should take turns as the pick, got %v", seen)
	}
	if seen[metrics.KiroPoolID] != 0 {
		t.Errorf("the pool was picked %d times, want 0", seen[metrics.KiroPoolID])
	}
}

// A hand-edited bundle can carry an untrimmed sentinel: import validation trims
// before comparing, so the value survives into stored config. The resolver must
// trim too, or the target is dropped and the operator's pool fallback silently
// disappears after the upstream fails.
func TestResolveRouteTrimsPoolSentinel(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Name: "primary", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Enabled: true},
				{UpstreamID: "  " + metrics.KiroPoolID + " ", Priority: 1, Enabled: true},
			},
		}},
	)
	_, targets := ResolveRoute("m")
	if len(targets) != 2 {
		t.Fatalf("untrimmed sentinel was dropped: got %d targets", len(targets))
	}
	if targets[1].Provider.ID != metrics.KiroPoolID {
		t.Fatalf("want the pool sentinel second, got %q", targets[1].Provider.ID)
	}
	// The stored id must be normalized as well, because the forwarder's sentinel
	// check and the cooldown barrier both compare it raw.
	if got := targets[1].Target.UpstreamID; got != metrics.KiroPoolID {
		t.Errorf("target upstreamId = %q, want the canonical form", got)
	}
}

// A route whose ONLY target is a disabled sentinel has nothing eligible, which
// must look exactly like an unrouted model: (nil, nil) so the caller falls
// through to the pool anyway. The outcome is the same either way, but the route
// must not resolve to an empty target list.
func TestResolveRouteOnlyDisabledPoolFallsThrough(t *testing.T) {
	makeCfg(t,
		nil,
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{{UpstreamID: metrics.KiroPoolID, Enabled: false}},
		}},
	)
	route, targets := ResolveRoute("m")
	if route != nil || targets != nil {
		t.Fatalf("want (nil, nil), got (%v, %+v)", route, targets)
	}
}
