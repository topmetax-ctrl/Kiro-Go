package config

// Export/import of the forwarding configuration (upstream providers + model
// routes) as a self-contained bundle.
//
// The bundle carries REAL provider API keys — unlike the admin GET /upstreams
// response, which masks them — so a file exported on one host can be imported on
// another and work immediately. Treat exported bundles as secret material.
//
// Import semantics are merge-skipping-duplicates: an entry that already exists
// is never overwritten, only reported as skipped. The interesting part is ID
// remapping — see MergeUpstreamBundle.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// UpstreamBundle is the on-disk/on-wire format for a forwarding-config export.
type UpstreamBundle struct {
	// Version mirrors config.Version at export time. Informational only —
	// diagnosis aid for humans reading the file; the importer gates on Schema.
	Version string `json:"version"`
	// Kind distinguishes this from the accounts export, which is also JSON with
	// a "version" field and is the file users are most likely to paste by mistake.
	Kind string `json:"kind"`
	// Schema is the format version the importer actually checks. Absent (0) is
	// accepted so hand-written bundles work.
	Schema     int                `json:"schema"`
	ExportedAt int64              `json:"exportedAt"`
	Providers  []UpstreamProvider `json:"providers"`
	Routes     []ModelRoute       `json:"routes"`

	// Accounts is never written by this package. It exists only so the importer
	// can recognize an accounts export (which has no "kind" field, so the Kind
	// check below cannot catch it) and reject it with a message that names the
	// actual mistake rather than "bundle contains no providers or routes".
	Accounts json.RawMessage `json:"accounts,omitempty"`
}

const (
	// UpstreamBundleKind is the required "kind" value for a forwarding bundle.
	UpstreamBundleKind = "kiro-go-upstreams"
	// UpstreamBundleSchema is the highest schema version this build understands.
	//
	// 1: routes carry a single upstreamId/targetModel pair.
	// 2: routes carry a ranked `targets` list. Legacy fields are still emitted, so
	//    a v2 bundle also imports into a v1 build (it reads upstreamId and ignores
	//    targets, losing only the backup targets). Bumping the constant is what
	//    makes a v1 build reject a *future* v3 bundle rather than misread it.
	UpstreamBundleSchema = 2

	maxBundleProviders = 200
	maxBundleRoutes    = 2000

	// SkipReasonDuplicate marks an entry skipped because an equivalent one exists.
	SkipReasonDuplicate = "duplicate"
	// SkipReasonUnknownProvider marks a route whose upstreamId resolves to nothing.
	SkipReasonUnknownProvider = "unknownProvider"
)

// ErrInvalidUpstreamBundle wraps every validation failure so callers can map the
// error to a 400 rather than a 500.
var ErrInvalidUpstreamBundle = errors.New("invalid upstream bundle")

// SkippedItem labels one entry the merge did not apply. Reason is a machine
// token (SkipReason*) that the UI maps to a localized string; it is never a
// pre-translated message.
type SkippedItem struct {
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// UpstreamMergeResult is the outcome of merging a bundle into an existing
// config. Providers/Routes are the merged slices; the counters and skip lists
// describe what happened.
type UpstreamMergeResult struct {
	Providers        []UpstreamProvider
	Routes           []ModelRoute
	ProvidersAdded   int
	ProvidersSkipped int
	RoutesAdded      int
	RoutesSkipped    int
	SkippedProviders []SkippedItem
	SkippedRoutes    []SkippedItem
}

// normUpstreamBaseURL canonicalizes a base URL for duplicate detection. The
// trailing slash must be ignored because tryForwardUpstream builds request URLs
// as TrimRight(BaseURL, "/") + subPath — "http://h/v1" and "http://h/v1/" are the
// same endpoint. Lowercasing the whole URL is mildly over-eager on the path
// component but is what makes "HTTP://Host/v1/" match "http://host/v1", which is
// the realistic form of this collision.
func normUpstreamBaseURL(s string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
}

// providerDedupKey identifies "the same provider" for merge purposes.
//
// Name is part of the key deliberately: one gateway host commonly fronts several
// logical providers distinguished only by API key (this is exactly how 9router
// and LiteLLM deployments look), and keying on baseURL alone would collapse them
// into one. The NUL separator prevents "ab"+"c" colliding with "a"+"bc".
func providerDedupKey(p UpstreamProvider) string {
	return normUpstreamBaseURL(p.BaseURL) + "\x00" + strings.ToLower(strings.TrimSpace(p.Name))
}

// routeDedupKey identifies "the same route". Matching is case-SENSITIVE to
// mirror FindEnabledRoute, which compares TrimSpace(r.Model) against the client
// model with no case folding. A second route on an identical model name could
// never be reached, so importing one would only create dead config.
func routeDedupKey(r ModelRoute) string {
	return strings.TrimSpace(r.Model)
}

// providerLabel is a human-readable identifier for skip reporting.
func providerLabel(p UpstreamProvider) string {
	name := strings.TrimSpace(p.Name)
	base := strings.TrimSpace(p.BaseURL)
	if name == "" {
		return base
	}
	return name + " (" + base + ")"
}

// isKiroPoolTarget reports whether an upstream id is the Kiro-pool sentinel.
//
// Trimming here rather than at each call site is deliberate: a hand-edited or
// third-party bundle can carry a padded id, and ResolveRoute trims before
// comparing, so every other site that decides "is this the pool" has to agree
// with it or a padded sentinel survives import and is then silently dropped at
// resolution time.
func isKiroPoolTarget(upstreamID string) bool {
	return strings.TrimSpace(upstreamID) == KiroPoolTargetID
}

// firstNonPoolTarget returns the first target that names a real configured
// upstream, skipping Kiro-pool sentinels. ok is false for a pool-only route.
func firstNonPoolTarget(targets []RouteTarget) (RouteTarget, bool) {
	for _, t := range targets {
		if isKiroPoolTarget(t.UpstreamID) {
			continue
		}
		if strings.TrimSpace(t.UpstreamID) == "" {
			continue
		}
		return t, true
	}
	return RouteTarget{}, false
}

// ExportUpstreamBundle snapshots the current forwarding config, API keys
// included and unmasked.
func ExportUpstreamBundle() UpstreamBundle {
	providers, routes := GetUpstreamConfig()
	if providers == nil {
		providers = []UpstreamProvider{}
	}
	if routes == nil {
		routes = []ModelRoute{}
	}
	// Backfill the legacy 1:1 fields from the top target on the way out. A route
	// created in this build has Targets but no upstreamId, and a v1 importer
	// requires upstreamId on every route — it would reject the entire file rather
	// than degrade. Emitting both keeps the export readable by older builds, which
	// then see the preferred target and ignore the backups.
	for i := range routes {
		if len(routes[i].Targets) == 0 || strings.TrimSpace(routes[i].UpstreamID) != "" {
			continue
		}
		// Backfill from the first target a v1 importer can actually resolve, which
		// means skipping past the Kiro-pool sentinel: it names the built-in pool
		// rather than a configured upstream, so it never appears in the bundle's
		// provider list and a v1 importer would reject the file for naming a
		// provider that is not there.
		//
		// Leaving the legacy field EMPTY is not the graceful alternative — the
		// pre-multi-target validator errors out on an empty upstreamId too, which
		// aborts the whole import. Degrading to the best resolvable target keeps the
		// rest of the file loadable, and a v1 build has no pool-as-target concept to
		// degrade to anyway.
		top, ok := firstNonPoolTarget(routes[i].Targets)
		if !ok {
			// Pool-only route: no target a v1 build could point at, and no legacy
			// value that would help it — the sentinel and an empty id both make v1
			// reject the file. The route is still exported with its Targets, because
			// silently dropping it would lose config on the path that matters (a v2
			// importer, which reads Targets and handles the pool correctly). A v1
			// build importing an export that contains a pool-only route fails on the
			// whole file; that is a real limitation, and the alternative — losing the
			// route on every modern import — is worse.
			continue
		}
		routes[i].UpstreamID = top.UpstreamID
		routes[i].TargetModel = top.TargetModel
	}
	return UpstreamBundle{
		Version:    Version,
		Kind:       UpstreamBundleKind,
		Schema:     UpstreamBundleSchema,
		ExportedAt: time.Now().UnixMilli(),
		Providers:  providers,
		Routes:     routes,
	}
}

// ValidateUpstreamBundle rejects structurally unusable bundles. It is agnostic
// of local state: a route's upstreamId must resolve within the bundle itself.
// Resolvability is validated here rather than silently skipped at merge time so
// that a hand-edited file fails loudly instead of quietly losing routes.
//
// Every returned error wraps ErrInvalidUpstreamBundle.
func ValidateUpstreamBundle(b *UpstreamBundle) error {
	if b == nil {
		return fmt.Errorf("%w: empty body", ErrInvalidUpstreamBundle)
	}
	if k := strings.TrimSpace(b.Kind); k != "" && k != UpstreamBundleKind {
		return fmt.Errorf("%w: unexpected kind %q; this does not look like a forwarding config export",
			ErrInvalidUpstreamBundle, k)
	}
	if b.Schema > UpstreamBundleSchema {
		return fmt.Errorf("%w: unsupported schema version %d (this build understands up to %d)",
			ErrInvalidUpstreamBundle, b.Schema, UpstreamBundleSchema)
	}
	// An accounts export carries no "kind", so the check above misses it. It is
	// the file users are most likely to paste by mistake, so name the mistake.
	if len(b.Accounts) > 0 && string(b.Accounts) != "null" {
		return fmt.Errorf("%w: this looks like an accounts export, not a forwarding config export",
			ErrInvalidUpstreamBundle)
	}
	if len(b.Providers) == 0 && len(b.Routes) == 0 {
		return fmt.Errorf("%w: bundle contains no providers or routes", ErrInvalidUpstreamBundle)
	}
	if len(b.Providers) > maxBundleProviders {
		return fmt.Errorf("%w: too many providers (%d, max %d)",
			ErrInvalidUpstreamBundle, len(b.Providers), maxBundleProviders)
	}
	if len(b.Routes) > maxBundleRoutes {
		return fmt.Errorf("%w: too many routes (%d, max %d)",
			ErrInvalidUpstreamBundle, len(b.Routes), maxBundleRoutes)
	}

	// Provider IDs present in this bundle, for route resolution below.
	inBundle := make(map[string]bool, len(b.Providers))
	for i, p := range b.Providers {
		base := strings.TrimSpace(p.BaseURL)
		if base == "" {
			return fmt.Errorf("%w: provider %d: baseUrl is required", ErrInvalidUpstreamBundle, i+1)
		}
		u, err := url.Parse(base)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: provider %d (%s): baseUrl must be an absolute http(s) URL",
				ErrInvalidUpstreamBundle, i+1, providerLabel(p))
		}
		if id := strings.TrimSpace(p.ID); id != "" {
			inBundle[id] = true
		}
	}

	for i, r := range b.Routes {
		label := strings.TrimSpace(r.Model)
		if label == "" {
			return fmt.Errorf("%w: route %d: model is required", ErrInvalidUpstreamBundle, i+1)
		}
		// A route may describe its destination either way: `targets` (schema 2) or
		// the legacy `upstreamId` (schema 1). Requiring the legacy field would
		// reject a hand-written v2 bundle, and requiring targets would reject every
		// existing v1 file, so accept either and validate whichever is present.
		if len(r.Targets) == 0 {
			upID := strings.TrimSpace(r.UpstreamID)
			if upID == "" {
				return fmt.Errorf("%w: route %q: needs either targets or upstreamId",
					ErrInvalidUpstreamBundle, label)
			}
			// The Kiro-pool sentinel resolves against the importing host's built-in
			// account pool, not against the bundle's provider list, so it is valid
			// everywhere by definition. Same reasoning as the targets loop below.
			if upID == KiroPoolTargetID {
				continue
			}
			if !inBundle[upID] {
				return fmt.Errorf("%w: route %q: upstreamId %q does not match any provider in this file",
					ErrInvalidUpstreamBundle, label, upID)
			}
			continue
		}
		for j, tg := range r.Targets {
			upID := strings.TrimSpace(tg.UpstreamID)
			if upID == "" {
				return fmt.Errorf("%w: route %q target %d: upstreamId is required",
					ErrInvalidUpstreamBundle, label, j+1)
			}
			// The Kiro-pool sentinel names the built-in account pool, which is not a
			// configured provider and so is never listed in the bundle. It is valid on
			// every host by definition, so it needs no bundle-local reference.
			if upID == KiroPoolTargetID {
				continue
			}
			if !inBundle[upID] {
				return fmt.Errorf("%w: route %q target %d: upstreamId %q does not match any provider in this file",
					ErrInvalidUpstreamBundle, label, j+1, upID)
			}
		}
	}
	return nil
}

// MergeUpstreamBundle merges b into the given slices without touching global
// state or mutating its inputs. Duplicates are skipped, never overwritten.
//
// The subtle part is ID remapping. Imported provider IDs cannot be trusted:
//
//   - A provider that duplicates an existing one is skipped, so its routes must
//     be re-pointed at the EXISTING provider's ID, not the (discarded) imported one.
//   - An imported ID may collide with an unrelated existing entry, in which case
//     the imported entry gets a freshly minted ID.
//   - Duplicates within the bundle itself fall out for free, because the
//     key->finalID index is updated as we go.
//
// Every imported route therefore resolves its UpstreamID through idMap. A route
// whose provider resolves to nothing is skipped with SkipReasonUnknownProvider
// — defense in depth; ValidateUpstreamBundle rejects that case up front.
//
// newID mints replacement IDs; pass nil to use GenerateMachineId.
func MergeUpstreamBundle(
	providers []UpstreamProvider,
	routes []ModelRoute,
	b UpstreamBundle,
	newID func() string,
) UpstreamMergeResult {
	if newID == nil {
		newID = GenerateMachineId
	}

	res := UpstreamMergeResult{
		SkippedProviders: []SkippedItem{},
		SkippedRoutes:    []SkippedItem{},
	}
	// Copy rather than append onto the caller's arrays: append may write into
	// spare capacity the caller still owns.
	res.Providers = make([]UpstreamProvider, len(providers))
	copy(res.Providers, providers)
	res.Routes = make([]ModelRoute, len(routes))
	copy(res.Routes, routes)

	// Existing state: dedup index and taken IDs. Providers and routes keep
	// SEPARATE id sets — nothing in the codebase compares IDs across the two.
	provByKey := make(map[string]string, len(res.Providers)) // dedup key -> provider ID
	provIDs := make(map[string]bool, len(res.Providers))
	for _, p := range res.Providers {
		provByKey[providerDedupKey(p)] = p.ID
		if p.ID != "" {
			provIDs[p.ID] = true
		}
	}
	routeKeys := make(map[string]bool, len(res.Routes))
	routeIDs := make(map[string]bool, len(res.Routes))
	for _, r := range res.Routes {
		routeKeys[routeDedupKey(r)] = true
		if r.ID != "" {
			routeIDs[r.ID] = true
		}
	}

	// freshID returns want if it is usable, else a newly minted unused ID.
	freshID := func(want string, taken map[string]bool) string {
		want = strings.TrimSpace(want)
		if want != "" && !taken[want] {
			taken[want] = true
			return want
		}
		for {
			id := newID()
			if id != "" && !taken[id] {
				taken[id] = true
				return id
			}
		}
	}

	// idMap: imported provider ID -> ID the routes should actually point at.
	idMap := make(map[string]string, len(b.Providers))

	for _, p := range b.Providers {
		importedID := strings.TrimSpace(p.ID)
		key := providerDedupKey(p)
		if existingID, ok := provByKey[key]; ok {
			// Duplicate: keep ours, point this bundle's routes at it.
			//
			// Note this drops the incoming apiKey/proxyURL too — a key rotated on
			// the source host will NOT overwrite the stale one here. That is what
			// "merge, skip duplicates" means; the UI hint text says so.
			if importedID != "" {
				idMap[importedID] = existingID
			}
			res.ProvidersSkipped++
			res.SkippedProviders = append(res.SkippedProviders, SkippedItem{
				Label:  providerLabel(p),
				Reason: SkipReasonDuplicate,
			})
			continue
		}
		p.ID = freshID(importedID, provIDs)
		if importedID != "" {
			idMap[importedID] = p.ID
		}
		provByKey[key] = p.ID
		res.Providers = append(res.Providers, p)
		res.ProvidersAdded++
	}

	for _, r := range b.Routes {
		key := routeDedupKey(r)
		if routeKeys[key] {
			res.RoutesSkipped++
			res.SkippedRoutes = append(res.SkippedRoutes, SkippedItem{
				Label:  key,
				Reason: SkipReasonDuplicate,
			})
			continue
		}
		// Remap every provider reference through idMap. Targets carry their own
		// provider IDs, so without this a bundle exported from another host lands
		// pointing at the SOURCE host's provider IDs. A target whose provider did
		// not survive the merge is dropped rather than left dangling.
		if len(r.Targets) > 0 {
			kept := make([]RouteTarget, 0, len(r.Targets))
			for _, tg := range r.Targets {
				// The Kiro-pool sentinel is host-independent: it refers to the importing
				// host's own account pool, not to anything that travelled in the bundle.
				// Remapping it through idMap would find no entry and silently drop the
				// pool from the route, turning a configured fallback into a dead end.
				if strings.TrimSpace(tg.UpstreamID) == KiroPoolTargetID {
					kept = append(kept, tg)
					continue
				}
				mapped, ok := idMap[strings.TrimSpace(tg.UpstreamID)]
				if !ok || mapped == "" {
					continue
				}
				tg.UpstreamID = mapped
				kept = append(kept, tg)
			}
			r.Targets = kept
		}
		// The legacy field is remapped too when present, so a v1 build importing
		// this route later still resolves it. It is optional in schema 2, so an
		// unresolvable one is only fatal when there is no surviving target either.
		if legacy := strings.TrimSpace(r.UpstreamID); legacy != "" && legacy != KiroPoolTargetID {
			if mapped, ok := idMap[legacy]; ok && mapped != "" {
				r.UpstreamID = mapped
			} else {
				r.UpstreamID = ""
			}
		}
		// Nothing usable left: every provider this route named was skipped or
		// unknown. Defense in depth — ValidateUpstreamBundle rejects the case where
		// the bundle itself is inconsistent; this catches references that died
		// during the merge.
		if len(r.Targets) == 0 && r.UpstreamID == "" {
			res.RoutesSkipped++
			res.SkippedRoutes = append(res.SkippedRoutes, SkippedItem{
				Label:  key,
				Reason: SkipReasonUnknownProvider,
			})
			continue
		}
		// A v1 bundle (legacy 1:1 schema, no Targets) must get Targets populated,
		// or ResolveRoute — which reads Targets exclusively — never matches it.
		routesToMigrate := []ModelRoute{r}
		migrateModelRoutes(routesToMigrate)
		r = routesToMigrate[0]
		r.ID = freshID(r.ID, routeIDs)
		routeKeys[key] = true
		res.Routes = append(res.Routes, r)
		res.RoutesAdded++
	}

	return res
}

// ImportUpstreamBundle validates and merges b into the live config atomically
// under the config lock, then persists.
//
// The whole read-modify-write happens inside one lock on purpose: doing
// GetUpstreamConfig -> merge -> UpdateUpstreamConfig from the handler would race
// a concurrent apiUpdateUpstreams and silently lose one of the two writes.
//
// Nothing is left changed when validation or persistence fails.
func ImportUpstreamBundle(b UpstreamBundle) (UpstreamMergeResult, error) {
	if err := ValidateUpstreamBundle(&b); err != nil {
		return UpstreamMergeResult{}, err
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return UpstreamMergeResult{}, fmt.Errorf("config not initialized")
	}

	res := MergeUpstreamBundle(cfg.Upstreams, cfg.ModelRoutes, b, nil)
	prevProviders, prevRoutes := cfg.Upstreams, cfg.ModelRoutes
	cfg.Upstreams, cfg.ModelRoutes = res.Providers, res.Routes
	if err := saveLocked(); err != nil {
		// Roll back in memory so the process does not keep serving a state that
		// never reached disk.
		cfg.Upstreams, cfg.ModelRoutes = prevProviders, prevRoutes
		return UpstreamMergeResult{}, err
	}
	return res, nil
}
