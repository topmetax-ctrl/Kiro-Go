package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// seqID returns a deterministic ID generator so tests can assert on minted IDs.
func seqID() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("gen-%d", n)
	}
}

func prov(id, name, base, key string) UpstreamProvider {
	return UpstreamProvider{ID: id, Name: name, BaseURL: base, ApiKey: key, Enabled: true}
}

func route(id, model, upstreamID string) ModelRoute {
	return ModelRoute{ID: id, Model: model, UpstreamID: upstreamID, Enabled: true}
}

func bundle(providers []UpstreamProvider, routes []ModelRoute) UpstreamBundle {
	return UpstreamBundle{
		Version:   Version,
		Kind:      UpstreamBundleKind,
		Schema:    UpstreamBundleSchema,
		Providers: providers,
		Routes:    routes,
	}
}

func findProvider(t *testing.T, providers []UpstreamProvider, name string) UpstreamProvider {
	t.Helper()
	for _, p := range providers {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("provider %q not found in %+v", name, providers)
	return UpstreamProvider{}
}

func findRoute(t *testing.T, routes []ModelRoute, model string) ModelRoute {
	t.Helper()
	for _, r := range routes {
		if r.Model == model {
			return r
		}
	}
	t.Fatalf("route %q not found in %+v", model, routes)
	return ModelRoute{}
}

func TestMergeIntoEmptyConfigKeepsIDs(t *testing.T) {
	b := bundle(
		[]UpstreamProvider{prov("p1", "9router", "http://a/v1", "sk-a"), prov("p2", "xpiki", "http://b/v1", "sk-b")},
		[]ModelRoute{route("r1", "coding", "p1"), route("r2", "writing", "p2")},
	)
	res := MergeUpstreamBundle(nil, nil, b, seqID())

	if res.ProvidersAdded != 2 || res.ProvidersSkipped != 0 {
		t.Fatalf("providers: added=%d skipped=%d, want 2/0", res.ProvidersAdded, res.ProvidersSkipped)
	}
	if res.RoutesAdded != 2 || res.RoutesSkipped != 0 {
		t.Fatalf("routes: added=%d skipped=%d, want 2/0", res.RoutesAdded, res.RoutesSkipped)
	}
	if got := findProvider(t, res.Providers, "9router").ID; got != "p1" {
		t.Errorf("provider ID = %q, want preserved %q", got, "p1")
	}
	if got := findRoute(t, res.Routes, "coding").UpstreamID; got != "p1" {
		t.Errorf("route upstreamId = %q, want %q", got, "p1")
	}
}

// The single most important case: a duplicate provider is skipped, so the
// imported route that referenced it must be re-pointed at the EXISTING
// provider's ID, not the discarded imported one.
func TestMergeDuplicateProviderRemapsRouteToExistingID(t *testing.T) {
	existing := []UpstreamProvider{prov("A", "9router", "http://a/v1", "sk-existing")}
	b := bundle(
		[]UpstreamProvider{prov("B", "9router", "http://a/v1", "sk-imported")},
		[]ModelRoute{route("r1", "coding", "B")},
	)
	res := MergeUpstreamBundle(existing, nil, b, seqID())

	if res.ProvidersAdded != 0 || res.ProvidersSkipped != 1 {
		t.Fatalf("providers: added=%d skipped=%d, want 0/1", res.ProvidersAdded, res.ProvidersSkipped)
	}
	if res.RoutesAdded != 1 {
		t.Fatalf("routes added = %d, want 1", res.RoutesAdded)
	}
	if got := findRoute(t, res.Routes, "coding").UpstreamID; got != "A" {
		t.Fatalf("route upstreamId = %q, want remapped to existing %q", got, "A")
	}
	if len(res.Providers) != 1 {
		t.Errorf("provider count = %d, want 1 (no duplicate created)", len(res.Providers))
	}
	// The existing API key must survive: merge never overwrites.
	if got := res.Providers[0].ApiKey; got != "sk-existing" {
		t.Errorf("apiKey = %q, want existing key preserved", got)
	}
	if len(res.SkippedProviders) != 1 || res.SkippedProviders[0].Reason != SkipReasonDuplicate {
		t.Errorf("SkippedProviders = %+v, want one %q", res.SkippedProviders, SkipReasonDuplicate)
	}
}

func TestMergeProviderDedupNormalizesTrailingSlashAndCase(t *testing.T) {
	existing := []UpstreamProvider{prov("A", "9router", "http://host/v1", "sk-a")}
	b := bundle([]UpstreamProvider{prov("B", "9Router", "HTTP://HOST/v1/", "sk-b")},
		[]ModelRoute{route("r1", "coding", "B")})
	res := MergeUpstreamBundle(existing, nil, b, seqID())

	if res.ProvidersAdded != 0 || res.ProvidersSkipped != 1 {
		t.Fatalf("providers: added=%d skipped=%d, want 0/1", res.ProvidersAdded, res.ProvidersSkipped)
	}
	if got := findRoute(t, res.Routes, "coding").UpstreamID; got != "A" {
		t.Errorf("route upstreamId = %q, want %q", got, "A")
	}
}

// One gateway host commonly fronts several logical providers differing only by
// name and key, so name is part of the dedup key.
func TestMergeSameBaseURLDifferentNameKeepsBoth(t *testing.T) {
	existing := []UpstreamProvider{prov("A", "cheap", "http://host/v1", "sk-a")}
	b := bundle([]UpstreamProvider{prov("B", "premium", "http://host/v1", "sk-b")},
		[]ModelRoute{route("r1", "coding", "B")})
	res := MergeUpstreamBundle(existing, nil, b, seqID())

	if res.ProvidersAdded != 1 || res.ProvidersSkipped != 0 {
		t.Fatalf("providers: added=%d skipped=%d, want 1/0", res.ProvidersAdded, res.ProvidersSkipped)
	}
	if got := findRoute(t, res.Routes, "coding").UpstreamID; got != "B" {
		t.Errorf("route upstreamId = %q, want its own provider %q", got, "B")
	}
}

func TestMergeDuplicateRouteModelSkippedAndExistingUntouched(t *testing.T) {
	existing := []UpstreamProvider{prov("A", "old", "http://a/v1", "sk-a")}
	existingRoutes := []ModelRoute{route("r-old", "coding", "A")}
	b := bundle([]UpstreamProvider{prov("B", "new", "http://b/v1", "sk-b")},
		[]ModelRoute{route("r-new", "coding", "B")})
	res := MergeUpstreamBundle(existing, existingRoutes, b, seqID())

	if res.RoutesAdded != 0 || res.RoutesSkipped != 1 {
		t.Fatalf("routes: added=%d skipped=%d, want 0/1", res.RoutesAdded, res.RoutesSkipped)
	}
	if len(res.Routes) != 1 {
		t.Fatalf("route count = %d, want 1", len(res.Routes))
	}
	if res.Routes[0].UpstreamID != "A" || res.Routes[0].ID != "r-old" {
		t.Errorf("existing route mutated: %+v", res.Routes[0])
	}
	if len(res.SkippedRoutes) != 1 || res.SkippedRoutes[0].Reason != SkipReasonDuplicate {
		t.Errorf("SkippedRoutes = %+v, want one %q", res.SkippedRoutes, SkipReasonDuplicate)
	}
}

// FindEnabledRoute matches model names case-sensitively, so route dedup must too.
func TestMergeRouteModelCaseSensitiveKeepsBoth(t *testing.T) {
	existing := []UpstreamProvider{prov("A", "p", "http://a/v1", "sk-a")}
	existingRoutes := []ModelRoute{route("r-old", "coding", "A")}
	b := bundle([]UpstreamProvider{prov("A", "p", "http://a/v1", "sk-a")},
		[]ModelRoute{route("r-new", "Coding", "A")})
	res := MergeUpstreamBundle(existing, existingRoutes, b, seqID())

	if res.RoutesAdded != 1 || res.RoutesSkipped != 0 {
		t.Fatalf("routes: added=%d skipped=%d, want 1/0", res.RoutesAdded, res.RoutesSkipped)
	}
}

// FindEnabledRoute trims the model name, so " coding" and "coding" are the same route.
func TestMergeRouteModelWhitespaceDedups(t *testing.T) {
	existing := []UpstreamProvider{prov("A", "p", "http://a/v1", "sk-a")}
	existingRoutes := []ModelRoute{route("r-old", "coding", "A")}
	b := bundle([]UpstreamProvider{prov("A", "p", "http://a/v1", "sk-a")},
		[]ModelRoute{route("r-new", "  coding ", "A")})
	res := MergeUpstreamBundle(existing, existingRoutes, b, seqID())

	if res.RoutesAdded != 0 || res.RoutesSkipped != 1 {
		t.Fatalf("routes: added=%d skipped=%d, want 0/1", res.RoutesAdded, res.RoutesSkipped)
	}
}

// An imported ID may collide with an unrelated existing entry; the import gets a
// fresh ID and its routes follow it.
func TestMergeIDCollisionWithUnrelatedProviderMintsFreshID(t *testing.T) {
	existing := []UpstreamProvider{prov("A", "one", "http://a/v1", "sk-a")}
	existingRoutes := []ModelRoute{route("r-old", "old-model", "A")}
	b := bundle([]UpstreamProvider{prov("A", "two", "http://b/v1", "sk-b")},
		[]ModelRoute{route("r-new", "new-model", "A")})
	res := MergeUpstreamBundle(existing, existingRoutes, b, seqID())

	if res.ProvidersAdded != 1 {
		t.Fatalf("providers added = %d, want 1", res.ProvidersAdded)
	}
	imported := findProvider(t, res.Providers, "two")
	if imported.ID != "gen-1" {
		t.Errorf("imported provider ID = %q, want freshly minted %q", imported.ID, "gen-1")
	}
	if got := findProvider(t, res.Providers, "one").ID; got != "A" {
		t.Errorf("existing provider ID = %q, want unchanged %q", got, "A")
	}
	if got := findRoute(t, res.Routes, "new-model").UpstreamID; got != "gen-1" {
		t.Errorf("imported route upstreamId = %q, want %q", got, "gen-1")
	}
	if got := findRoute(t, res.Routes, "old-model").UpstreamID; got != "A" {
		t.Errorf("existing route upstreamId = %q, want unchanged %q", got, "A")
	}
}

func TestMergeEmptyImportedIDsGetGenerated(t *testing.T) {
	b := bundle([]UpstreamProvider{prov("", "p", "http://a/v1", "sk-a")},
		[]ModelRoute{route("", "coding", "")})
	// Route upstreamId "" cannot resolve, so point it at the provider's (empty)
	// imported ID — which is exactly the ambiguous case. Use an explicit ID
	// instead to exercise only the empty-route-ID path.
	b.Providers[0].ID = "p1"
	b.Routes[0].UpstreamID = "p1"

	res := MergeUpstreamBundle(nil, nil, b, seqID())
	if res.ProvidersAdded != 1 || res.RoutesAdded != 1 {
		t.Fatalf("added providers=%d routes=%d, want 1/1", res.ProvidersAdded, res.RoutesAdded)
	}
	if res.Routes[0].ID == "" {
		t.Error("route ID still empty, want generated")
	}
	if res.Routes[0].UpstreamID != "p1" {
		t.Errorf("route upstreamId = %q, want %q", res.Routes[0].UpstreamID, "p1")
	}
}

// Duplicates inside the bundle itself collapse, and both routes follow the one
// provider that was actually added.
func TestMergeDuplicateProvidersWithinBundle(t *testing.T) {
	b := bundle(
		[]UpstreamProvider{prov("p1", "9router", "http://a/v1", "sk-a"), prov("p2", "9router", "http://a/v1", "sk-a")},
		[]ModelRoute{route("r1", "coding", "p1"), route("r2", "writing", "p2")},
	)
	res := MergeUpstreamBundle(nil, nil, b, seqID())

	if res.ProvidersAdded != 1 || res.ProvidersSkipped != 1 {
		t.Fatalf("providers: added=%d skipped=%d, want 1/1", res.ProvidersAdded, res.ProvidersSkipped)
	}
	if res.RoutesAdded != 2 {
		t.Fatalf("routes added = %d, want 2", res.RoutesAdded)
	}
	a := findRoute(t, res.Routes, "coding").UpstreamID
	c := findRoute(t, res.Routes, "writing").UpstreamID
	if a != "p1" || c != "p1" {
		t.Errorf("route upstreamIds = %q/%q, want both %q", a, c, "p1")
	}
}

// Defense in depth: ValidateUpstreamBundle rejects this, but Merge must not
// panic or leave a dangling reference if called directly.
func TestMergeUnresolvableRouteSkipped(t *testing.T) {
	b := bundle([]UpstreamProvider{prov("p1", "p", "http://a/v1", "sk-a")},
		[]ModelRoute{route("r1", "coding", "does-not-exist")})
	res := MergeUpstreamBundle(nil, nil, b, seqID())

	if res.RoutesAdded != 0 || res.RoutesSkipped != 1 {
		t.Fatalf("routes: added=%d skipped=%d, want 0/1", res.RoutesAdded, res.RoutesSkipped)
	}
	if len(res.SkippedRoutes) != 1 || res.SkippedRoutes[0].Reason != SkipReasonUnknownProvider {
		t.Errorf("SkippedRoutes = %+v, want one %q", res.SkippedRoutes, SkipReasonUnknownProvider)
	}
	if len(res.Routes) != 0 {
		t.Errorf("routes = %+v, want none", res.Routes)
	}
}

// Guards against an append-into-spare-capacity aliasing bug.
func TestMergeDoesNotMutateInputSlices(t *testing.T) {
	existing := make([]UpstreamProvider, 1, 8)
	existing[0] = prov("A", "one", "http://a/v1", "sk-a")
	existingRoutes := make([]ModelRoute, 1, 8)
	existingRoutes[0] = route("r-old", "old-model", "A")

	b := bundle([]UpstreamProvider{prov("B", "two", "http://b/v1", "sk-b")},
		[]ModelRoute{route("r-new", "new-model", "B")})
	MergeUpstreamBundle(existing, existingRoutes, b, seqID())

	if len(existing) != 1 || existing[0].ID != "A" || existing[0].Name != "one" {
		t.Errorf("input providers mutated: %+v", existing)
	}
	if len(existingRoutes) != 1 || existingRoutes[0].Model != "old-model" {
		t.Errorf("input routes mutated: %+v", existingRoutes)
	}
}

func TestValidateUpstreamBundleRejects(t *testing.T) {
	okProv := prov("p1", "p", "http://a/v1", "sk-a")

	tests := []struct {
		name   string
		bundle UpstreamBundle
		want   string
	}{
		{"wrong kind", UpstreamBundle{Kind: "kiro-go-accounts", Providers: []UpstreamProvider{okProv}}, "unexpected kind"},
		{"future schema", UpstreamBundle{Kind: UpstreamBundleKind, Schema: 2, Providers: []UpstreamProvider{okProv}}, "unsupported schema"},
		{"empty", bundle(nil, nil), "no providers or routes"},
		{"empty baseUrl", bundle([]UpstreamProvider{prov("p1", "p", "", "k")}, nil), "baseUrl is required"},
		{"not a url", bundle([]UpstreamProvider{prov("p1", "p", "not a url", "k")}, nil), "absolute http(s) URL"},
		{"wrong scheme", bundle([]UpstreamProvider{prov("p1", "p", "ftp://h/v1", "k")}, nil), "absolute http(s) URL"},
		{"blank model", bundle([]UpstreamProvider{okProv}, []ModelRoute{route("r1", "   ", "p1")}), "model is required"},
		{"blank upstreamId", bundle([]UpstreamProvider{okProv}, []ModelRoute{route("r1", "coding", "")}), "upstreamId is required"},
		{"dangling upstreamId", bundle([]UpstreamProvider{okProv}, []ModelRoute{route("r1", "coding", "nope")}), "does not match any provider"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.bundle
			err := ValidateUpstreamBundle(&b)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want containing %q", err, tc.want)
			}
			if !errorIsInvalidBundle(err) {
				t.Errorf("error %v does not wrap ErrInvalidUpstreamBundle", err)
			}
		})
	}
}

func TestValidateUpstreamBundleRejectsAccountsExportShape(t *testing.T) {
	// The accounts export has no "kind" field, so the kind check cannot catch it.
	b := UpstreamBundle{
		Version:  Version,
		Accounts: json.RawMessage(`[{"id":"a","email":"x@y.z"}]`),
	}
	err := ValidateUpstreamBundle(&b)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "accounts export") {
		t.Errorf("error = %v, want it to name the accounts export", err)
	}
}

func TestValidateUpstreamBundleRejectsTooManyRoutes(t *testing.T) {
	routes := make([]ModelRoute, maxBundleRoutes+1)
	for i := range routes {
		routes[i] = route(fmt.Sprintf("r%d", i), fmt.Sprintf("m%d", i), "p1")
	}
	b := bundle([]UpstreamProvider{prov("p1", "p", "http://a/v1", "k")}, routes)
	err := ValidateUpstreamBundle(&b)
	if err == nil || !strings.Contains(err.Error(), "too many routes") {
		t.Fatalf("err = %v, want 'too many routes'", err)
	}
}

func TestValidateUpstreamBundleAcceptsMissingKindAndSchema(t *testing.T) {
	// Hand-written bundles without the envelope fields must still import.
	b := UpstreamBundle{
		Providers: []UpstreamProvider{prov("p1", "p", "http://a/v1", "k")},
		Routes:    []ModelRoute{route("r1", "coding", "p1")},
	}
	if err := ValidateUpstreamBundle(&b); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func errorIsInvalidBundle(err error) bool {
	for e := err; e != nil; {
		if e == ErrInvalidUpstreamBundle {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func seedUpstreams(t *testing.T) {
	t.Helper()
	providers := []UpstreamProvider{
		{ID: "p1", Name: "9router", BaseURL: "http://a/v1", ApiKey: "sk-secret-a", Enabled: true},
		{ID: "p2", Name: "xpiki", BaseURL: "http://b/v1", ApiKey: "sk-secret-b", ProxyURL: "socks5://h:1080", Enabled: true},
	}
	routes := []ModelRoute{
		{ID: "r1", Model: "coding", UpstreamID: "p1", TargetModel: "gpt-5-codex", Enabled: true},
		{ID: "r2", Model: "writing", UpstreamID: "p2", Enabled: true},
	}
	if err := UpdateUpstreamConfig(providers, routes); err != nil {
		t.Fatalf("seed upstreams: %v", err)
	}
}

// Regression guard: the export must NOT mask keys, or the file is useless on
// another host.
func TestExportUpstreamBundleIncludesRealApiKeys(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	seedUpstreams(t)

	b := ExportUpstreamBundle()
	if b.Kind != UpstreamBundleKind || b.Schema != UpstreamBundleSchema {
		t.Errorf("envelope = kind %q schema %d, want %q/%d", b.Kind, b.Schema, UpstreamBundleKind, UpstreamBundleSchema)
	}
	if b.ExportedAt == 0 {
		t.Error("exportedAt not set")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "sk-secret-a") || !strings.Contains(s, "sk-secret-b") {
		t.Errorf("export is missing real API keys: %s", s)
	}
	if strings.Contains(s, "****") {
		t.Errorf("export contains masked keys: %s", s)
	}
	if !strings.Contains(s, "socks5://h:1080") {
		t.Error("export dropped proxyURL")
	}
}

// Importing an export back into the same config must be a no-op.
func TestImportUpstreamBundleIsIdempotent(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	seedUpstreams(t)

	res, err := ImportUpstreamBundle(ExportUpstreamBundle())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.ProvidersAdded != 0 || res.RoutesAdded != 0 {
		t.Fatalf("added providers=%d routes=%d, want 0/0", res.ProvidersAdded, res.RoutesAdded)
	}
	if res.ProvidersSkipped != 2 || res.RoutesSkipped != 2 {
		t.Fatalf("skipped providers=%d routes=%d, want 2/2", res.ProvidersSkipped, res.RoutesSkipped)
	}
	providers, routes := GetUpstreamConfig()
	if len(providers) != 2 || len(routes) != 2 {
		t.Fatalf("config grew: %d providers, %d routes", len(providers), len(routes))
	}
}

// The cross-machine path: export on one config, import into a fresh one.
func TestImportUpstreamBundleIntoFreshConfig(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	seedUpstreams(t)
	exported := ExportUpstreamBundle()

	// Simulate a different host.
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("re-init config: %v", err)
	}
	res, err := ImportUpstreamBundle(exported)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.ProvidersAdded != 2 || res.RoutesAdded != 2 {
		t.Fatalf("added providers=%d routes=%d, want 2/2", res.ProvidersAdded, res.RoutesAdded)
	}

	providers, routes := GetUpstreamConfig()
	p := findProvider(t, providers, "9router")
	if p.ApiKey != "sk-secret-a" {
		t.Errorf("apiKey = %q, want %q", p.ApiKey, "sk-secret-a")
	}
	r := findRoute(t, routes, "coding")
	if r.UpstreamID != p.ID {
		t.Errorf("route upstreamId = %q, want provider ID %q", r.UpstreamID, p.ID)
	}
	if r.TargetModel != "gpt-5-codex" {
		t.Errorf("targetModel = %q, want preserved", r.TargetModel)
	}

	// The imported config must actually be usable for forwarding.
	gotRoute, gotProv := FindEnabledRoute("coding")
	if gotRoute == nil || gotProv == nil {
		t.Fatal("FindEnabledRoute found nothing after import")
	}
	if gotProv.ApiKey != "sk-secret-a" {
		t.Errorf("resolved provider apiKey = %q, want %q", gotProv.ApiKey, "sk-secret-a")
	}
}

func TestImportUpstreamBundleRejectsInvalidWithoutMutating(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	seedUpstreams(t)

	_, err := ImportUpstreamBundle(UpstreamBundle{
		Kind:      "kiro-go-accounts",
		Providers: []UpstreamProvider{prov("x", "bad", "http://c/v1", "k")},
	})
	if err == nil {
		t.Fatal("expected error for wrong kind")
	}
	providers, routes := GetUpstreamConfig()
	if len(providers) != 2 || len(routes) != 2 {
		t.Fatalf("config mutated on rejected import: %d providers, %d routes", len(providers), len(routes))
	}
}
