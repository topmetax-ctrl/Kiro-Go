# Implementation Report — Hardening + Multi-Profile Picker + Bounded LRU

## Overview

- **Source SHA (base):** `9bdc617943cf924ba4529b0e9e19573183e2a837`
- **New branch:** `feat/hardening-profile-picker-lru`
- **Scope:** the correctness/security/reliability/perf items agreed in Audit Rev 2,
  plus a multi-profile picker and a bounded O(1) prompt-cache LRU with metrics.
- **Result:** `go vet` clean, `go test ./...` green, `go test -race ./...` clean,
  `git diff --check` clean, gofmt clean. No proxy API schema changes beyond the
  documented security scoping and additive admin/stats fields.

## Commits (in order)

```
4bd9221 test: restore green translator baseline
1224c69 test: add hot-path benchmark harness and capture baseline
4d486f1 docs: add implementation plan for hardening + profile picker + LRU
63ec2da security: reject insecure public defaults
028f34b fix: return immutable account snapshots
bfc2278 feat: centralize token refresh coordination
3864c21 fix: make config persistence atomic
9756a7e fix: persist account stats synchronously
3bc4f8d security: scope stored responses by principal
9a0c8b5 security: validate idp endpoints at outbound boundary
93e6452 security: enforce inbound body limits
0f0c89c fix: add streaming-safe transport timeouts and forward client
c6c2cfd fix: propagate cancellation and account usage in upstream forwarding
917341b perf: replace prompt tracker map with bounded LRU
1af99e9 feat: expose prompt-cache metrics
a1a87a2 feat: add multi-profile discovery and picker
add3242 reliability: add graceful shutdown
```

## What changed, by area

### Green baseline (translator)
Two tests added in PR #106 were merged **red** — they asserted that orphaned
tool-results stay attached structurally, contradicting PR #104's deliberate
flatten-to-text design. Root-caused, found a real data-loss bug (orphaned mixed
text+image tool-result dropped its text), fixed the code, and corrected the two
stale assertions to the shipped contract. Files: `proxy/translator.go`,
`proxy/translator_test.go`.

### Phase 0 — operational safety
- Default bind changed from `0.0.0.0` to `127.0.0.1` (fresh installs only; existing
  configs untouched).
- Startup safety gate: a non-loopback bind + default password or disabled auth
  hard-fails, unless `ALLOW_INSECURE_PUBLIC_BIND=true` (downgrades to a loud
  warning). Pure, table-tested decision function; no secrets logged.
- CI workflow running vet/test/race on push and PR.
- Files: `config/startup_safety.go`, `config/config.go`, `main.go`, `.github/workflows/ci.yml`.

### Phase 1 — account state, tokens, config, responses, endpoints, ingress
- **Immutable account snapshots** (`pool/account.go`): getters return an independent
  `config.Account` value snapshot instead of a pointer into the live slice, removing
  a torn-read race under concurrent refresh. +1 small alloc per selection.
- **Central TokenManager** (`proxy/token_manager.go`): all refresh paths coalesce per
  account via a self-contained singleflight (no new dependency); different accounts
  refresh in parallel (replacing the global tokenRefreshMu). Owns the
  memory↔persistence transaction: re-read latest snapshot → validate IdP response →
  bump generation → persist → publish; on persist failure keep the rotated token in
  memory + publish it, mark PersistenceDegraded, retry with bounded backoff. State
  machine: Healthy/Refreshing/PersistenceDegraded/ReauthRequired/Disabled.
- **Atomic config save** (`config/config.go`): temp-file + fsync + rename + `.bak`
  last-known-good, with validate-before-replace and a corrupt-primary → backup
  recovery on load.
- **Synchronous account-stats persistence** (`pool/account.go`): removed a
  fire-and-forget `go config.UpdateAccountStats` goroutine with no lifecycle
  management (it could outlive shutdown and raced saves).
- **Responses ownership** (`proxy/responses_store.go`, `_handler.go`, `_history.go`):
  stored responses carry `OwnerPrincipalID` (stable ApiKeyEntry.ID; rotation-in-place
  keeps access). `loadResponseForOwner` enforces ownership and returns a single
  generic not-found for both missing and cross-owner IDs; the ancestor-chain walk
  applies the check at every hop. Legacy owner-empty docs are denied under auth and
  accessible only in single-user (auth-off) scope.
- **Endpoint validation at the outbound boundary** (`auth/kiro_sso.go`): the existing
  HTTPS + no-IP + Microsoft allow-list check is now re-applied on every refresh
  (cached/persisted endpoint no longer trusted) and inside OIDC discovery (issuer and
  discovered endpoints). Refresh client already refuses redirects.
- **Ingress body limits** (`proxy/handler.go`): `http.MaxBytesReader` applied centrally
  in ServeHTTP by endpoint class (32 MiB inference, 4 MiB admin).

### Phase 2 — upstream forwarding correctness
- Streaming-safe transport (`proxy/kiro.go`): added Dial/TLS-handshake/response-header/
  expect-continue timeouts to `buildKiroTransport`; new `GetForwardClientForProxy`
  with **no** blanket client timeout so long SSE streams are not truncated.
- Cancellation: `http.NewRequestWithContext(r.Context())` so client disconnect cancels
  the upstream call/stream; a cancelled client is no longer a 502.
- Usage accounting: forwarded success now charges the key (upstream-reported tokens if
  present, else one request unit) instead of the previous `recordSuccess(0,0,0)`. The
  429 over-limit gate (in `authenticate`, before the handler) is unchanged.
- Header hygiene: request/response allow-lists; client Authorization/X-Api-Key never
  forwarded upstream. Error redaction: generic client message, detailed log only.
- Metrics recorded at end of relay (full latency, not just time-to-headers).

### Performance — bounded O(1) LRU + metrics
- `proxy/cache_tracker.go`: replaced the per-account nested map + O(total-entries)
  prune with a single global bounded LRU (`map[promptCacheKey]*list.Element` +
  `container/list`), keyed by `{AccountID, Fingerprint}`. O(1) lookup/insert/update/
  evict; lazy expiry; global capacity bound (default 50k) regardless of account count;
  per-account isolation preserved. All prompt-cache semantics unchanged.
- Metrics (hits/misses/creations/evictions/expiredEvicted/currentEntries/capacity)
  exposed additively on `/v1/stats` under a `promptCache` object (no secrets; existing
  fields unchanged).

### Multi-profile picker
- `proxy/profile_discovery.go`: `DiscoverProfiles` (all regions, dedupe, stable sort,
  partial success, ctx-aware, no side token refresh), `GetPinnedProfile`,
  `SelectProfile` (validate → verify ARN at region → atomic persist → pool reload →
  invalidate only that account's model cache). Admin API: GET `/accounts/{id}/profiles`,
  GET/POST `/accounts/{id}/profile`. Admin UI in `web/app.js` (Discover, per-profile
  Use with confirm, pinned marker, partial-region warnings).

### Graceful shutdown
- `main.go` serves in a goroutine and waits on `signal.NotifyContext`; on signal it
  calls `Handler.Shutdown` (flush degraded persists, stop workers, final stats save)
  then `srv.Shutdown` with a 30s drain deadline (falls back to `srv.Close`).

## Behavior changes (intentional)

- Cross-owner reads of stored responses now return 404 (was: any key could read any
  stored response via `previous_response_id`).
- A fresh install binds to loopback; a public bind with weak posture refuses to start.
- Forwarded requests now advance the per-key usage counter.
- No SSE ordering, translator semantics, model rewrite, route matching, fingerprint,
  or public usage-response changes.

## Migration & rollback

- **Migration:** none required. New fields (`OwnerPrincipalID`/`CreatedByKeyID`,
  profile ARN/region) are additive; old configs load unchanged. `config.json.bak` and
  `.tmp-*` are additive files.
- **Rollback:** each concern is an isolated commit; revert individually. The account
  getters keep the `*config.Account` signature, so the snapshot change is
  caller-transparent.

## Test results

- `go vet ./...`: clean.
- `go test ./...`: all packages pass.
- `go test -race ./...`: clean (pool snapshot races, TokenManager coalescing, LRU
  concurrency, forwarding, shutdown all covered).
- Baseline had 2 red translator tests (now fixed); no other pre-existing failures.

## Benchmarks (before/after)

See `docs/benchmarks-before-hardening.txt`, `docs/benchmarks-after-hardening.txt`,
and `docs/performance-comparison-hardening.md`. Headline: prompt-cache `Compute` at 64
accounts went from ~17µs (scaling linearly with total entries) to ~290ns (flat), with
0 allocs/op retained; account selection added exactly one ~704B alloc per call for the
race-fix snapshot.

## Ported concepts (re-implemented, not cherry-picked)

- Multi-region profile discovery/picker — concept from `zsecducna/Kiro-Go`
  (`5095a2a0`, `4afdb79c`); re-written against this codebase's pool/config/TokenManager.
- Bounded O(1) LRU + cache metrics — concept from `ngh1105/Kiro-Go` (`36a07999`,
  `216c65b2`); re-written to keep per-account isolation and this project's exact
  cache-usage semantics.
- **Not ported:** prompt-cache disk persistence (out of scope; the tracker stores no
  model computation, so persistence would not save quota — not advertised as such).

## Confirmations

- **Proxy hot path does not run profile discovery.** The request path reads only the
  pinned profile from the account snapshot; `DiscoverProfiles`/`SelectProfile` are
  admin/setup-only.
- **Prompt-cache semantics unchanged.** Fingerprint, prefix/breakpoint selection, token
  counting, TTL, first-request-vs-85%-cap, hit-refreshes-expiry, usage fields, response
  format, per-account isolation all preserved; verified by the pre-existing cache tests
  plus new LRU tests.
- **Forwarding usage accounting behavior.** Within-limit key → forwards and the per-key
  counter advances (upstream tokens when reported, else 1 request unit); over-limit key
  → 429 before the handler (unchanged). No fabricated pricing.

## Known limitations / open product decisions

- Forwarded-usage cost policy defaults to request-count (multiplier 1) when the upstream
  omits usage headers; a token/credit cost model is a product decision.
- The multi-profile picker UI was wired and JS-syntax-checked but not exercised in a
  live browser in this environment.
- LRU global capacity (50k) is a compile-time default; making it configurable is a
  follow-up if operators need it.

## Appendix

```
git log --oneline --decorate 9bdc617..HEAD    # 17 commits (listed above)
git diff --stat 9bdc617..HEAD                 # 39 files, +4276 / -203
```
