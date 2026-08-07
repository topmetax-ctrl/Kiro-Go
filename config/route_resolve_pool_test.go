package config

// The Kiro-pool sentinel as a route target. It names the built-in account pool
// rather than a configured upstream, so ResolveRoute must synthesize a provider
// for it instead of looking one up — otherwise a route naming the pool would
// resolve to nothing and the operator's configured fallback would vanish.

import (
	"testing"

	"kiro-go/metrics"
)

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
