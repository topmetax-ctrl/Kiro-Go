# Profile Picker Correctness — Report

## Provenance

- **Source SHA (base):** `5e8be59b7e7cbcb5b25e2ff757b1fdf1d7b2f78c`
  (`chore(docker): opt into insecure public bind for local compose`)
- **Working branch:** `fix/profile-picker-correctness`
- **Parent branch:** `feat/hardening-profile-picker-lru`
- **Working tree at start:** clean.

## Scope

Verify and complete the existing multi-profile picker without changing its UX
flow. Kept **Account Detail → Profiles → Discover → Use**. Did **not** add a
"Switch profile" button on the account card and did **not** add a second
picker/modal. Delivered: current-profile visibility on the account card
(read-only, from the existing `/accounts` payload), a correct model-cache
refresh after a switch (prefetch-before-commit, honest success reporting), and a
verified maxResults/pagination decision.

## maxResults & pagination

- **`maxResults` stays `10`.** The proven-working `listAvailableProfiles`
  (`kiro_api.go`) uses `{"maxResults":10}`; a prior real bug on the parent branch
  confirmed the API returns HTTP 400 `REQUEST_BODY_INVALID` for larger values.
  `listProfilesInRegion` (`profile_discovery.go`) keeps `10`.
- **Pagination is NOT supported by the upstream (no evidence).** A repo-wide
  search for `nextToken` / `nextPageToken` / `continuationToken` found no matches
  in code, tests, or fixtures. The observed response schema exposes only a
  `profiles` array (`{arn, profileName}`). No pagination was invented.
- **Max profiles without pagination:** at most **10 profiles per region** are
  enumerated. Candidate regions are `us-east-1` and `eu-central-1`
  (`kiro_api.go: kiroProfileRegions`), so up to 20 profiles total. The real-world
  US/EU case (1 profile per region) is unaffected.
- If Kiro later documents a pagination token this is a clean follow-up: loop
  `maxResults:10`, thread the verified token, dedupe across pages, cap pages
  (~20) with repeated-token detection and ctx cancellation.

## Verification of the current implementation (before changes)

All 16 assertions were checked against the code at the source SHA. Summary
(full evidence table in `docs/profile-picker-correctness-plan.md`):

- Discovery probes all regions, collects all profiles (not first-only), dedupes
  by ARN, stable-sorts, and returns partial success — **all TRUE**.
- `SelectProfile` re-verified the target ARN and persisted ARN+region together —
  **TRUE**.
- Discovery is admin/setup-only; the hot path reads only the pinned profile from
  the account snapshot — **TRUE**.
- **Bug (confirmed):** after a switch the code only ran `SetModelList(id, nil)`
  (invalidate) yet the handler returned `modelCacheRefreshed: true` and the UI
  announced "model cache refreshed" — a false success, and the global
  `/v1/models` aggregate kept models only the old profile offered.
- `/accounts` did not return the current ProfileArn/region; the account card did
  not show the current profile — **TRUE** (both now added).
- `maxResults=10`; no pagination token anywhere — **confirmed**.

## What the current implementation already got right

- Multi-region discovery, all-profiles collection, ARN dedupe, stable sort,
  partial-region success, ctx cancellation.
- Pre-commit target-ARN re-verification.
- Atomic ARN+region persistence (`UpdateAccountProfileArnWithRegion`).
- Admin-only discovery; hot path untouched.
- Admin lookup falls back to full config so disabled/banned accounts are
  addressable (fixed on the parent branch).

## The old model-cache bug

`SelectProfile` persisted the new ARN, then only invalidated the account's
routing list (`SetModelList(id, nil)`). It never fetched the new profile's models
and never rebuilt the global aggregate, yet reported `modelCacheRefreshed: true`.
Consequences: (1) false success — nothing was refreshed; (2) stale aggregate —
`/v1/models` kept old-profile-only models until the next full `refreshModelsCache`.

## Model-cache behavior after the fix

`SelectProfile` is now **prefetch-before-commit**, serialized per account:

1. validate inputs; resolve account (pool→config admin lookup).
2. `EnsureFresh` token via TokenManager.
3. re-verify the target ARN exists in the region.
4. build a **value-copy candidate** snapshot (`ProfileArn=target`,
   `ApiRegion=region`).
5. fetch the candidate profile's models via `ListAvailableModels(candidate)` —
   **outside any cache lock**. On failure: do NOT persist; old profile stays
   intact; return the error.
6. persist ARN+region atomically.
7. `pool.Reload()` to publish the new snapshot.
8. replace ONLY this account's routing model list with the candidate models.
9. rebuild the global aggregate **in memory** from per-account model metadata so
   models no remaining account offers drop out while other accounts' models stay
   — no network I/O under the lock.

`modelCacheRefreshed:true` and `modelCount` are returned **only** when the fetch
and publish actually succeeded. The response is
`{success, pinned, modelCacheRefreshed, modelCount, warnings}`.

### Aggregate rebuild without extra network I/O

`mergeUniqueModels` only ever grows the aggregate, so dropping stale models
requires model metadata, not just IDs. A per-account `ModelInfo` cache
(`Handler.modelInfoByAccount`, guarded by `modelsCacheMu`) is populated wherever
the ID set is written (`refreshModelsCache`, `fetchAndCacheAccountModels`,
`SelectProfile`). `rebuildAggregateLocked` unions those cached lists — pure
in-memory, no network, no touching unrelated accounts. (The cache lives on
`Handler`, not the pool, because `ModelInfo` is defined in package `proxy` which
already imports `pool`; putting it in `pool` would create an import cycle.)

## Current-profile fields added to the account API

`apiGetAccounts` now returns four additive, read-only, backward-compatible
fields sourced from the existing config snapshot — no network, no refresh, no
persist, no secrets:

- `currentProfileArn`    = `a.ProfileArn`
- `currentProfileRegion` = `a.EffectiveApiRegion()`
- `currentProfileLabel`  = `shortARN(a.ProfileArn)` (no persisted profile-name
  field exists; no migration added just for a label)
- `hasPinnedProfile`     = `a.ProfileArn != ""`

## UI

- **Location unchanged:** the picker stays in Account Detail → Profiles →
  Discover → Use. **No "Switch profile" button on the account card. No second
  picker/modal.** Confirmed in a live browser.
- **Account card** shows a dedicated profile line, distinct from the auth-method
  badge: `Profile: <region> · <label>`. Region is shown (not a secret); the
  identifier is masked under privacy mode (`ACPY***`) and shown in full when
  privacy is off (`ACPYXKUPYE3H`). Long labels use ellipsis; the full value is in
  the element `title`. Accounts with no pinned profile show a localized
  "Profile not resolved". Data comes from the single `/accounts` response — no
  per-card API call, no N+1, no discovery on render.
- **i18n:** new keys added to both `en.json` and `zh.json` (639→ matched counts);
  previously hardcoded English picker strings were also localized.
- After a successful switch the UI calls `loadAccounts()` so the card updates
  immediately (no full-page reload). The success toast now reflects the real
  outcome (`{n} models cached`), not a fixed "model cache refreshed".

## Hot-path / concurrency guarantees

- No discovery/usage/profile network call in the chat handler or account
  selection; the card uses only the existing `/accounts` payload.
- Cache locks are never held across Kiro network I/O.
- Per-account switch mutex (`profileSwitchLocks`, keyed by account ID); switches
  of different accounts run in parallel — no global lock on the hot path.
- In-flight requests hold an immutable account snapshot → keep the old profile;
  requests after publish see the new one. Prefetch-before-commit means there is
  no "new ARN + old region" and no "new profile + stale model list" window.

## Tests

New/updated (all in package `proxy`):

- `profile_discovery_test.go`
  - `TestSelectProfilePersistsAndReplacesModelCache` — ARN+region persisted;
    candidate carries target ARN/region; routing list replaced (old model gone,
    new models present); `modelCacheRefreshed`+`modelCount` reported.
  - `TestSelectProfileRejectsVanishedProfile` — absent target → error, account
    unchanged, model prefetch never runs.
  - `TestSelectProfileKeepsOldProfileWhenModelFetchFails` — prefetch failure →
    switch refused, old ARN/region and old model list intact.
  - discovery regression tests retained (single region, multi-region dedupe+sort,
    partial success, all-regions-fail, ctx-cancel, concurrent).
- `profile_cutover_test.go`
  - `TestSelectProfileRebuildsAggregateDroppingStaleModels` — aggregate drops
    old-only model, keeps a model still offered by another account.
  - `TestSelectProfileSameAccountSerialized` — two concurrent same-account
    switches end in one consistent state (ARN⇔region⇔model list agree).
  - `TestSelectProfileDifferentAccountsParallel` — two different-account switches
    both complete.
  - `TestApiGetAccountsExposesCurrentProfile` — `/accounts` returns the four
    profile fields with correct values; no secret leaks; no-profile account
    reports `hasPinnedProfile:false`.
- `profile_admin_test.go` — updated to the new `SelectProfile` signature and to
  stub the model lister.

## Test results

- `gofmt`: clean (changed files).
- `go vet ./...`: clean.
- `go test ./...`: all packages pass.
- `go test -race ./...`: clean across `pool`, `proxy`, `config` (aggregate
  rebuild, per-account serialization, parallel different-account switches all
  covered).

## Benchmarks

`docs/benchmarks-before-profile-correctness.txt` vs
`docs/benchmarks-after-profile-correctness.txt`:

- `BenchmarkRoute`: ~4.8µs → ~4.4µs (within noise), 33 allocs / 2848 B/op
  unchanged.
- `BenchmarkGetNextExcluding` / `GetByID` / parallel selection: ~300–460 ns,
  704 B / 1 alloc — unchanged (the alloc is the pre-existing race-fix snapshot).
- Prompt-cache Update/Compute: 0 allocs/op retained; timings unchanged.
- **No hot-path regression.** The new work (per-account model metadata, switch
  lock, aggregate rebuild) lives entirely on the admin/setup path.

## Manual / integration validation (live Docker)

- Rebuilt the image, restarted the container.
- `/accounts` returns the four profile fields; no `accessToken`/`refreshToken`
  in the payload.
- Discovery for a real account returns both US and EU profiles.
- **Failed switch** (US profile → HTTP 403 not authorized) is refused at the
  model-prefetch step; the old EU profile stays pinned — honest failure, no
  partial state.
- **Valid switch** (EU) returns `modelCacheRefreshed:true`, `modelCount:13`.
- Account card renders `Profile: <region> · <label>` for enabled AND
  disabled/banned accounts; privacy-on masks the label, privacy-off shows it in
  full (verified via Playwright).
- `/v1/models` returns the expected aggregate (33 entries incl. thinking variants
  and aliases).
- Direct Claude request smoke test returns a real completion — proxy flow intact.

## Known limitations

- **No pagination:** at most 10 profiles/region are enumerated (no upstream
  pagination token exists). US/EU real-world case unaffected.
- **Aggregate rebuild scope:** the rebuild unions per-account model metadata that
  has been observed at least once (via a models refresh or a prior switch). An
  account whose models were never fetched contributes nothing until its own next
  refresh — the switch guarantees correctness for the account being switched and
  never invents models for others.
- **Profile label:** `shortARN` (last ARN segment); no persisted human profile
  name field exists and none was added (no migration for a cosmetic label).
- **403 on switch** is upstream profile-level authorization, surfaced verbatim
  from the model prefetch; the picker cannot pre-know per-profile entitlement
  without attempting the call.

## Out of scope (not done, per task)

Switch-on-card button; a second picker/modal; per-profile usage preview; auto
usage API per profile; prompt-cache persistence; handler.go refactor; DB/shared
store; proxy request schema change.
