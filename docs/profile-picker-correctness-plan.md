# Profile Picker Correctness — Plan & Verification

- **Source SHA (base):** `5e8be59b7e7cbcb5b25e2ff757b1fdf1d7b2f78c`
  (`chore(docker): opt into insecure public bind for local compose`)
- **Working branch:** `fix/profile-picker-correctness`
- **Parent branch:** `feat/hardening-profile-picker-lru`
- **Working tree at start:** clean.

## Scope (what this task does / does not do)

Verify and complete the existing multi-profile picker. Keep the current UX flow
(**Account Detail → Profiles → Discover → Use**) — do NOT add a "Switch profile"
button on the account card. Only:

1. Surface the *current* pinned profile on the account card (read-only, from the
   existing `/accounts` payload — no per-card API call, no discovery on render).
2. Make model-cache refresh after a switch *correct* (prefetch the new
   profile's models before commit; rebuild the aggregate; only report
   `modelCacheRefreshed: true` when a real fetch+publish succeeded).
3. Confirm `maxResults` / pagination behavior (keep `10`; document limit).

Explicitly out of scope: switch-on-card button, a second picker/modal,
per-profile usage preview, auto usage API per profile, prompt-cache
persistence, handler.go refactor, DB/shared store, proxy request schema change.

## Verification of current implementation (code = source of truth)

Each assertion below was checked against the code at the source SHA.

| # | Assertion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | DiscoverProfiles probes all candidate regions | TRUE | `profile_discovery.go:54` loops `profileProbeRegions(account)` |
| 2 | Discovery does not stop at first profile | TRUE | `listProfilesInRegion` collects all (`:136-147`); `DiscoverProfiles` appends all (`:64-74`) |
| 3 | Deduplicate by ARN | TRUE | `seen` map, `:64-69` |
| 4 | Stable-sort results | TRUE | `sort.SliceStable` region→ARN, `:79-84` |
| 5 | Partial-region failure still returns profiles | TRUE | `:59-62` record error + continue; error only when zero profiles `:86-91` |
| 6 | SelectProfile re-verifies target before persist | TRUE | `:229-242` lists region, checks ARN present |
| 7 | ARN + region persisted together | TRUE | `config.UpdateAccountProfileArnWithRegion` `:245` |
| 8 | Discovery only on admin/setup path | TRUE | only `apiDiscoverAccountProfiles` / `apiSelectAccountProfile` call it; hot path never does |
| 9 | Hot path reads only pinned profile from snapshot | TRUE | `withProfileArnQuery`/`regionalizeURL` read `account.ProfileArn`/`ApiRegion` from the pool snapshot |
| 10 | After switch only `SetModelList(id, nil)` | TRUE | `profile_discovery.go:254` (invalidate only) |
| 11 | `modelCacheRefreshed:true` reported without loading new list | TRUE (BUG) | `apiSelectAccountProfile` returns `"modelCacheRefreshed": true` (handler.go); UI toasts "model cache refreshed" (`app.js:1358`) yet no fetch happens |
| 12 | `/admin/api/accounts` does not return ProfileArn/ApiRegion | TRUE | `apiGetAccounts` map (`handler.go:2636-2677`) has no profile fields |
| 13 | Account card does not show current profile | TRUE | `renderAccounts` (`app.js:973-1050`) renders no profile line |
| 14 | Picker lives in Account Detail | TRUE | detail modal section (`app.js:1294-1299`) |
| 15 | `listProfilesInRegion` uses `maxResults=10` | TRUE | `profile_discovery.go:105` |
| 16 | Response parser has nextToken/pagination | FALSE | schema is `{"profiles":[{arn,profileName}]}`; no pagination token in code, tests, or fixtures anywhere in repo |

## maxResults & pagination decision

- The proven-working `listAvailableProfiles` (`kiro_api.go:451`) uses
  `{"maxResults":10}`. A prior real bug (fixed on the parent branch) confirmed
  the API returns HTTP 400 `REQUEST_BODY_INVALID` for larger values.
- Repo-wide search for `nextToken` / `nextPageToken` / `continuationToken`
  found **no matches** in `.go`, tests, or fixtures. The documented response
  schema exposes only a `profiles` array.
- **Decision:** keep `{"maxResults":10}`. Do **not** invent pagination. Record a
  known limitation: at most 10 profiles per region are enumerated. The real-world
  US/EU two-profile case is unaffected (1 profile per region).
- If Kiro later documents a pagination token, this is a clean follow-up: loop
  `maxResults:10` requests, thread the verified token, dedupe across pages, cap
  pages (~20) with repeated-token detection and ctx cancellation.

## Model-cache architecture (audited)

- **Per-account routing cache:** `pool.modelLists map[accountID]set` — used by
  `accountHasModel` / `GetNextForModel*` for routing. Guarded by `pool.mu`.
- **Global aggregate:** `Handler.cachedModels []ModelInfo` — served by
  `/v1/models` (`handleModels`). Guarded by `Handler.modelsCacheMu`.
- `fetchAndCacheAccountModels` sets the per-account list, then **merges** into
  the global aggregate (`mergeUniqueModels`) — it never *removes* models that a
  now-departed profile used to provide.
- `ListAvailableModels(account)` builds its URL from `account.ProfileArn` +
  `regionalizeURL(account)`, so a **value-copy candidate** with the target
  ARN/region can prefetch the new profile's models before we commit.
- Neither cache lock is held across the network call today, and we will keep it
  that way (fetch outside locks).

### The bug

`SelectProfile` persists the new ARN then only calls `SetModelList(id, nil)`
(invalidate). It never fetches the new profile's model list and never rebuilds
the global aggregate. Yet the handler returns `modelCacheRefreshed: true` and the
UI announces "model cache refreshed". Two problems:

1. **False success**: nothing was refreshed.
2. **Stale aggregate**: `/v1/models` keeps models that only the *old* profile
   offered until the next full `refreshModelsCache`.

## Planned design (per task spec §6)

`SelectProfile` becomes prefetch-before-commit:

1. validate inputs; resolve account (pool→config admin lookup).
2. `EnsureFresh` token via TokenManager.
3. verify target ARN still present in the region.
4. build a **value-copy candidate** snapshot with `ProfileArn=target`,
   `ApiRegion=region`.
5. `ListAvailableModels(candidate)` — **outside any cache lock**.
   - on failure: do NOT persist; leave the old profile intact; return error.
6. persist ARN+region atomically (`UpdateAccountProfileArnWithRegion`).
7. `pool.Reload()` to publish the new snapshot.
8. `pool.SetModelList(id, newModelIDs)` — replace this account's routing list.
9. rebuild the global aggregate from all enabled accounts' per-account lists so
   models no account supports anymore drop out, while other accounts' models
   stay. (A dedicated rebuild that does not do network I/O — it unions the
   already-cached per-account lists.)
10. return `{success, pinned, modelCacheRefreshed:true, modelCount, warnings}`
    only when 5–9 succeeded.

Per-account serialization: a per-account mutex keyed by account ID so two
concurrent switches of the *same* account cannot interleave persist/publish,
while switches of *different* accounts still run in parallel (no global lock).

### Aggregate rebuild without network I/O

`mergeUniqueModels` only ever grows the aggregate. To *drop* stale models we need
model metadata (`ModelInfo`), not just IDs. Options considered:

- (A) Re-fetch every enabled account's models on each switch — extra network I/O,
  slower, and touches unrelated accounts. Rejected.
- (B) Keep a per-account `[]ModelInfo` cache so the aggregate can be rebuilt by
  unioning cached metadata (no network). This is the chosen approach: store the
  candidate's `[]ModelInfo` for the switched account, union with other accounts'
  last-known `[]ModelInfo`. Accounts without cached metadata are simply skipped
  in the rebuild (their models reappear on their own next refresh) — the switch
  itself only guarantees correctness for the account being switched and never
  *invents* models.

We add a small per-account `ModelInfo` cache in the pool
(`modelInfoLists map[accountID][]ModelInfo`) alongside the existing ID set,
written wherever the ID set is written, so the aggregate rebuild is pure
in-memory. This keeps `/v1/models` free of stale old-profile models after a
switch without re-fetching unrelated accounts.

## Account-card current-profile fields (task spec §5)

Extend `apiGetAccounts` (read-only, additive, backward-compatible) from the
existing snapshot — no network, no refresh, no persist, no secrets:

- `currentProfileArn`   = `a.ProfileArn`
- `currentProfileRegion`= `a.EffectiveApiRegion()`
- `currentProfileLabel` = `shortARN(a.ProfileArn)` (no persisted profile name
  field exists; do not add a migration just for a label)
- `hasPinnedProfile`    = `a.ProfileArn != ""`

Card UI: a dedicated line/badge, distinct from the auth-region badge, e.g.
`Profile: eu-central-1 · <shortARN>`. Privacy mode masks the identifier
(region stays visible — not a secret). i18n key `accounts.profile` /
`accounts.profileNone`. After a successful switch, `loadAccounts()` refreshes
the card. No N+1 (data already in the single `/accounts` response).

## Hot-path / concurrency guarantees

- No discovery/usage/profile network call in the chat handler or account
  selection. Card data comes from the existing `/accounts` payload only.
- Cache locks are never held across Kiro network I/O.
- Per-account switch mutex; different accounts switch in parallel.
- In-flight requests hold an immutable account snapshot → keep old profile;
  requests after publish see the new one. No "new ARN + old region" and no
  "new profile + old model list" window (persist+publish+model replace happen
  after a successful prefetch).

## Tests planned

- discovery: request always sends `maxResults:10`; two profiles in one region
  both returned; two regions each yielding a profile; (pagination tests only if
  a token is ever verified — not now).
- `/accounts`: returns ARN + active region + label + hasPinnedProfile; no
  token/secret; account without profile → `hasPinnedProfile:false`, no error.
- model-cache: candidate fetch fails → old profile kept, no `modelCacheRefreshed`;
  candidate fetch succeeds → that account's routing list replaced; global
  aggregate drops old-only models, keeps other accounts' models; `modelCount`
  reported.
- concurrency: two same-account switches deterministic (serialized); two
  different-account switches not serialized; `go test -race` clean.
- UI (manual/integration): card shows profile; privacy masks label; switch US↔EU
  updates card; `/v1/models` no longer holds old-only models.

## Commit plan

1. test: add profile picker correctness regressions
2. (pagination commit only if verified — skipped: no pagination)
3. feat: expose current profile in account summaries
4. feat: show active profile on account cards
5. fix: refresh profile-scoped model cache after switch
6. test: cover profile cutover and concurrency
7. docs: document profile picker invariants
