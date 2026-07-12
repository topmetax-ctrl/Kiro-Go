# Implementation Plan — Hardening + Multi-Profile Picker + Bounded LRU

- **Source SHA (branch base):** `9bdc617943cf924ba4529b0e9e19573183e2a837`
- **Branch:** `feat/hardening-profile-picker-lru`
- **Baseline commits already on branch:**
  - `4bd9221 test: restore green translator baseline`
  - `1224c69 test: add hot-path benchmark harness and capture baseline`
- **Audit inputs:** `docs/architecture-and-security-audit-2026-07-12.md` (rev1),
  `docs/architecture-and-security-audit-2026-07-12-rev2.md` (rev2, authoritative).
- **Rule:** where audit and HEAD code disagree, **HEAD code wins**; deltas are noted per item.

This plan is grounded in code re-read at HEAD, not only the audit. Each item lists
verified current behavior, files, call path, shared state, locks, API invariants to
preserve, existing/new tests, design, migration, rollback, and risks.

---

## Baseline verification (done)

- `go version` = go1.26.5. `go vet ./...` clean.
- The two translator tests (`TestClaudeToolResultMixedTextAndImage`,
  `TestOpenAIToolResultImageCarriedWhenFollowedByUser`) were merged **red** in PR
  #106; root-caused as assertions contradicting the #104 flatten design, plus one
  genuine text-drop bug. Fixed in `4bd9221`. Full suite + `-race` now green.
- No pre-existing benchmarks. Added harness in `1224c69`; baseline in
  `docs/benchmarks-before-hardening.txt`. Confirmed the prompt-cache O(n) prune:
  `Compute` scales ~330ns (1 acct) → ~18µs (64 accts).

---

## Item P0.1 — Reject insecure public defaults

**Verified current behavior (HEAD):**
- `config/config.go:564-567` defaults: `Password:"changeme"`, `Host:"0.0.0.0"`,
  `RequireApiKey:false`.
- `RequireApiKey` is marked deprecated (`config.go:247`): with multi-key support,
  `len(ApiKeys)>0` implicitly enforces auth. Auth gate lives in `proxy/auth.go`.
- `EvaluateSecurityWarnings` (config) only logs; no hard-fail.
- Startup: `main.go:80` builds `http.Server`, `main.go:98/102` `ListenAndServeTLS` /
  `ListenAndServe`.

**Files:** `main.go`, `config/config.go` (read-only helpers), new `config/security.go`
or a function in `main.go`.

**Design:**
- Change default `Host` to `127.0.0.1` (only affects a *fresh* config; existing
  configs keep their value — no migration of a saved safe/again-unsafe host).
- Add a startup gate `checkStartupSafety(cfg)` invoked in `main.go` before Listen:
  - Determine `nonLoopback := host is not 127.0.0.1 / ::1 / localhost` (also treat
    `0.0.0.0` / `::` as non-loopback).
  - If `nonLoopback` and password == `changeme` → hard-fail (return error, exit 1).
  - If `nonLoopback` and auth disabled (`len(enabledApiKeys)==0`) → hard-fail.
  - Override env `ALLOW_INSECURE_PUBLIC_BIND=true` downgrades hard-fail to a loud
    warning. Never on by default. Never logs secrets.

**API/behavior to preserve:** existing safe deployments (loopback, or public with a
real password + keys) start unchanged. Only the fresh-default + still-`changeme` +
public combination is newly blocked.

**Tests (write first):** table test over {host, password, keysEnabled, envOverride}
→ expect error / nil. No secret in output.

**Migration:** none (defaults only apply on first init). **Rollback:** revert commit;
env override provides an escape hatch meanwhile.

**Risk:** could block a deliberately-public dev instance — mitigated by the env
override and a precise error message. Low perf impact (startup only).

**Commit:** `security: reject insecure public defaults`.

---

## Item P0.2 — CI safety gates

**Design:** add `.github/workflows/ci.yml` (or extend if present) running
`go vet ./...`, `go test ./...`, `go test -race ./...`. Only commit tool config if it
does not spawn wide out-of-scope churn. staticcheck/golangci-lint optional and only if
clean. **Commit:** folded into `security: reject insecure public defaults` or a small
`ci:` commit.

---

## Item P1.1 — Immutable account snapshots (pool pointer leak)

**Verified current behavior (HEAD):** `pool/account.go`
- `GetNextExcluding` / `GetNextForModelExcluding` / `GetByID` take `p.mu.RLock()`,
  `defer RUnlock()`, then `return &p.accounts[idx]` — a pointer into the live backing
  slice that escapes after the lock releases.
- Writers under `p.mu.Lock()`: `UpdateToken` (385), `UpdateStats` (435),
  `Reload` (49, replaces `p.accounts`), `RecordSuccess/RecordError`.
- `config.Account` is all value-typed (no nested maps/slices/pointers) → a value copy
  is a complete, safe, independent snapshot.

**Call path:** `proxy/handler.go` selection → `pool.GetNext*` → reads `acc.AccessToken`,
`acc.ProfileArn`, etc., possibly concurrently with a background refresh's `UpdateToken`.

**Shared state / lock:** `p.accounts` slice under `p.mu`.

**Design:**
- Change the three getters to **return `config.Account` by value** (not `*config.Account`).
  Value copy happens under the read lock → callers get an immutable, race-free snapshot.
- Update callers in `proxy/` to use the value (they mostly read fields). Where a caller
  currently checks `acc == nil`, switch to an `ok bool` second return (`(config.Account, bool)`)
  to preserve "no account available" semantics without a nil pointer.

**API to preserve:** account selection result identity (same account chosen), retry
exclusion behavior, model-filtered selection.

**Existing tests:** `pool/account_test.go` covers selection/exclusion; will adapt to the
value-return signature.

**Tests (write first):** `-race` concurrency tests: getter vs `UpdateToken`, vs
`UpdateStats`, vs `Reload`, and disable-during-selection. Assert no race + snapshot
internal consistency (token/expiry belong together).

**Benchmark:** compare `GetNext*` before/after — a 64-account `config.Account` value
copy must not materially regress; baseline shows 0 allocs / ~600ns.

**Migration:** none. **Rollback:** revert the signature commit (callers pinned in same commit).

**Risk:** signature change ripples to callers — contained, compiler-enforced. Value copy
cost negligible vs request cost.

**Commit:** `fix: return immutable account snapshots`.

**Audit delta:** rev2 rated this HIGH pending a reproducible consequence. The consequence
is a torn read (token from gen N, expiry from gen N+1) during concurrent refresh — the
snapshot fix removes it structurally regardless of whether the race detector caught it.

---

## Item P1.2 — Central TokenManager (refresh coordination)

**Verified current behavior (HEAD):** `proxy/handler.go`
- `ensureValidToken` (~2280) holds a **global** `h.tokenRefreshMu`, double-checks via
  `pool.GetByID`, then calls `auth.RefreshToken`. One global lock ⇒ refreshing account A
  blocks a refresh of unrelated account B.
- `refreshAllAccounts` (~275) calls `auth.RefreshToken` with **no** lock and discards the
  `UpdateAccountToken` error.
- No generation/version on token state. `UpdateToken` (pool) + `config.UpdateAccountToken`
  (persist) are two separate steps; persistence is non-atomic (`os.WriteFile`).

**Shared state:** `pool` account token fields; `config` on-disk file; `h.tokenRefreshMu`.

**Design:**
- Introduce a concrete `TokenManager` (new file `proxy/token_manager.go`) owning a
  `singleflight.Group` keyed by **stable account ID**. All refresh paths (foreground
  `ensureValidToken`, background `refreshAllAccounts`, admin refresh-one/all, model
  discovery, import-with-refresh) funnel through `tm.EnsureFresh(ctx, accountID)`.
- Callback: (1) re-read latest snapshot from pool; (2) if token still fresh (>skew), return
  it; (3) call IdP with `ctx` + timeout; (4) validate response (status, JSON, non-empty
  `access_token`, `expires_in>0`, new refresh token if provided); (5) bump generation;
  (6) persist atomically (see P1.3); (7) publish to pool only after persist succeeds;
  (8) return new state to all waiters.
- Replace the global `tokenRefreshMu` with the per-account singleflight (no global lock on
  the hot path).
- **Persistence-failure state machine:** `Healthy → Refreshing → PersistenceDegraded →
  ReauthRequired → Disabled`. On IdP success + persist fail: keep new token in memory
  (do not drop the just-rotated credential), mark `PersistenceDegraded`, retry persist with
  bounded backoff, emit metric; do not overwrite good on-disk data with a corrupt write; try
  to flush on shutdown.

**API to preserve:** the auth/retry policy the handler already implements (a refreshed
token is used for the in-flight request); no change to which account serves a request.

**Existing tests:** `auth/*` covers `RefreshToken` parsing; no coordination test today.

**Tests (write first):** 100 concurrent `EnsureFresh` on one account → exactly one IdP call
(fake IdP counter); N accounts concurrent → parallel (not serialized); stale generation does
not clobber newer; persist-fail → `PersistenceDegraded`, error surfaced not swallowed; empty
token response rejected; background + foreground share coordination. `-race`.

**Migration:** none (in-memory coordination + same on-disk schema). **Rollback:** revert the
TokenManager commit; `ensureValidToken` global-lock path restored.

**Risk:** refresh is not on the pure hot path (only when token near-expiry), so per-account
singleflight adds no steady-state contention and removes cross-account blocking. Correctness
risk mitigated by the concurrency tests above.

**Commit:** `feat: centralize token refresh coordination`.

---

## Item P1.3 — Atomic/durable config save

**Verified current behavior (HEAD):** `config/config.go`
- `Save()` (640) = `json.MarshalIndent` then `os.WriteFile(cfgPath, data, 0600)` — a
  truncating, non-atomic write. A crash mid-write can leave a truncated/corrupt config.
- `saveLocked()` (629) is the lock-held internal; ~35 call sites all hold `cfgLock`.

**Design:** replace the write with tmp-file + fsync + rename:
1. create `cfgPath + ".tmp-<pid>"` in the same dir; 2. marshal + validate (round-trip
   `json.Unmarshal` into a throwaway struct); 3. write; 4. `f.Sync()`; 5. `f.Close()`;
   6. chmod `0600`; 7. `os.Rename(tmp, cfgPath)` (atomic on same fs); 8. best-effort parent
   dir `fsync`; 9. keep `cfgPath + ".bak"` last-known-good before rename.
- All writers already serialize on `cfgLock`, so no new writer coordination needed.

**API to preserve:** on-disk JSON schema unchanged; all `Save()` callers unchanged.

**Tests (write first):** simulate marshal/validate failure → primary untouched; corrupt
tmp never replaces primary; perms 0600; load falls back to `.bak` when primary is corrupt
(controlled). Use `t.TempDir()`.

**Migration:** none (same file, same schema; `.bak`/`.tmp` are additive). **Rollback:** revert.

**Risk:** extra fsync per save; saves are infrequent (config mutation, not per request) so
no hot-path impact.

**Commit:** `fix: make config persistence atomic`.

---

## Item P1.4 — Responses ownership by principal

**Verified current behavior (HEAD):** `proxy/responses_store.go`, `proxy/responses_handler.go`
- `storedResponseDoc` (store:169) has **no** owner field. `loadResponse(id)` (store:84)
  takes no owner. `saveResponse` (store:41) writes atomically (tmp+rename 0600).
- Handler computes `apiKeyID := apiKeyIDFromContext` but `loadResponse(previous_response_id)`
  is called with no owner → any key can read any stored response via `previous_response_id`.
- IDs from `crypto/rand` 12 bytes → not brute-forceable, but cross-key reference leaks.

**Design:**
- Add `OwnerPrincipalID string` + `CreatedByKeyID string` to `storedResponseDoc`.
- Owner = stable `ApiKeyEntry.ID` (rotation-in-place keeps the ID → conversations survive
  key rotation). No `PrincipalID` abstraction yet (single evidence-backed owner).
- `saveResponse` records owner from request context. New `loadResponseForOwner(id, ownerID)`:
  owner mismatch → generic "stored response not found" (no existence disclosure). Keep
  `loadResponse` only for internal/no-auth paths.
- **Legacy (owner-empty) docs:** when auth is enabled, deny access (no "first accessor
  becomes owner"); purge by TTL. When auth disabled, single-user/anonymous scope allows access.

**API to preserve:** Responses API request/response schema unchanged; continuation still
works for the owning key. The only behavior change: cross-owner reads now 404 (documented
security change).

**Tests (write first):** same principal allowed; different principal denied (generic 404);
raw-key rotation keeps ownership if ID stable; legacy owner-empty behavior (auth on/off);
auth-disabled anonymous scope.

**Migration:** old docs have empty owner → treated as legacy (deny under auth, TTL purge).
No data loss for the owning single-user/no-auth case. **Rollback:** revert; owner field is
additive and ignored by old code.

**Risk:** a multi-key deployment that *relied* on cross-key sharing (unlikely/none) would
break — that is the intended fix. Low perf (one string compare).

**Commit:** `security: scope stored responses by principal`.

---

## Item P1.5 — Endpoint validation at outbound boundary

**Verified current behavior (HEAD):** token endpoints come from persisted config /
discovery; validation is not centralized and persisted config is trusted. `buildKiroTransport`
(`proxy/kiro.go`) does not set `CheckRedirect`.

**Design:** central `EndpointPolicy.Validate(rawURL)` (new `proxy/endpoint_policy.go` or in
auth), called at: import, post-discovery, config load, and **immediately before** each
outbound token request. Rules: require HTTPS; reject IP literals; reject look-alike suffixes
(allow-list of known IdP hosts/suffixes); reject URL-embedded credentials; set
`CheckRedirect` to re-validate each hop or fail closed.

**API to preserve:** legitimate configured IdP endpoints continue to work.

**Tests (write first):** accept known-good; reject http, IP literal, `user:pass@host`,
look-alike suffix, disallowed redirect target.

**Migration:** a persisted endpoint that fails the new policy is rejected at use with a clear
error (does not silently corrupt). **Rollback:** revert.

**Risk:** over-strict allow-list could reject a valid tenant host — mitigate by matching the
hosts the code already targets (AWS SSO/OIDC + configured external IdP). **Commit:**
`security: validate idp endpoints at outbound boundary`.

---

## Item P1.6 — Ingress body limits

**Verified current behavior (HEAD):** handlers read request bodies without a global size cap
in several places.

**Design:** wrap body reads with `http.MaxBytesReader` per endpoint class (large for
messages/chat/responses; small for admin/config/import/token-count), returning `413` on
overflow. Centralize the limits as named constants.

**Tests (write first):** oversized body → 413; within-limit → normal. Per class.

**Risk:** limit set too low could reject legitimate large prompts — choose generous
message-class limit. **Commit:** `security: enforce inbound body limits`.

**Audit delta:** none; matches rev2.

---

## Item P2 — Upstream forwarding correctness

**Verified current behavior (HEAD):** `proxy/upstream_forward.go`
- `tryForwardUpstream` (78) uses `http.NewRequest("POST", url, ...)` — **no context**, so a
  client disconnect does not cancel the upstream call/stream.
- `h.recordSuccess(0,0,0)` (115) — forwarded success records zero tokens ⇒ per-key usage
  counter never increments for forwarded traffic (usage-accounting gap; the auth gate itself
  is enforced earlier in `authenticate`).
- Metric recorded at header receipt, not stream end. Only ~5 request headers set.
  `sendErr(502, "upstream request failed: "+err.Error())` leaks internal error text.
- `copyUpstreamResponse` preserves only Content-Type.
- Transport: `buildKiroTransport`/`GetClientForProxy` (`proxy/kiro.go:75,133`) use a blanket
  `Timeout: 5*time.Minute` and omit `DialContext`/`TLSHandshakeTimeout`/`ResponseHeaderTimeout`.

**Design (keep route/model behavior identical):**
- **8.1 Cancellation:** `http.NewRequestWithContext(r.Context(), ...)`. Client disconnect
  cancels upstream + SSE. Do not reuse a cancelled context to write a second error response.
- **8.2 Transport:** dedicated forward transport with `DialContext` (dial timeout),
  `TLSHandshakeTimeout`, `ResponseHeaderTimeout`, `ExpectContinueTimeout`, tuned idle/per-host
  conns; **no** blanket client `Timeout` that would truncate a valid long SSE stream — rely on
  response-header timeout + context instead.
- **8.3 Usage accounting:** keep the 429-before-handler gate. Record real per-key accounting on
  forwarded success. **Integration test written first** proving: over-limit → 429, no forward;
  within-limit → forward + counters unchanged at HEAD; after fix → counters increment per
  policy. Policy expressed in config/code: count by request initially (documented), parse
  upstream usage when present, fall back to request-count when absent; forwarded cost multiplier
  left as an open product decision (default 1, documented — no fabricated pricing).
- **8.4 Headers/errors:** request allow-list (`anthropic-beta`, idempotency, tracing/request-id,
  needed compat headers); never forward client `Authorization` upstream. Response allow-list
  (`Retry-After`, rate-limit, provider request-id, Content-Type, Content-Encoding when truly
  passthrough). Client-facing errors are generic; internal logs redacted (no host/IP/topology).
- **8.5 Metrics:** split TTFB, total stream duration, status, bytes relayed, cancellation,
  upstream timeout, usage. Record total latency after stream completion.

**Existing tests:** forwarding covered partially; add the above. **Tests:** cancellation
propagation (client cancels → upstream ctx done, no double-write); transport timeouts; usage
integration; header allow-list; error redaction.

**Migration:** none. **Rollback:** revert forwarding commits (grouped).

**Risk:** must not change SSE event ordering or truncate valid streams — verified by a fake SSE
backend test. **Commits:** `fix: propagate cancellation through upstream forwarding`,
`feat: account forwarded usage` (+ transport/headers folded appropriately).

---

## Item MP — Multi-profile discovery + picker (from zsecducna concept)

**Verified current behavior (HEAD):** profile resolution probes fallback regions but the
picker/multi-region discovery UI does not exist here. `pool` caches per-account model lists
(`SetModelList`/`GetModelList`). Hot path reads a single resolved profile.

**Design (separate discovery from selection; concept from `5095a2a0`/`4afdb79c`, re-implemented):**
- `DiscoveredProfile{ARN, Region, DisplayName}` (no secrets).
- `DiscoverProfiles(ctx, snapshot) ([]DiscoveredProfile, error)`: probe all configured
  candidate regions (IDC + external IdP), don't stop at first, dedupe by ARN, stable-sort,
  partial success (one region fails → return others + warning; all fail → classified error),
  respect ctx cancellation, obtain tokens **only** via `TokenManager` (no side refresh).
- `GetPinnedProfile(accountID)` reads the pinned profile from the account snapshot.
- `SelectProfile(ctx, accountID, arn, region)`: validate input → verify ARN still exists at
  region → prepare new snapshot (arn+region+model state) → persist atomically → publish to pool
  → invalidate **only that account's** model cache → in-flight requests keep old snapshot, new
  requests get new. Any pre-commit failure leaves the account unchanged; persist failure uses the
  same durability policy as config/token state.

**Hot-path invariant (must verify in report):** proxy request path only *reads* the pinned
profile from the snapshot; discovery never runs per request and adds no lock to the hot path.

**Admin API/UI:** list discovered profiles, get pinned, select; return partial-region
warnings; never return tokens/secrets. UI: show region + shortened ARN + pinned state, disable
button during op, no auto-switch on dropdown open, confirm before switching an active account,
show model-cache refresh result, no sensitive data in localStorage.

**Tests (write first):** 1 region/1 profile; 2 regions/many; duplicate ARN dedupe; region A
fails B ok; all fail; selected profile vanished pre-commit; switch during in-flight request;
persist fail → old profile kept or degraded; only that account's model cache invalidated; IDC
fallback; external-IdP fallback; ctx cancellation. `-race`. **Benchmark:** hot path unchanged.

**Migration:** additive fields (pinned profile already representable); accounts without a pin
use existing automatic first-match/default resolution. **Rollback:** revert; picker is additive.

**Commit:** `feat: add multi-profile discovery and picker`.

---

## Item LRU — Bounded O(1) LRU + prompt-cache metrics (from ngh1105 concept)

**Verified current behavior (HEAD):** `proxy/cache_tracker.go`
- `entriesByAccount map[string]map[[32]byte]promptCacheEntry`, guarded by one `sync.Mutex`.
- `pruneExpiredLocked` scans **every** account's **every** entry on **every** `Compute`/`Update`
  → O(total entries) per call. Benchmark confirms ~50x growth 1→64 accounts.
- Semantics: prefix/fingerprint tracker + usage-metadata generator (NOT a model/inference
  cache). Per-account isolation. No persistence.

**Design (re-implemented from concept `36a07999`/`216c65b2`):**
- Bounded LRU: `map[cacheKey]*list.Element` + `container/list`, where
  `cacheKey{AccountID string; Fingerprint [32]byte}` — a **single global** capacity with the
  account ID in the key (keeps isolation; bounds total memory regardless of account count).
- O(1) amortized lookup/insert/update/move-to-front/evict. TTL checked on lookup; expired entry
  removed on access; capacity-full → evict LRU tail. Incremental, budgeted cleanup only; **no**
  full O(n) scan on the hot path; no new background goroutine.
- Preserve exactly: fingerprint algorithm, prefix selection, token counting, TTL semantics,
  cache creation/read usage fields, per-account isolation, response format. No persistence, no
  quota-savings claims.
- Thread safety: keep a single mutex unless a benchmark proves sharding is needed (avoid
  over-engineering).

**Metrics:** hits, misses, creations, evictions, expired-removals, current-entries, capacity.
Thread-safe; no prompt/fingerprint/token content in metrics; global aggregate + optional
per-account only in admin API. Backward-compatible; do not change the public usage response.

**Tests (write first):** hit; miss; creation; update existing; move-to-front; capacity eviction
order; TTL expiry; expired-metric; per-account isolation (A cannot hit B); global capacity;
concurrent Compute/Update; metrics snapshot consistency; no prompt/fingerprint in metrics. `-race`.

**Benchmark (before/after):** 100 & 1000 entries, capacity pressure, high hit/miss, N accounts,
parallel. Goal: remove the O(n) prune growth, no ns/op or alloc regression on the hot path,
bounded memory. Save `docs/benchmarks-after-hardening.txt` + `docs/performance-comparison-hardening.md`
with real numbers. Regression ⇒ root-cause/optimize/rollback, never hide.

**Migration:** in-memory only; no schema. **Rollback:** revert; tracker interface to callers
(`Compute`/`Update`/`BuildClaudeProfile`) preserved.

**Commits:** `perf: replace prompt tracker map with bounded lru`, `feat: expose prompt-cache metrics`.

---

## Item GS — Graceful shutdown

**Verified current behavior (HEAD):** `main.go` calls `ListenAndServe[TLS]` with no
`signal.Notify`/`srv.Shutdown`; background refresh `stopRefresh` channel never closed.

**Design:** `signal.NotifyContext(SIGINT/SIGTERM)`; on signal, `srv.Shutdown(ctx)` with a drain
deadline; stop the refresh ticker/goroutine; flush any dirty token/config state (TokenManager
`PersistenceDegraded` retry) and metrics; reject new requests during shutdown; drain in-flight
SSE within the deadline or cancel explicitly.

**Tests:** integration — start server, open a streaming request, trigger shutdown, assert clean
drain within deadline. **Commit:** `reliability: add graceful shutdown`.

---

## Commit strategy (ordered)

1. `test: restore green translator baseline` ✅ (`4bd9221`)
2. `test: add hot-path benchmark harness and capture baseline` ✅ (`1224c69`)
3. `security: reject insecure public defaults`
4. `test: add concurrency and ownership reproductions`
5. `fix: return immutable account snapshots`
6. `feat: centralize token refresh coordination`
7. `fix: make config persistence atomic`
8. `security: scope stored responses by principal`
9. `security: validate idp endpoints at outbound boundary`
10. `security: enforce inbound body limits`
11. `fix: propagate cancellation through upstream forwarding`
12. `feat: account forwarded usage`
13. `feat: add multi-profile discovery and picker`
14. `perf: replace prompt tracker map with bounded lru`
15. `feat: expose prompt-cache metrics`
16. `reliability: add graceful shutdown`
17. `docs: add migration and performance reports`

Never commit with related tests failing. No squash. No push/merge to
`feat/upstream-forwarding` without request.

---

## Open product decisions (not code-decidable)

- Forwarded-usage cost policy (request vs token vs credit; cost multiplier). Default: count by
  request, multiplier 1, documented; parse upstream usage when present.
- Legacy owner-empty stored responses: deny under auth + TTL purge (chosen); no owner-adoption.
- LRU global capacity value (default chosen with a benchmark-backed number; configurable).

