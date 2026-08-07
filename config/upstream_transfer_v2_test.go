package config

// Schema-2 (multi-target) behavior of the forwarding bundle: cross-host ID
// remapping of targets, v1<->v2 interoperability, and validation of the newer
// route shape.

import (
	"strings"
	"testing"
)

// multiRoute builds a schema-2 route with ranked targets and no legacy fields,
// which is what the current UI produces.
func multiRoute(id, model string, upstreamIDs ...string) ModelRoute {
	r := ModelRoute{ID: id, Model: model, Enabled: true}
	for i, up := range upstreamIDs {
		r.Targets = append(r.Targets, RouteTarget{
			UpstreamID: up, Priority: i, Weight: 1, Enabled: true,
		})
	}
	return r
}

// Every target must resolve to a provider that exists in the merged config.
// Note the imported id is legitimately REUSED when it does not collide locally
// (freshID keeps it), so the invariant is resolvability, not renaming.
func TestMergeRemapsAllTargetIDs(t *testing.T) {
	b := bundle(
		[]UpstreamProvider{
			prov("remote-1", "alpha", "https://a.example/v1", "k1"),
			prov("remote-2", "beta", "https://b.example/v1", "k2"),
		},
		[]ModelRoute{multiRoute("r1", "coding", "remote-1", "remote-2")},
	)

	// Pre-existing providers whose ids collide with the imported ones, forcing
	// the merge to mint fresh ids and re-point the targets.
	existing := []UpstreamProvider{
		prov("remote-1", "gamma", "https://g.example/v1", "kg"),
		prov("remote-2", "delta", "https://d.example/v1", "kd"),
	}

	n := 0
	res := MergeUpstreamBundle(existing, nil, b, func() string {
		n++
		return "minted-" + string(rune('0'+n))
	})

	if res.RoutesAdded != 1 || len(res.Routes) != 1 {
		t.Fatalf("routes added = %d, want 1", res.RoutesAdded)
	}
	got := res.Routes[0]
	if len(got.Targets) != 2 {
		t.Fatalf("want 2 targets preserved, got %d", len(got.Targets))
	}

	// Map each local id to its provider so we can check the targets point at the
	// IMPORTED providers (alpha/beta), not the unrelated colliding ones.
	byID := map[string]UpstreamProvider{}
	for _, p := range res.Providers {
		byID[p.ID] = p
	}
	for i, want := range []string{"alpha", "beta"} {
		p, ok := byID[got.Targets[i].UpstreamID]
		if !ok {
			t.Errorf("target %d upstreamId %q resolves to no provider", i, got.Targets[i].UpstreamID)
			continue
		}
		if p.Name != want {
			t.Errorf("target %d resolved to %q, want the imported %q", i, p.Name, want)
		}
	}
	// Priority order is operator intent and must survive the round trip.
	if got.Targets[0].Priority != 0 || got.Targets[1].Priority != 1 {
		t.Errorf("priorities not preserved: %+v", got.Targets)
	}
}

// A target whose provider was skipped as a duplicate must re-point at the
// EXISTING local provider rather than being dropped.
func TestMergeTargetFollowsDeduplicatedProvider(t *testing.T) {
	existing := []UpstreamProvider{prov("local-1", "alpha", "https://a.example/v1", "local-key")}
	b := bundle(
		[]UpstreamProvider{
			prov("remote-1", "alpha", "https://a.example/v1", "remote-key"), // duplicate of local-1
			prov("remote-2", "beta", "https://b.example/v1", "k2"),
		},
		[]ModelRoute{multiRoute("r1", "coding", "remote-1", "remote-2")},
	)

	res := MergeUpstreamBundle(existing, nil, b, nil)
	if res.ProvidersSkipped != 1 {
		t.Fatalf("providers skipped = %d, want 1 (the duplicate)", res.ProvidersSkipped)
	}
	got := res.Routes[0]
	if len(got.Targets) != 2 {
		t.Fatalf("want both targets kept, got %d", len(got.Targets))
	}
	if got.Targets[0].UpstreamID != "local-1" {
		t.Errorf("target 0 = %q, want it re-pointed at the existing provider local-1", got.Targets[0].UpstreamID)
	}
}

// A v1 bundle (legacy fields only) must come out with Targets populated, since
// ResolveRoute reads Targets exclusively — otherwise the import looks successful
// but the route never matches a request.
func TestMergeV1BundleGetsTargets(t *testing.T) {
	b := UpstreamBundle{
		Version: Version, Kind: UpstreamBundleKind, Schema: 1,
		Providers: []UpstreamProvider{prov("remote-1", "alpha", "https://a.example/v1", "k")},
		Routes:    []ModelRoute{{ID: "r1", Model: "coding", UpstreamID: "remote-1", TargetModel: "gpt-4o", Enabled: true}},
	}
	if err := ValidateUpstreamBundle(&b); err != nil {
		t.Fatalf("a v1 bundle must still validate: %v", err)
	}

	res := MergeUpstreamBundle(nil, nil, b, nil)
	if res.RoutesAdded != 1 {
		t.Fatalf("routes added = %d, want 1", res.RoutesAdded)
	}
	got := res.Routes[0]
	if len(got.Targets) != 1 {
		t.Fatalf("v1 route must be migrated to 1 target, got %d", len(got.Targets))
	}
	if got.Targets[0].TargetModel != "gpt-4o" || !got.Targets[0].Enabled {
		t.Errorf("migrated target lost data: %+v", got.Targets[0])
	}
	if got.Targets[0].UpstreamID != res.Providers[0].ID {
		t.Errorf("migrated target points at %q, want local %q", got.Targets[0].UpstreamID, res.Providers[0].ID)
	}
}

// Export must remain readable by a v1 build: those importers require upstreamId
// on every route and reject the whole file otherwise.
func TestExportBackfillsLegacyFieldsForOldBuilds(t *testing.T) {
	t.Cleanup(func() {
		cfgLock.Lock()
		cfg = nil
		cfgLock.Unlock()
	})
	cfgLock.Lock()
	cfg = &Config{
		Upstreams: []UpstreamProvider{
			prov("u1", "alpha", "https://a.example/v1", "k1"),
			prov("u2", "beta", "https://b.example/v1", "k2"),
		},
		ModelRoutes: []ModelRoute{multiRoute("r1", "coding", "u1", "u2")},
	}
	cfgLock.Unlock()

	b := ExportUpstreamBundle()
	if b.Schema != UpstreamBundleSchema {
		t.Errorf("schema = %d, want %d", b.Schema, UpstreamBundleSchema)
	}
	if len(b.Routes) != 1 {
		t.Fatalf("want 1 route, got %d", len(b.Routes))
	}
	// The legacy field must name the PREFERRED target, so an old build degrades to
	// the right provider rather than an arbitrary one.
	if b.Routes[0].UpstreamID != "u1" {
		t.Errorf("legacy upstreamId = %q, want the top-priority target u1", b.Routes[0].UpstreamID)
	}
	if len(b.Routes[0].Targets) != 2 {
		t.Errorf("targets must still be present for v2 readers, got %d", len(b.Routes[0].Targets))
	}
	// And the backfill must not corrupt live config.
	_, routes := GetUpstreamConfig()
	if routes[0].UpstreamID != "" {
		t.Errorf("export mutated stored config: upstreamId = %q", routes[0].UpstreamID)
	}
}

// A schema-2 route carries no upstreamId; validation must accept it and still
// check each target resolves inside the bundle.
func TestValidateSchema2Routes(t *testing.T) {
	okProv := prov("p1", "alpha", "https://a.example/v1", "k")

	valid := bundle([]UpstreamProvider{okProv}, []ModelRoute{multiRoute("r1", "coding", "p1")})
	if err := ValidateUpstreamBundle(&valid); err != nil {
		t.Errorf("targets-only route should validate: %v", err)
	}

	dangling := bundle([]UpstreamProvider{okProv}, []ModelRoute{multiRoute("r1", "coding", "p1", "ghost")})
	err := ValidateUpstreamBundle(&dangling)
	if err == nil || !strings.Contains(err.Error(), "does not match any provider") {
		t.Errorf("dangling target should be rejected, got %v", err)
	}

	blank := bundle([]UpstreamProvider{okProv}, []ModelRoute{{
		ID: "r1", Model: "coding", Enabled: true,
		Targets: []RouteTarget{{UpstreamID: "  ", Enabled: true}},
	}})
	if err := ValidateUpstreamBundle(&blank); err == nil || !strings.Contains(err.Error(), "upstreamId is required") {
		t.Errorf("blank target upstreamId should be rejected, got %v", err)
	}
}

// Full round trip: export from one host, import into another with unrelated
// provider ids, and the resolved routing must be identical.
func TestBundleRoundTripPreservesRouting(t *testing.T) {
	t.Cleanup(func() {
		cfgLock.Lock()
		cfg = nil
		cfgLock.Unlock()
	})
	cfgLock.Lock()
	cfg = &Config{
		Upstreams: []UpstreamProvider{
			prov("src-1", "alpha", "https://a.example/v1", "k1"),
			prov("src-2", "beta", "https://b.example/v1", "k2"),
		},
		ModelRoutes: []ModelRoute{multiRoute("r1", "coding", "src-2", "src-1")},
	}
	cfgLock.Unlock()

	exported := ExportUpstreamBundle()

	// Destination host: same providers, different ids.
	dest := []UpstreamProvider{
		prov("dst-9", "alpha", "https://a.example/v1", "k1"),
		prov("dst-8", "beta", "https://b.example/v1", "k2"),
	}
	res := MergeUpstreamBundle(dest, nil, exported, nil)

	if res.ProvidersAdded != 0 || res.ProvidersSkipped != 2 {
		t.Fatalf("both providers should dedupe: added=%d skipped=%d", res.ProvidersAdded, res.ProvidersSkipped)
	}
	got := res.Routes[0]
	if len(got.Targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(got.Targets))
	}
	// Preferred target was "beta" on the source host; it must still be beta here.
	if got.Targets[0].UpstreamID != "dst-8" {
		t.Errorf("preferred target = %q, want dst-8 (beta)", got.Targets[0].UpstreamID)
	}
	if got.Targets[1].UpstreamID != "dst-9" {
		t.Errorf("backup target = %q, want dst-9 (alpha)", got.Targets[1].UpstreamID)
	}
}
