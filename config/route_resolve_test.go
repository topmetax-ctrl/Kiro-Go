package config

import (
	"testing"
)

// makeCfg installs a test config directly. Tests here exercise pure resolution
// logic, so no file I/O is involved.
func makeCfg(t *testing.T, ups []UpstreamProvider, routes []ModelRoute) {
	t.Helper()
	cfgLock.Lock()
	cfg = &Config{Upstreams: ups, ModelRoutes: routes}
	cfgLock.Unlock()
	t.Cleanup(func() {
		cfgLock.Lock()
		cfg = nil
		cfgLock.Unlock()
	})
}

func TestMigrateModelRoutesPromotesLegacySchema(t *testing.T) {
	routes := []ModelRoute{{
		ID: "r1", Model: "claude-opus-5", UpstreamID: "u1",
		TargetModel: "opus", Enabled: true,
	}}
	if !migrateModelRoutes(routes) {
		t.Fatal("expected migration to report a change")
	}
	if len(routes[0].Targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(routes[0].Targets))
	}
	got := routes[0].Targets[0]
	if got.UpstreamID != "u1" || got.TargetModel != "opus" || got.Priority != 0 || got.Weight != 1 || !got.Enabled {
		t.Fatalf("unexpected migrated target: %+v", got)
	}
	// Legacy fields must survive for downgrade/bundle compatibility.
	if routes[0].UpstreamID != "u1" {
		t.Error("legacy UpstreamID should be preserved, not cleared")
	}
	// Idempotent.
	if migrateModelRoutes(routes) {
		t.Error("second migration should report no change")
	}
}

func TestMigrateModelRoutesSkipsEmptyAndAlreadyMigrated(t *testing.T) {
	routes := []ModelRoute{
		{ID: "r1", Model: "a", Enabled: true}, // no legacy upstream, nothing to do
		{ID: "r2", Model: "b", UpstreamID: "u1", Targets: []RouteTarget{{UpstreamID: "u9", Enabled: true}}},
	}
	if migrateModelRoutes(routes) {
		t.Error("expected no change")
	}
	if len(routes[0].Targets) != 0 {
		t.Error("route without legacy upstream should get no targets")
	}
	if routes[1].Targets[0].UpstreamID != "u9" {
		t.Error("existing Targets must not be overwritten by the legacy field")
	}
}

func TestResolveRouteOrdersByPriority(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "primary", Enabled: true},
			{ID: "u2", Name: "backup", Enabled: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "claude-opus-5", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u2", Priority: 5, Enabled: true},
				{UpstreamID: "u1", Priority: 0, Enabled: true},
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
	if targets[0].Provider.Name != "primary" || targets[1].Provider.Name != "backup" {
		t.Fatalf("wrong order: %s, %s", targets[0].Provider.Name, targets[1].Provider.Name)
	}
}

func TestResolveRouteFiltersDisabled(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "off-provider", Enabled: false},
			{ID: "u2", Name: "live", Enabled: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Enabled: true},  // provider disabled
				{UpstreamID: "u2", Priority: 1, Enabled: false}, // target disabled
				{UpstreamID: "u2", Priority: 2, Enabled: true},  // the only eligible one
			},
		}},
	)
	_, targets := ResolveRoute("m")
	if len(targets) != 1 || targets[0].Target.Priority != 2 {
		t.Fatalf("want only the priority-2 target, got %+v", targets)
	}
}

// A route whose every target is unusable must be indistinguishable from an
// unrouted model, so the caller falls through to the Kiro pool.
func TestResolveRouteNoEligibleTargetsFallsThrough(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Enabled: false}},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{{UpstreamID: "u1", Enabled: true}},
		}},
	)
	route, targets := ResolveRoute("m")
	if route != nil || targets != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", route, targets)
	}
}

func TestResolveRouteExactMatchOnly(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "claude-opus-5", Enabled: true,
			Targets: []RouteTarget{{UpstreamID: "u1", Enabled: true}},
		}},
	)
	// Trimming is allowed; case folding and prefixes are not. This is the
	// loop-safety invariant, so it is worth pinning.
	if r, _ := ResolveRoute("  claude-opus-5  "); r == nil {
		t.Error("surrounding whitespace should be trimmed")
	}
	for _, bad := range []string{"Claude-Opus-5", "claude-opus-5-thinking", "claude-opus", ""} {
		if r, _ := ResolveRoute(bad); r != nil {
			t.Errorf("%q should not match", bad)
		}
	}
}

// Within a tier every target must appear (so failover covers the whole tier),
// and the leading pick must follow the configured weights over many draws.
func TestResolveRouteWeightedWithinTier(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "heavy", Enabled: true},
			{ID: "u2", Name: "light", Enabled: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Weight: 3, Enabled: true},
				{UpstreamID: "u2", Priority: 0, Weight: 1, Enabled: true},
			},
		}},
	)
	counts := map[string]int{}
	const draws = 400
	for i := 0; i < draws; i++ {
		_, targets := ResolveRoute("m")
		if len(targets) != 2 {
			t.Fatalf("tier must stay fully covered for failover, got %d", len(targets))
		}
		counts[targets[0].Provider.Name]++
	}
	// 3:1 over an exact round-robin expansion.
	if counts["heavy"] != draws*3/4 || counts["light"] != draws/4 {
		t.Errorf("want 3:1 split, got heavy=%d light=%d", counts["heavy"], counts["light"])
	}
}

func TestFindEnabledRouteWrapperReturnsTopTarget(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "primary", Enabled: true},
			{ID: "u2", Name: "backup", Enabled: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u2", Priority: 9, TargetModel: "b", Enabled: true},
				{UpstreamID: "u1", Priority: 0, TargetModel: "a", Enabled: true},
			},
		}},
	)
	route, up := FindEnabledRoute("m")
	if route == nil || up == nil {
		t.Fatal("expected a match")
	}
	if up.Name != "primary" {
		t.Errorf("want primary provider, got %s", up.Name)
	}
	// The legacy fields must reflect the *selected* target, not stale config.
	if route.TargetModel != "a" || route.UpstreamID != "u1" {
		t.Errorf("legacy fields not synced to selection: %+v", route)
	}
}
