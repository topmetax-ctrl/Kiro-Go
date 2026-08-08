package config

// Bundle behavior for the Kiro-pool sentinel target. The pool is not a
// configured provider: it names the IMPORTING host's own account pool, so it
// travels through export/validate/merge untouched rather than being resolved
// against the bundle's provider list.

import (
	"strings"
	"testing"

	"kiro-go/metrics"
)

// poolRoute builds a schema-2 route whose last target is the pool sentinel,
// which is the shape the UI produces for "upstream first, pool as backup".
func poolRoute(id, model string, upstreamIDs ...string) ModelRoute {
	r := multiRoute(id, model, upstreamIDs...)
	r.Targets = append(r.Targets, RouteTarget{
		UpstreamID: metrics.KiroPoolID,
		Priority:   len(upstreamIDs),
		Weight:     1,
		Enabled:    true,
	})
	return r
}

// The sentinel can never appear in a bundle's provider list, so validating it
// like a normal reference would reject every file that uses the feature.
func TestValidateAcceptsPoolSentinelTarget(t *testing.T) {
	okProv := prov("p1", "alpha", "https://a.example/v1", "k")

	mixed := bundle([]UpstreamProvider{okProv}, []ModelRoute{poolRoute("r1", "coding", "p1")})
	if err := ValidateUpstreamBundle(&mixed); err != nil {
		t.Errorf("upstream+pool route should validate: %v", err)
	}

	// A pool-only route is legitimate on a host with zero providers.
	poolOnly := bundle(nil, []ModelRoute{poolRoute("r1", "coding")})
	if err := ValidateUpstreamBundle(&poolOnly); err != nil {
		t.Errorf("pool-only route should validate with no providers: %v", err)
	}

	// The sentinel must not become a loophole for genuinely dangling references.
	dangling := bundle([]UpstreamProvider{okProv}, []ModelRoute{poolRoute("r1", "coding", "ghost")})
	if err := ValidateUpstreamBundle(&dangling); err == nil ||
		!strings.Contains(err.Error(), "does not match any provider") {
		t.Errorf("dangling non-pool target should still be rejected, got %v", err)
	}
}

// Legacy schema-1 routes can name the pool too (a v2 export degrades that way,
// and a hand-edited file may do it directly).
func TestValidateAcceptsPoolSentinelLegacyField(t *testing.T) {
	b := bundle(nil, []ModelRoute{{
		ID: "r1", Model: "coding", UpstreamID: metrics.KiroPoolID, Enabled: true,
	}})
	if err := ValidateUpstreamBundle(&b); err != nil {
		t.Errorf("legacy pool-only route should validate: %v", err)
	}
}

// Merging must carry the sentinel through verbatim. Remapping it through idMap
// would find no entry and drop it, silently turning a configured fallback into
// a dead end.
func TestMergeKeepsPoolSentinelTarget(t *testing.T) {
	b := bundle(
		[]UpstreamProvider{prov("remote-1", "alpha", "https://a.example/v1", "k1")},
		[]ModelRoute{poolRoute("r1", "coding", "remote-1")},
	)
	// Colliding local id forces the imported provider to be re-minted, so the
	// upstream target IS remapped while the sentinel must not be.
	existing := []UpstreamProvider{prov("remote-1", "gamma", "https://g.example/v1", "kg")}

	res := MergeUpstreamBundle(existing, nil, b, func() string { return "minted-1" })
	if res.RoutesAdded != 1 {
		t.Fatalf("routes added = %d, want 1", res.RoutesAdded)
	}
	got := res.Routes[0]
	if len(got.Targets) != 2 {
		t.Fatalf("want 2 targets preserved, got %d: %+v", len(got.Targets), got.Targets)
	}
	if got.Targets[0].UpstreamID != "minted-1" {
		t.Errorf("upstream target = %q, want the re-minted id minted-1", got.Targets[0].UpstreamID)
	}
	if got.Targets[1].UpstreamID != metrics.KiroPoolID {
		t.Errorf("pool target = %q, want the sentinel untouched", got.Targets[1].UpstreamID)
	}
	if got.Targets[1].Priority != 1 {
		t.Errorf("pool priority = %d, want 1 (stays the backup tier)", got.Targets[1].Priority)
	}
}

// A pool-only route survives a merge that resolves nothing at all. Without the
// sentinel guard the empty-targets check below would skip it as
// SkipReasonUnknownProvider, which is exactly the silent loss to avoid.
func TestMergeKeepsPoolOnlyRoute(t *testing.T) {
	b := bundle(nil, []ModelRoute{poolRoute("r1", "coding")})

	res := MergeUpstreamBundle(nil, nil, b, nil)
	if res.RoutesAdded != 1 || len(res.Routes) != 1 {
		t.Fatalf("pool-only route should import: added=%d skipped=%d %+v",
			res.RoutesAdded, res.RoutesSkipped, res.SkippedRoutes)
	}
	if got := res.Routes[0].Targets; len(got) != 1 || got[0].UpstreamID != metrics.KiroPoolID {
		t.Errorf("targets = %+v, want the lone pool sentinel", got)
	}
}

// The legacy remap must leave the sentinel alone. Blanking it would combine with
// the "nothing usable left" check to drop a legacy pool-only route entirely.
func TestMergeKeepsPoolSentinelInLegacyField(t *testing.T) {
	b := bundle(nil, []ModelRoute{{
		ID: "r1", Model: "coding", UpstreamID: metrics.KiroPoolID, Enabled: true,
	}})

	res := MergeUpstreamBundle(nil, nil, b, nil)
	if res.RoutesAdded != 1 {
		t.Fatalf("legacy pool route should import: added=%d %+v", res.RoutesAdded, res.SkippedRoutes)
	}
	got := res.Routes[0]
	if got.UpstreamID != metrics.KiroPoolID {
		t.Errorf("legacy upstreamId = %q, want the sentinel preserved", got.UpstreamID)
	}
	// migrateModelRoutes runs during the merge, so the route must arrive with
	// Targets populated — ResolveRoute reads Targets exclusively.
	if len(got.Targets) != 1 || got.Targets[0].UpstreamID != metrics.KiroPoolID {
		t.Errorf("targets = %+v, want a migrated pool target", got.Targets)
	}
}

// Export must never put the sentinel in the legacy field, but it must not leave
// that field EMPTY either when a resolvable target exists: the pre-multi-target
// validator errors on both, and either way aborts the whole file. So the backfill
// degrades past the pool to the first target a v1 build can actually resolve.
func TestExportBackfillsPastPoolSentinel(t *testing.T) {
	t.Cleanup(func() {
		cfgLock.Lock()
		cfg = nil
		cfgLock.Unlock()
	})
	cfgLock.Lock()
	cfg = &Config{
		Upstreams: []UpstreamProvider{prov("u1", "alpha", "https://a.example/v1", "k1")},
		ModelRoutes: []ModelRoute{
			// Pool is the PREFERRED target here, so a naive backfill would pick it.
			multiRoute("r1", "pool-first", metrics.KiroPoolID, "u1"),
			multiRoute("r2", "upstream-first", "u1", metrics.KiroPoolID),
		},
	}
	cfgLock.Unlock()

	b := ExportUpstreamBundle()
	if len(b.Routes) != 2 {
		t.Fatalf("want 2 routes, got %d", len(b.Routes))
	}
	poolFirst := findRoute(t, b.Routes, "pool-first")
	// Degraded to u1 — the sentinel is skipped, not emitted, and not left blank.
	if poolFirst.UpstreamID != "u1" {
		t.Errorf("legacy upstreamId = %q, want u1 (first resolvable target)", poolFirst.UpstreamID)
	}
	// The real preference still lives in Targets, which is what a v2 reader uses,
	// so degrading the legacy field costs nothing on the modern path.
	if len(poolFirst.Targets) != 2 || poolFirst.Targets[0].UpstreamID != metrics.KiroPoolID {
		t.Errorf("targets must be intact for v2 readers: %+v", poolFirst.Targets)
	}
	if up := findRoute(t, b.Routes, "upstream-first"); up.UpstreamID != "u1" {
		t.Errorf("legacy upstreamId = %q, want u1", up.UpstreamID)
	}

	// The bundle we just produced must still validate and re-import cleanly.
	if err := ValidateUpstreamBundle(&b); err != nil {
		t.Errorf("own export should validate: %v", err)
	}
}

// A pool-only route has no resolvable target to degrade to, so the legacy field
// stays empty. The route must still be exported with its Targets intact: a v2
// importer handles it correctly, and dropping it would lose config on the path
// that actually matters.
func TestExportPoolOnlyRouteKeepsTargets(t *testing.T) {
	t.Cleanup(func() {
		cfgLock.Lock()
		cfg = nil
		cfgLock.Unlock()
	})
	cfgLock.Lock()
	cfg = &Config{
		Upstreams:   []UpstreamProvider{},
		ModelRoutes: []ModelRoute{multiRoute("r1", "pool-only", metrics.KiroPoolID)},
	}
	cfgLock.Unlock()

	b := ExportUpstreamBundle()
	got := findRoute(t, b.Routes, "pool-only")
	if got.UpstreamID != "" {
		t.Errorf("legacy upstreamId = %q, want empty (nothing resolvable to degrade to)", got.UpstreamID)
	}
	if len(got.Targets) != 1 || got.Targets[0].UpstreamID != metrics.KiroPoolID {
		t.Errorf("targets = %+v, want the lone pool sentinel preserved", got.Targets)
	}
	if err := ValidateUpstreamBundle(&b); err != nil {
		t.Errorf("own export should validate: %v", err)
	}
}

// Full round trip with the pool in the chain: export from one host, import into
// another whose provider ids differ, and the try-order must be identical.
func TestPoolRouteRoundTripPreservesOrder(t *testing.T) {
	t.Cleanup(func() {
		cfgLock.Lock()
		cfg = nil
		cfgLock.Unlock()
	})
	cfgLock.Lock()
	cfg = &Config{
		Upstreams:   []UpstreamProvider{prov("src-1", "alpha", "https://a.example/v1", "k1")},
		ModelRoutes: []ModelRoute{poolRoute("r1", "coding", "src-1")},
	}
	cfgLock.Unlock()

	exported := ExportUpstreamBundle()
	dest := []UpstreamProvider{prov("dst-7", "alpha", "https://a.example/v1", "k1")}
	res := MergeUpstreamBundle(dest, nil, exported, nil)

	if res.RoutesAdded != 1 {
		t.Fatalf("routes added = %d, want 1 (%+v)", res.RoutesAdded, res.SkippedRoutes)
	}
	got := res.Routes[0].Targets
	if len(got) != 2 {
		t.Fatalf("want 2 targets, got %d", len(got))
	}
	if got[0].UpstreamID != "dst-7" {
		t.Errorf("preferred target = %q, want dst-7 (alpha)", got[0].UpstreamID)
	}
	if got[1].UpstreamID != metrics.KiroPoolID {
		t.Errorf("backup target = %q, want the pool sentinel", got[1].UpstreamID)
	}
}
