# Kiro-Go — Architecture & Security Audit (Revision 2)

> Supersedes `architecture-and-security-audit-2026-07-12.md` (Rev 1). Rev 2
> incorporates a review that challenged the severity table, threat model, and
> plan. All `needs-more-check` items from Rev 1 are **closed here by code
> tracing** (see §0). Severities are recalibrated against an explicit threat
> model (§3). Where Rev 1 was wrong, it is marked **CORRECTED**.

## Revision & method

- **Repo/branch/SHA:** `topmetax-ctrl/Kiro-Go` @ `9bdc617943cf924ba4529b0e9e19573183e2a837`, `feat/upstream-forwarding`. Working tree clean (only untracked audit docs).
- **Toolchain:** Go 1.26.5 (go.mod: `go 1.21`). Dep: `google/uuid v1.6.0`.
- **Baseline:** `go vet` clean; `go test ./...` → 2 pre-existing FAILs (`translator_test.go:558`, `:635`); `go test -race ./...` no race in existing suite (suite does not exercise the pool-pointer pattern — not proof of absence); `golangci-lint` not installed.
- **Research:** Microsoft Learn, *Refresh tokens in the Microsoft identity platform* (accessed 2026-07-12): rotation without revoke-on-use, verbatim quote in §5.

---

## 0. Closed `needs-more-check` items (were open in Rev 1)

**NC-1 — CORS/admin exploit. CORRECTED — Rev 1 overstated it.**
Admin auth is read from the `X-Admin-Password` **header** or the `admin_password` **cookie** (`proxy/handler.go` `handleAdminAPI`):
```go
password := r.Header.Get("X-Admin-Password")
if password == "" {
    cookie, _ := r.Cookie("admin_password")
    if cookie != nil { password = cookie.Value }
}
if subtle.ConstantTimeCompare([]byte(password), []byte(config.GetPassword())) != 1 { ... 401 }
```
`X-Admin-Password` is **not** in `Access-Control-Allow-Headers` (`ServeHTTP` line 414). The cookie is set client-side only, `SameSite=Strict` (`web/app.js`: `document.cookie = 'admin_password=' + … + '; path=/; SameSite=Strict'`), stored in `localStorage`/`sessionStorage`; the server never issues it. `Access-Control-Allow-Origin: *` **cannot** carry credentials cross-origin per the Fetch spec, and a cross-origin `fetch` setting a custom `X-Admin-Password` header triggers a preflight that fails (header not allow-listed). **Conclusion: the browser-driven cross-origin admin exploit described in Rev 1 is NOT viable.** `ACAO: *` remains an unnecessarily broad default (defense-in-depth), but there is no demonstrated exploit. Severity: **LOW**.

**NC-2 — Which HTTP client does external_idp refresh use / redirect policy.**
`buildKiroTransport` (`proxy/kiro.go`) sets `Proxy`, `MaxIdleConns`, `IdleConnTimeout`, `ForceAttemptHTTP2` but **no** `CheckRedirect` override, so clients built from it follow redirects by default (Go default: up to 10). The SSO *loopback exchange* client (`auth/kiro_sso.go externalIdpHTTPClient`) sets `CheckRedirect: ErrUseLastResponse` (does not follow). The **refresh** path (`RefreshExternalIdpToken`) uses the auth client, which does **not** pin `CheckRedirect` → a 30x from a (validated) host could redirect the token POST elsewhere. This **adds a redirect vector to CRIT/HIGH-EndpointValidation** but only matters once the primary allow-list gap is considered. Recorded under the endpoint finding.

**NC-3 — `Save()` call sites (was LOW-003). CLOSED.**
~35 call sites enumerated in `config/*.go`; every one is reached via `saveLocked()` or a function that already holds `cfgLock` (e.g. `UpdateAccountToken`, `AddApiKey`). `Save()` reads `cfg` without taking the lock, but no caller invokes it without the lock held. The in-file comment (config.go ~546) documents this invariant. **No unlocked-marshal race in practice.** The remaining concern is purely the non-atomic write (HIGH-Config), not lock discipline. LOW-003 is closed as *not a defect*, with the caveat that the exported unlocked `Save()` is a footgun for future callers → make it private or lock it in Phase 3.

**NC-4 — idc fallback region. CLOSED (concept confirmed N/A as framed).**
`profileProbeRegions` builds `[defaultProbeRegion(account), us-east-1, eu-central-1]` (dedup) and the resolver loop probes all of them **regardless of auth method**. So the branch already probes eu-central-1 for `idc` accounts; ngh1105's method-gated fix does not apply here. (A targeted test should still assert an `idc` account reaches the fallback list — added to Phase 0.)

**NC-5 — Forward quota bypass: exact boundary. CLOSED & CORRECTED.**
Per-key quota **is** enforced in `authenticate` (`proxy/auth.go`) *before* the handler runs:
```go
if overToken, overCredit := config.ApiKeyOverLimit(*entry); overToken || overCredit {
    ... return 429 "token/credit limit exceeded"
}
```
`tryForwardUpstream` runs **inside** the handler, after auth passed. So an **already-over-limit** key is rejected at the door and cannot forward. The real gap is different from Rev 1's framing: a **within-limit** key's forwarded request calls `h.recordSuccess(0,0,0)` and never increments per-key usage → the key can issue **unlimited forwarded calls without ever advancing toward its own limit**. This is a **usage-accounting bypass**, not an auth-gate bypass. Severity depends on whether forwarded upstreams are billable (see MED/HIGH-Forward + integration test requirement).

---

## 1. Executive summary (unchanged facts, recalibrated risk)

Single-process Go proxy exposing Anthropic/OpenAI-compatible APIs over a pool of Kiro accounts, with OAuth refresh, SSE, admin panel, native web-search (SearXNG+Tavily), and per-model upstream forwarding + metrics dashboard.

**Strengths (verified):** web-search subsystem (SSRF-closed, context-cancelled, size-limited, bounded concurrency, no race); forwarding metrics fully mutex-guarded; responses storage atomic + crypto-random IDs + path-traversal-safe; Entra allow-list uses correct dot-boundary matching; foreground refresh already dedups.

**Top risks after recalibration:**
- **HIGH-DEPLOY** — default `Host: 0.0.0.0` + default password `changeme` + empty admin allow-list (all IPs) + admin API accepts `changeme` immediately with only a log warning. Public-by-default with a known password. **This is the finding to fix first** (was buried inside Rev 1's HIGH-005 bundle).
- **HIGH-POOL-RACE** (was CRIT-002) — pool getters return live-slice pointers after unlock; concurrent mutation is a real data race. Downgraded to HIGH pending a reproduction that demonstrates a concrete bad outcome (§4).
- **HIGH-RESP-OWNER** (was CRIT-003) — no ownership model on stored responses; conditional-CRIT only in strict multi-tenant with sensitive prompts.
- **HIGH-REFRESH** (was HIGH-002) — background/admin refresh bypass the global refresh mutex; discarded persist error; no transaction invariant.
- **HIGH-CONFIG** — non-atomic `config.Save`.
- **HIGH-BODY** — unbounded ingress `io.ReadAll(r.Body)` (High if public/shared; Medium if private/trusted+fronted).
- **MED-ENDPOINT** (was CRIT-001) — persisted IdP endpoint not re-validated before refresh POST + redirect-follow vector. Downgraded: exploit requires config/import write capability (privilege-boundary argument in §4).
- **MED-FORWARD** — no context propagation, usage-accounting bypass, header loss, mis-timed latency, missing transport timeouts, `err.Error()` leak, response-header loss.

**Production multi-user:** not yet — HIGH-DEPLOY, HIGH-POOL-RACE, HIGH-RESP-OWNER, HIGH-REFRESH must land first.
**Horizontal scale:** single-instance only (§8).

---

## 2. Architecture map

Unchanged from Rev 1 — see that file for the module table and the 8 Mermaid flow diagrams (direct dispatch, forwarding, refresh, responses/`previous_response_id`, web-search loop, import/profile, background refresh, model cache). No diagram changed; only severities and the transport detail below.

**Transport detail (new, closes NC-2 / transport-audit gap):** `buildKiroTransport` (`proxy/kiro.go`):
```go
t := &http.Transport{
    MaxIdleConns:        100,
    MaxIdleConnsPerHost: 20,
    IdleConnTimeout:     90 * time.Second,
    DisableCompression:  false,
    ForceAttemptHTTP2:   true,
}
```
**Missing:** `DialContext` (no dial timeout), `TLSHandshakeTimeout`, `ResponseHeaderTimeout`, `ExpectContinueTimeout`. Forwarding uses `GetClientForProxy` → blanket `Timeout: 5 * time.Minute`. That blanket timeout is wrong for both directions: it can **cut a legitimate long SSE stream at 5 min**, yet provides **no** protection against an upstream that accepts the connection then never sends headers (no `ResponseHeaderTimeout`). Streaming needs context deadlines + transport-level header/dial timeouts, not a single client `Timeout`.

---

## 3. Threat model & severity rubric (new — was missing in Rev 1)

**Actor capabilities considered:**

| Actor | Capability |
|---|---|
| A0 Remote unauthenticated | reach exposed ports only |
| A1 Remote API-key holder | valid `/v1/*` key (one of many in multi-user) |
| A2 Admin | knows admin password |
| A3 Local filesystem writer | can read/write `data/config.json` |
| A4 Compromised container | full in-process access |
| A5 Malicious upstream | operator-configured forward target misbehaves |
| A6 Malicious web content | returned by a web-search provider |

**Deployment postures:** (P-single) single trusted user; (P-multi) multiple API-key tenants sharing one instance; (P-public) internet-exposed.

**Severity rubric used below:**
- **Critical** — remote/low-privilege actor (A0/A1) achieves cross-tenant data disclosure, account takeover, or unauthenticated compromise, with a demonstrated path.
- **High** — realistic exploit for a plausible posture, OR guaranteed data loss / correctness violation under normal operation, OR a dangerous default.
- **Medium** — requires elevated privilege (A2/A3), or impact bounded (cost/DoS/observability), or exploit not yet demonstrated.
- **Low** — defense-in-depth, timing side-channels behind high-entropy secrets, or requires unlikely preconditions.

Findings are pinned to (actor, posture). No Critical is asserted without a demonstrated low-privilege path — hence the Rev 1 CRITs are recalibrated.

---

## 4. Verified findings (recalibrated)

### HIGH-DEPLOY-001 — Public-by-default with default password (split out of Rev 1 HIGH-005)
- **Severity:** High (Critical under P-public if left as shipped) · **Confidence:** High · **Actor:** A0 · **Posture:** P-public
- **Evidence:** `config/config.go` init default `Host: "0.0.0.0"`, `Password: "changeme"`; empty `AdminAllowlist` ⇒ `adminIPAllowed` permits all IPs. `main.go` calls only `config.EvaluateSecurityWarnings()` which **logs** `Admin password is still the default "changeme". Change it now.` — no hard-fail, no forced rotation. `handleAdminAPI` accepts `changeme` via constant-time compare immediately.
- **Verified sub-questions (reviewer's checklist):** (1) fresh install does **not** force a password change; (2) admin API accepts `changeme` right away; (3) `ADMIN_PASSWORD` env can override but is optional; (4) only a log warning, no hard fail; (5) `RequireApiKey` default — **needs one more line to confirm default value**; regardless, the admin plane is the exposed risk.
- **Impact:** A default `docker run -p 8080:8080` exposes an admin panel whose password is publicly known. Full account/credential/route control by A0.
- **Minimal safe fix:** default `Host` to `127.0.0.1`; on startup, if bind is non-loopback AND password is `changeme`, **hard-fail** (not warn). Document the opt-in for `0.0.0.0`.
- **Tests:** startup refuses to serve on `0.0.0.0` with default password; loopback + default password warns only.
- **Migration risk:** Medium — behavior change; must be release-noted.

### HIGH-POOL-RACE-002 (was CRIT-002) — Pool getters leak live-slice pointers
- **Severity:** High · **Confidence:** High (race) / severity pending reproduction · **Actor:** internal concurrency · **Posture:** all
- **Evidence:** `pool/account.go` getters take `RLock`+`defer RUnlock` and `return &p.accounts[idx]`; `UpdateToken`/`UpdateStats` write the same struct under `Lock`; `Reload` replaces the whole slice (`p.accounts = weighted`). Reader holds the pointer past the critical section.
- **Why downgraded from Critical:** proven = data race + stale state + possibly inconsistent token/expiry. **Not** proven = cross-tenant token disclosure or takeover. Under the §3 rubric, that is High. It becomes Critical only if the Phase-0 reproduction shows account A's token used in account B's request (see required repro).
- **`Account` shape:** all value types (no nested maps/slices/pointers) → a value copy is self-contained; no deep-copy needed.
- **Reproduction required to finalize severity (must show one):** (a) request uses old token after a concurrent refresh; (b) request uses a just-disabled account; (c) lost stats update; (d) selection returns an account no longer in the pool; (e) token/profile pair from different generations.
- **Fix:** getters return `config.Account` by value under the read lock; callers treat it as a snapshot; mutations go through pool methods (`ensureValidToken`'s in-place writes to the returned `*account` must be re-pointed at `pool.UpdateToken`).
- **Migration risk:** Medium (wide caller change).

### HIGH-RESP-OWNER-003 (was CRIT-003) — No ownership model on stored responses
- **Severity:** High (Critical under P-multi with sensitive stored prompts) · **Confidence:** High · **Actor:** A1 · **Posture:** P-multi
- **Evidence:** `responses_handler.go` computes `apiKeyID` but `loadResponse(id)` (`responses_store.go`) takes no identity and `storedResponseDoc` persists no owner.
- **Attack complexity:** NOT low — needs a valid second key + a leaked victim response ID (logs/telemetry/history/screenshot) + within 30-day TTL. IDs are `crypto/rand` (not enumerable).
- **Ownership model (reviewer's point — CORRECTED design):** bind responses to a **principal**, not a credential. Recommended schema field `OwnerPrincipalID`; where no tenant/principal model exists yet, populate it with a **stable key ID** as an interim, so key rotation does not orphan conversations later. Do **not** key on the raw API key.
- **Fix:** add `OwnerPrincipalID` to `storedResponseDoc`; `loadResponse(id, principal)` returns generic not-found on mismatch (no enumeration signal). Legacy empty-owner policy: inaccessible under auth-on; purge at next TTL (Decision log).
- **Tests:** cross-key isolation (same/diff/legacy-empty/auth-disabled).
- **Migration risk:** Medium (legacy data purge/migrate decision).

### HIGH-REFRESH-004 (was HIGH-002) — Refresh coordination gaps + no transaction invariant
- **Severity:** High · **Confidence:** High · **Actor:** internal · **Posture:** all
- **Evidence:** `ensureValidToken` holds a **single global** `tokenRefreshMu` + double-checks pool state. `refreshAllAccounts` (background), admin batch/single, and `ResolveProfileArn` fallback call `auth.RefreshToken` with **no** shared lock; `refreshAllAccounts` **discards** `UpdateAccountToken`'s error.
- **Real impact (CORRECTED via §5 research):** concurrent refresh of the same account produces an **orphaned-but-valid** rotated token (Entra does not revoke on use) — **not** account lockout. Plus: global mutex serializes unrelated accounts; discarded persist error → silent memory/disk divergence.
- **Design (with reviewer's three additions):** one `TokenManager` using a **single** `singleflight.Group` keyed by account ID (`Do(accountID, fn)` — not one Group per account). Inside the callback: (1) re-read authoritative latest state (never refresh off a pre-wait snapshot); (2) validate the full response (LOW-REFRESH-VALIDATION); (3) stamp a **generation/version** and use compare-and-swap so a late-completing old refresh cannot overwrite newer state; (4) **persist atomically first, publish to memory only on persist success**; propagate the error. Note singleflight solves only in-process dedup — cross-replica needs a distributed lease (§8).
- **Transaction invariant (reviewer's §6 — now specified):**
  1. singleflight by account ID → 2. read latest authoritative snapshot → 3. if still fresh, return it → 4. refresh via IdP → 5. validate response fully → 6. build `AccountTokenState{generation+1}` → 7. persist atomically → 8. publish to memory only after persist succeeds → 9. return new state to waiters → 10. on persist failure, do NOT mutate memory (fail closed; caller retries).
- **Tests:** 100 concurrent refreshes of one account → exactly one IdP call; N accounts refresh in parallel → no serialization; persist-fails → memory unchanged, error surfaced; late old-generation write rejected. All `-race`.
- **Migration risk:** Medium (touches all refresh sites).

### HIGH-CONFIG-005 — Non-atomic `config.Save`
- **Severity:** High · **Confidence:** High · **Actor:** internal · **Posture:** all
- **Evidence:** `config.Save` = `json.MarshalIndent` then `os.WriteFile(cfgPath, …)` (no tmp+rename); called on every token refresh.
- **Production-grade fix (reviewer's list):** tmp file in **same dir** → write → `f.Sync()` → close → chmod 0600 → `os.Rename` → `Sync()` parent dir → keep last-known-good backup → validate JSON before commit → single-writer (already under `cfgLock`).
- **Tests:** fault-injection (rename fail → primary intact; partial write → no truncated primary; corrupt-on-load → fall back to backup).
- **Migration risk:** Low.

### HIGH-BODY-006 — Unbounded ingress body
- **Severity:** High under P-public/P-multi; Medium under P-single-fronted · **Confidence:** High · **Actor:** A0/A1
- **Evidence:** `io.ReadAll(r.Body)` at Claude/OpenAI/responses handlers + `json.NewDecoder(r.Body)` at import/upstreams — no `http.MaxBytesReader`. `GetMaxPayloadBytes` caps the downstream serialized Kiro payload only.
- **Fix:** ingress middleware wrapping `r.Body` in `MaxBytesReader` per endpoint class → 413.
- **Migration risk:** Low.

### HIGH-SHUTDOWN-007 (was HIGH-004, reviewer: reliability) — No graceful shutdown
- **Severity:** Medium→High boundary; classify **Medium (reliability)** by default per reviewer, High only if deploys are frequent/streaming-heavy · **Confidence:** High
- **Evidence:** `main.go` `ListenAndServe` with no `signal.Notify`/`srv.Shutdown`; `stopRefresh` never closed.
- **Impact:** SSE cut mid-stream on SIGTERM; no drain; partial stats-window loss. No corruption path identified → Medium.
- **Fix:** signal handling + `srv.Shutdown(ctx)` drain deadline + close `stopRefresh` + flush stats.

### MED-ENDPOINT-008 (was CRIT-001) — Persisted IdP endpoint not re-validated before refresh
- **Severity:** Medium (High as defense-in-depth if import can set endpoints without secret-read) · **Confidence:** High · **Actor:** A3 (or a limited-privilege import path) · **Posture:** all
- **Evidence:** `RefreshExternalIdpToken` / `resolveExternalIdpTokenEndpoint` POST the refresh token to `account.IdPTokenEndpoint`/`IssuerURL` (from `data/config.json`) with no `validateExternalIdpURL` call; the auth client sets no `CheckRedirect` (NC-2) so a redirect from a valid host can bounce the POST.
- **Why downgraded from Critical (reviewer's privilege-boundary argument):** the primary path assumes A3 (config write). An actor who can rewrite `data/config.json` already holds the refresh tokens and secrets in that same file — exfil-via-endpoint does not raise their capability. Critical would require a demonstrated **low-privilege** path, e.g.: an import/edit API that sets `idpTokenEndpoint` without granting secret-read; a metadata-only account-edit endpoint; untrusted stored-data injection; a write-only config channel; or the redirect-from-allow-listed-host vector. Those are **open questions to prove**, not assumed. Until proven, this is High-value defense-in-depth (fail-closed is cheap), rated Medium.
- **Fix:** central `EndpointPolicy.Validate` called at import, discovery, and immediately before every outbound token request, plus on config load; pin `CheckRedirect` to reject cross-host redirects on all auth clients.
- **Tests:** table-driven allow-list (apex/sub/lookalike `…microsoftonline.com.evil.com`/IP-literal/http/punycode); tampered-endpoint refresh rejected; disallowed redirect rejected.
- **Migration risk:** Low.

### MED-FORWARD-009 (was MED-001, expanded per reviewer) — Forwarding correctness, cost, and leakage
- **Severity:** Medium (High under P-multi if upstreams are billable) · **Confidence:** High · **Actor:** A1/A5
- **Sub-findings (all code-verified):**
  1. `http.NewRequest` with no `r.Context()` → client disconnect does not cancel upstream.
  2. **Usage-accounting bypass (CORRECTED framing, NC-5):** within-limit key's forwarded calls never increment usage (`recordSuccess(0,0,0)`) → unlimited billable calls possible without tripping the key's own limit. *Requires an integration test to state the exact counter behavior before fixing.*
  3. Latency recorded at response headers, not stream completion (no TTFB vs total distinction).
  4. Request headers dropped except 5 hard-coded (`anthropic-beta`, trace, idempotency, `x-stainless-*` lost).
  5. Full-body RAM buffering (`bytes.NewReader` on pre-read `[]byte`).
  6. Upstream `BaseURL` stored with **no** validation (`apiUpdateUpstreams`→`UpdateUpstreamConfig`) → admin-only SSRF (A2).
  7. **Transport timeouts missing** (see §2): 5-min blanket `Timeout` both truncates legit SSE and fails to cap a header-stalled upstream; no dial/TLS/response-header timeout.
  8. **Error leakage:** `sendErr(502, "upstream request failed: "+err.Error())` returns raw error to the client → can leak upstream hostname/IP/proxy topology.
  9. **Response headers dropped:** `copyUpstreamResponse` preserves only `Content-Type` → loses `Retry-After`, rate-limit headers, request-id, content-encoding. Needs a **response**-header allow-list, not just request.
- **Integration test required before fix (reviewer's BDD):** given exhausted key → 429 at auth (confirmed); given within-limit key + routed model → forwarded, and assert exactly which usage/quota counters change (expected: none today). Also: client-cancel propagates; header allow-lists (req+resp); latency-at-completion; oversized body 413; invalid BaseURL rejected; error redaction.
- **Architecture:** `RouteResolver`, `RequestTransformer`/`ResponseTransformer`, context-aware `StreamingProxy`, `UsageAccounting`+`QuotaService`, `MetricsRecorder`. **No** circuit breaker / health / LB until >1 upstream per route (speculative today).

### MED-CACHE-010 (was MED-002) — Prompt-cache tracker unbounded + O(n) prune
- **Severity:** Medium · **Confidence:** High
- **Classification (unchanged):** stores only `{ExpiresAt, TTL}` per fingerprint per account — a **usage-metadata generator**, not a cache backend. Per-account scoped ⇒ cross-account isolation already present (ngh1105 C1 N/A here).
- **Evidence:** no LRU/max-entries; `pruneExpiredLocked` scans the whole map every `Compute`/`Update`.
- **Fix:** bounded O(1) LRU (`container/list`), configurable max-entries, keep per-account scope. Persistence optional and **must not** be sold as quota savings (holds no cached computation).

### MED-SECRETS-011 (new, split from Rev 1 HIGH-005 per reviewer) — Config secret model
- **Severity:** Medium · **Confidence:** High · **Actor:** A3
- **Evidence:** config persists, in plaintext: client API keys (`ApiKeyEntry.Key`), OAuth refresh tokens (`Account.RefreshToken`), upstream API keys (`UpstreamProvider.ApiKey`), and possibly proxy credentials in `ProxyURL`.
- **Nuance (reviewer):** hashing only the client key does **not** make the file leak-safe. Differentiate: client auth key → store **hash**; upstream/provider secret + OAuth refresh token → encrypt or external secret store; stable public identifier → plaintext OK.
- **Fix:** hash client keys (compare by hash — also removes the timing concern below); document/encrypt provider+refresh secrets or move to a secret manager.

### LOW-KEYCMP-012 (was inside HIGH-005) — Non-constant-time API-key compare
- **Severity:** Low · **Confidence:** High
- **Evidence:** `config/apikeys.go FindApiKeyByValue` uses `cfg.ApiKeys[i].Key == key` (linear `==`); legacy path `provided != expected`.
- **Note:** remote timing attack across network jitter + linear scan + high-entropy keys is impractical; **disappears** once keys are stored/looked-up by hash (MED-SECRETS-011). Do not prioritize separately.

### LOW-CORS-013 (was inside HIGH-005) — `ACAO: *` on all routes
- **Severity:** Low · **Confidence:** High · **Evidence & refutation:** see NC-1. Broad default; no demonstrated exploit. Scope CORS for `/admin` in Phase 3.

### LOW-REFRESH-VALIDATION-014 (was LOW-001) — Refresh response not validated
- **Severity:** Low · **Confidence:** High
- **Evidence:** refresh parsers check status 200 + JSON decode but not `AccessToken != ""` / `ExpiresIn > 0`; `expiresAt = now + 0` persists an immediately-stale token. Fold into the TokenManager validate step (HIGH-REFRESH-004).

### LOW-WEBSEARCH-015 (was LOW-002) — Result sanitization / sentinel escaping
- **Severity:** Low **today**, escalates if the model gains tools with side effects · **Confidence:** High · **Actor:** A6
- **Evidence:** results wrapped in a `WEB_SEARCH_RESULTS…END_WEB_SEARCH_RESULTS` fence with a warning; sentinel not escaped from content; raw snippet text injected.
- **Broader assessment (reviewer):** evaluate — other tools available to the model; whether web content can steer disclosure of the system prompt/secrets; per-result provenance; avoid raw HTML; prefer **structured** tool-result separation over a textual fence; ensure web content cannot drive out-of-policy follow-up fetches. Budget caps loops but not injection.

---

## 5. Claims disproved (unchanged from Rev 1, plus NC corrections)

- **Entra "one-time-use / burns token / locks account":** disproved. Microsoft Learn (accessed 2026-07-12): *"Refresh tokens replace themselves with a fresh token upon every use. The Microsoft identity platform doesn't revoke old refresh tokens when used to fetch new access tokens. Securely delete the old refresh token after acquiring a new one."* Confirmed: rotation without revoke-on-use. Inference (not verified against this deployment): the Kiro→Entra app-registration class (public/confidential) and tenant token-lifetime config.
- **Foreground refresh has no dedup:** disproved (global mutex + double-check).
- **Multi-region only external_idp:** disproved (method-agnostic probe; NC-4).
- **Cross-account prompt-cache sharing:** disproved for this branch (per-account key).
- **Forwarding stats race:** disproved (mutex-guarded).
- **Responses IDs guessable:** disproved (crypto-random) — it is an authZ gap.
- **NEW — Cross-origin browser can drive admin API via `ACAO:*`:** disproved (NC-1: custom header not allow-listed; `SameSite=Strict` cookie; `*` cannot carry credentials).
- **NEW — Rev 1 CRIT-001 "most severe finding":** corrected — Medium (privilege-boundary argument, NC-2/§4).
- **NEW — Rev 1 forward "quota bypass":** corrected — auth gate is enforced; the real gap is usage-accounting non-increment (NC-5).

---

## 6. Fork comparison

Unchanged from Rev 1 §6 (by SHA, port-concept-not-cherry-pick). Two clarifications: (a) ngh1105 C1 cross-account cache fix is **not needed** here; (b) zsecducna `e75e4e63 ValidateExternalIdpEndpoint` is the closest existing implementation to MED-ENDPOINT-008 — port the concept.

---

## 7. Target architecture (reviewer: avoid double migration; fewer interfaces)

**Decision — lightweight path (chosen):**
- Getters return an immutable `AccountSnapshot` (value). `AccountPool` **remains the concrete owner** of account state; all mutations go through its methods.
- **Do not** introduce an `AccountManager`/`AccountService` interface until a second implementation actually exists. This avoids migrating every call site twice (Rev 1 sequenced snapshot in Phase 1 then a manager in Phase 3 — dropped).
- `TokenManager` is a concrete type operating **through the pool's mutation methods**, holding the singleflight + transaction invariant (§4 HIGH-REFRESH-004).
- `ConfigStore` and `ResponseStore` become interfaces **only** where a real second backend (DB/object-store) is on the roadmap and a test seam is needed — that is a genuine boundary, so it earns an interface.
- Ingress middleware (`httpx`): body limits, request IDs, structured logging, `/admin` CORS scoping.
- Forwarding: concrete `StreamingProxy` + `UsageAccounting`/`QuotaService`; no breaker/health/LB yet.

Interfaces are justified by a consumer-side seam (test mock or real second impl), not by "large systems have interfaces."

---

## 8. Phased plan (reordered per reviewer)

### Phase 0 — Operational safety + green baseline (no behavior change beyond the deploy guard)
- **0.1** Confirm & fix HIGH-DEPLOY-001: hard-fail on non-loopback bind + default password; default `Host=127.0.0.1`. *(This is the one behavior change allowed in Phase 0 because it is the highest operational risk.)* Files: `main.go`, `config/config.go`. **M**.
- **0.2** Fix the 2 translator test failures → green baseline. **M**.
- **0.3** Reproduction tests (RED): pool race (HIGH-POOL-RACE-002, must show a concrete bad outcome to finalize severity), response ownership, refresh concurrency + late-generation write. **M**.
- **0.4** Close remaining open questions by code/test, not implementation: idc fallback assertion (NC-4); `RequireApiKey` default value; the forward usage-counter integration test (NC-5); the low-privilege endpoint-write question (MED-ENDPOINT-008). **M**.
- **0.5** CI gates: `go test ./...`, `go test -race ./...` (concurrency pkgs), `go vet`, committed golangci-lint/staticcheck config, dep-vuln scan, fuzz targets (URL parse, response IDs, translator, SSE parser); block merge on red baseline. **M**.

### Phase 1 — Correctness & isolation
1. Immutable account snapshots (HIGH-POOL-RACE-002).
2. `TokenManager` with singleflight + transaction invariant + response validation (HIGH-REFRESH-004, LOW-REFRESH-VALIDATION-014).
3. Atomic/durable `ConfigStore` (HIGH-CONFIG-005) + make unlocked `Save()` private (NC-3).
4. Response ownership by principal/stable-key-id (HIGH-RESP-OWNER-003).
5. `EndpointPolicy` validation before every outbound token request + redirect pinning (MED-ENDPOINT-008).
6. Ingress body limits (HIGH-BODY-006).

### Phase 2 — Forwarding correctness & cost control
Context propagation; transport timeouts (dial/TLS/response-header) + streaming via context deadline not blanket `Timeout`; quota/usage-accounting policy (decision-gated, test-proven); request+response header allow-lists; error redaction; TTFB + total-duration metrics; BaseURL SSRF validation.

### Phase 3 — Lifecycle & maintainability
Graceful shutdown (HIGH-SHUTDOWN-007); split `handler.go`; typed errors replacing string matching; structured logging + secret redaction; prompt-tracker bounded LRU (MED-CACHE-010); config validation on load; `/admin` CORS scoping (LOW-CORS-013); secret model (MED-SECRETS-011: hash client keys → also closes LOW-KEYCMP-012).

### Phase 4 — Scale-out (only when a 2nd replica is real)
Shared store (accounts/responses/metrics); distributed token lease or optimistic versioning (singleflight is in-process only); shared atomic per-key quota counter; background job leader-election/work-queue; Prometheus/OTel; stop multi-replica JSON-file writes.

---

## 9. Decision log
1. Pool: value snapshots + concrete pool owner; no manager interface yet (avoids double migration).
2. Refresh: single `singleflight.Group` keyed by account ID; transaction invariant persist-before-publish with generation CAS.
3. Response owner: `OwnerPrincipalID` (interim = stable key id); legacy empty-owner inaccessible under auth-on, purge at TTL.
4. Deploy default: hard-fail on public-bind + default password; default bind loopback.
5. Forward quota: **open policy** (§10) — but usage counters must at least be recorded even if quota is not enforced.
6. Prompt-tracker persistence: optional, not marketed as quota savings.
7. Interfaces only at real consumer seams (ConfigStore/ResponseStore when a 2nd backend is planned).
8. No cherry-picking bundled fork commits; port concepts with local tests.

## 10. Open questions (genuinely undecidable from code)
1. Forwarded-request quota **policy**: count against per-key quota? estimate usage from upstream response or ignore? (product decision — the code gap is proven; the intent is not.)
2. Kiro→Entra app-registration class + tenant refresh-token lifetime (not in this repo).
3. Legacy stored-response migration: purge vs migrate to first-accessor (retention/expectation decision).
4. Is there any **low-privilege** path (import/metadata-edit API) that can set `idpTokenEndpoint` without secret-read? If yes, MED-ENDPOINT-008 → High/Critical. (Phase 0.4 investigates; if none found, stays Medium.)
