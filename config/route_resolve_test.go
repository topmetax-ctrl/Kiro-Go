package config

import (
	"strings"
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

// Hidden is a presentation-only flag for the admin list. Resolution must ignore
// it completely: an operator who hides a provider to declutter a long list has
// not asked to stop forwarding to it, and silently dropping its traffic would
// make Hide a second, mislabelled disable switch.
func TestResolveRouteIgnoresHidden(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "hidden-but-live", Enabled: true, Hidden: true},
			{ID: "u2", Name: "visible", Enabled: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Enabled: true},
				{UpstreamID: "u2", Priority: 1, Enabled: true},
			},
		}},
	)
	route, targets := ResolveRoute("m")
	if route == nil {
		t.Fatal("hiding a provider must not unroute the model")
	}
	if len(targets) != 2 {
		t.Fatalf("want both targets eligible, got %d: %+v", len(targets), targets)
	}
	if targets[0].Provider.Name != "hidden-but-live" {
		t.Errorf("hidden provider must keep its priority-0 slot, got %s", targets[0].Provider.Name)
	}
}

// The two flags are independent, so Hidden must not rescue a disabled provider
// either — the failure mode in the opposite direction.
func TestAdvertisedRouteModelsListsEnabledClientNamesOnly(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "9router", Enabled: true},
			{ID: "u2", Name: "off", Enabled: false},
		},
		[]ModelRoute{
			{
				ID: "on", Model: "claude-sonnet-5", Enabled: true,
				Targets: []RouteTarget{{
					UpstreamID: "u1", TargetModel: "runapi/claude-sonnet-5-secret", Enabled: true,
				}},
			},
			{
				ID: "off-route", Model: "claude-opus-5", Enabled: false,
				Targets: []RouteTarget{{UpstreamID: "u1", TargetModel: "hidden-opus", Enabled: true}},
			},
			{
				ID: "dead", Model: "dead-model", Enabled: true,
				Targets: []RouteTarget{{UpstreamID: "u2", Enabled: true}},
			},
			{
				ID: "dup", Model: "claude-sonnet-5", Enabled: true,
				Targets: []RouteTarget{{UpstreamID: "u1", TargetModel: "other-rewrite", Enabled: true}},
			},
			{
				ID: "pool", Model: "gpt-5.6-terra", Enabled: true,
				Targets: []RouteTarget{{UpstreamID: KiroPoolTargetID, Enabled: true}},
			},
		},
	)

	got := AdvertisedRouteModels()
	want := []string{"claude-sonnet-5", "gpt-5.6-terra"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
	joined := strings.Join(got, "\n")
	for _, leak := range []string{"runapi/claude-sonnet-5-secret", "hidden-opus", "other-rewrite", "9router", "dead-model", "claude-opus-5"} {
		if strings.Contains(joined, leak) {
			t.Errorf("advertised list leaked %q: %v", leak, got)
		}
	}
}

func TestGetPublicModelCatalogAutoPicksForwardingWhenRoutesExist(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "claude-sonnet-5", Enabled: true,
			Targets: []RouteTarget{{UpstreamID: "u1", Enabled: true}},
		}},
	)
	if got := GetPublicModelCatalog(); got != PublicModelCatalogForwarding {
		t.Fatalf("auto with routes: got %q", got)
	}
}

func TestGetPublicModelCatalogAutoPicksKiroWhenNoRoutes(t *testing.T) {
	makeCfg(t, nil, nil)
	if got := GetPublicModelCatalog(); got != PublicModelCatalogKiro {
		t.Fatalf("auto without routes: got %q", got)
	}
}

func TestNormalizePublicModelCatalog(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", PublicModelCatalogAuto, true},
		{"auto", PublicModelCatalogAuto, true},
		{"forwarding", PublicModelCatalogForwarding, true},
		{"routes", PublicModelCatalogForwarding, true},
		{"kiro", PublicModelCatalogKiro, true},
		{"both", PublicModelCatalogBoth, true},
		{"nope", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizePublicModelCatalog(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q: got (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestHasEnabledModelRoutes(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "claude-sonnet-5", Enabled: true,
			Targets: []RouteTarget{{UpstreamID: "u1", Enabled: true}},
		}},
	)
	if !HasEnabledModelRoutes() {
		t.Fatal("expected enabled routes")
	}
}

func TestHasEnabledModelRoutesIgnoresDisabled(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Enabled: true}},
		[]ModelRoute{{
			ID: "r1", Model: "claude-sonnet-4.6", Enabled: false,
			Targets: []RouteTarget{{UpstreamID: "u1", Enabled: true}},
		}},
	)
	if HasEnabledModelRoutes() {
		t.Fatal("disabled routes must not lock the public catalog")
	}
}

func TestAdvertisedRouteModelsEmptyWhenNoneUsable(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{{ID: "u1", Enabled: false}},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{{UpstreamID: "u1", TargetModel: "behind", Enabled: true}},
		}},
	)
	if got := AdvertisedRouteModels(); len(got) != 0 {
		t.Fatalf("want empty list, got %v", got)
	}
}

func TestResolveRouteHiddenDoesNotOverrideDisabled(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "off-and-hidden", Enabled: false, Hidden: true},
			{ID: "u2", Name: "live-and-hidden", Enabled: true, Hidden: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Enabled: true},
				{UpstreamID: "u2", Priority: 1, Enabled: true},
			},
		}},
	)
	_, targets := ResolveRoute("m")
	if len(targets) != 1 {
		t.Fatalf("want only the enabled target, got %d: %+v", len(targets), targets)
	}
	if targets[0].Provider.Name != "live-and-hidden" {
		t.Errorf("want live-and-hidden, got %s", targets[0].Provider.Name)
	}
}

// RouteTarget.Hidden is the per-target counterpart of UpstreamProvider.Hidden:
// it declutters the route editor and nothing else. Resolution must ignore it, or
// tidying a long fallback chain would silently reroute live traffic.
func TestResolveRouteIgnoresTargetHidden(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "primary", Enabled: true},
			{ID: "u2", Name: "fallback", Enabled: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Enabled: true, Hidden: true},
				{UpstreamID: "u2", Priority: 1, Enabled: true},
			},
		}},
	)
	route, targets := ResolveRoute("m")
	if route == nil {
		t.Fatal("hiding a target must not unroute the model")
	}
	if len(targets) != 2 {
		t.Fatalf("want both targets eligible, got %d: %+v", len(targets), targets)
	}
	if targets[0].Provider.Name != "primary" {
		t.Errorf("hidden target must keep its priority-0 slot, got %s", targets[0].Provider.Name)
	}
}

// The mirror of TestResolveRouteHiddenDoesNotOverrideDisabled, one level down:
// Hidden and Enabled stay independent on a target too, so unhiding never
// resurrects a target the operator parked by disabling it.
func TestResolveRouteTargetHiddenDoesNotOverrideDisabled(t *testing.T) {
	makeCfg(t,
		[]UpstreamProvider{
			{ID: "u1", Name: "parked", Enabled: true},
			{ID: "u2", Name: "live", Enabled: true},
		},
		[]ModelRoute{{
			ID: "r1", Model: "m", Enabled: true,
			Targets: []RouteTarget{
				{UpstreamID: "u1", Priority: 0, Enabled: false, Hidden: true},
				{UpstreamID: "u2", Priority: 1, Enabled: true, Hidden: true},
			},
		}},
	)
	_, targets := ResolveRoute("m")
	if len(targets) != 1 {
		t.Fatalf("want only the enabled target, got %d: %+v", len(targets), targets)
	}
	if targets[0].Provider.Name != "live" {
		t.Errorf("want live, got %s", targets[0].Provider.Name)
	}
}
