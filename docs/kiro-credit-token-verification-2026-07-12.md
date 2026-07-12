# Kiro-Go Credit/Token Verification — 2026-07-12

Verify-only audit. **No production code was modified.** All findings are backed by
file/function/line evidence, a runnable test harness, and one official source.

---

## 1. Revision verified

| Field | Value |
|---|---|
| Repo | `topmetax-ctrl/Kiro-Go` (fork of `Quorinex/Kiro-Go`) |
| Branch | `feat/hardening-profile-picker-lru` |
| HEAD at report time | `3864c21` `fix: make config persistence atomic` |
| HEAD at session start | `028f34b` `fix: return immutable account snapshots` |
| Working tree | **Dirty** — `proxy/responses_handler.go`, `proxy/responses_store.go` modified (in-progress, does NOT build) |
| Toolchain | `go1.26.5 darwin/amd64` |
| vs `origin/main` | 9 ahead (measured at session start) |
| vs `upstream/main` | 33 ahead / 5 behind |

### Important caveat about the working tree

During this session an **external process advanced HEAD twice** (`028f34b` →
`bfc2278` → `3864c21`) and left new uncommitted edits in `responses_handler.go` /
`responses_store.go`. **Those edits currently break `go build ./proxy/`**
(`undefined: responsesOwnerPrincipal`, `loadResponse`, `context`; `saveResponse`
arity mismatch). This is unrelated WIP, not part of this audit.

- **Clean HEAD `3864c21` builds and all tests pass green.** Verified in an isolated
  `git worktree` at that SHA (`/tmp/kiro-verify`), which is where the harness ran.
- I did **not** touch the in-progress `responses_*.go` edits.
- The one temporary test file created for this audit lives only in the throwaway
  worktree and is **not** in the main tree (see §11).

All code findings below are pinned to code present at `3864c21` (the same content
as `028f34b` for every file cited except the two broken `responses_*.go` files,
which this audit does not depend on).

---

## 2. Executive verdict

**The dominant effect is an ACCOUNTING/REPORTING bug, not higher real credit
consumption.** The proxy reports `contextUsagePercentage × contextWindow` as the
request's `input_tokens` and overrides the real upstream token count with it. Real
Kiro **credits** (the actual billed unit) come straight from the upstream
`meteringEvent` and are summed correctly per round — the proxy does **not** invent
or inflate credits.

Two things can nonetheless make the proxy cost more **real credits** than the IDE,
but both are conditional and were not proven to be active in your runtime:
model/effort mismatch (Q7) and wrong profile (Q8).

| Hypothesis | Verdict |
|---|---|
| H1 `ctxPct × window` reported as request input tokens | **Confirmed** |
| H2 Context-derived value overrides real upstream input tokens | **Confirmed** |
| H3 Output token is always an estimate (upstream value discarded) | **Confirmed** |
| H4 Full history + system + tools re-sent every turn | **Confirmed** |
| H5 One logical chat = more Kiro inference calls than IDE | **Partially confirmed** (only with web_search; plain chat = 1 call) |
| H6 Proxy maps to a different model than IDE "Auto" | **Partially confirmed** (proxy pins a concrete model; IDE Auto is opaque) |
| H7 Proxy enables thinking/effort higher than IDE | **Disproved by default** (off unless suffix/native field/config) |
| H8 Proxy pinned to US while IDE uses EU Power | **Not verifiable** (code probes US-first & can pin US; runtime value unread) |
| H9 Prompt cache tracker is metadata-only, not cheaper backend | **Confirmed** |
| H10 Per-key quota counts estimated context occupancy, not billed usage | **Confirmed** (token quota); credit quota uses real credits |
| H11 Forwarded-request accounting unrelated to Kiro credit but can skew stats | **Confirmed** (global request count only; no token/credit) |
| H12 Retry/failover can double-meter credits | **Partially confirmed** (retry only before stream starts; see §7) |

---

## 3. Metric definitions

| Metric | Source (file:line) | Meaning | Client response | Quota | Real billing |
|---|---|---|---|---|---|
| **Metered credit** | `kiro.go:546-549` `meteringEvent.usage`; summed `kiro_conversation_runner.go:93` | Real Kiro unit of work | No (not in usage map) | Yes — `CreditsUsed` (`apikeys.go:172-174`) | **Yes — this is the real bill** |
| **Upstream input token** | `kiro.go:526,573-623` `updateTokensFromEvent` | Real prompt tokens from upstream `usage` | **Overridden** (see below) | Indirectly (if it survives) | No |
| **Upstream output token** | same `OnComplete(inTok,outTok)` | Real completion tokens | **Discarded** — replaced by estimate | No | No |
| **Estimated input token** | `token_estimator.go:49` `estimateClaudeRequestInputTokens` | Heuristic over request bytes | Only as last-resort fallback | Fallback | No |
| **Estimated output token** | `token_estimator.go:69` `estimateClaudeOutputTokens` | Heuristic over response text/thinking/tool JSON | **Yes — always** | Yes (token quota) | No |
| **Context occupancy** | `kiro.go:550-555` `contextUsageEvent.contextUsagePercentage` | % of context window in use | **Yes — misreported as `input_tokens`** | Yes (token quota) | No |
| **Prompt-cache create/read** | `cache_tracker.go` (local only) | Synthetic Anthropic-compat metadata | Yes (`cache_*` fields) | No | No |

**Formula (the core bug):**
```
realInputTokens = int(contextUsagePercentage * getContextWindowSize(model) / 100.0)
```
`getContextWindowSize` = `1_000_000` for Claude ≥ 4.6, else `200_000`
(`kiro.go:635-640`).

---

## 4. Exact accounting call graph

```
Kiro event-stream (parseEventStream, kiro.go:473)
  ├── assistantResponseEvent / reasoningContentEvent → OnText → response body text
  ├── usage{inputTokens,outputTokens} → updateTokensFromEvent → OnComplete(inTok,outTok)
  ├── meteringEvent.usage (float) → totalCredits += usage → OnCredits(c)     [REAL CREDIT]
  └── contextUsageEvent.contextUsagePercentage → OnContextUsage(pct)

Handler (handler.go handleClaudeStream/NonStream)
  realInputTokens = pct * window / 100                     (1302 / 1628 / 2103)
  if realInputTokens > 0:  inputTokens = realInputTokens   (1418-1419 / 1651-1652 / 2124-2125)   <-- OVERRIDE
  outputTokens = estimateClaudeOutputTokens(...)           (1431 / 1656 / 2137)                  <-- ESTIMATE
        │
        ├── client usage map  buildClaudeUsageMap(inputTokens, outputTokens, ...)  (1449 / 1688)
        ├── recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)   (1433 / 1664)
        │       ├── recordSuccess → h.totalTokens += in+out ; addCredits(credits)  (1524-1528)
        │       └── config.RecordApiKeyUsage(id, in+out, credits)                  (1539)
        │               ├── TokensUsed  += (in+out)   [context-derived tokens]     (apikeys.go:170)
        │               └── CreditsUsed += credits     [REAL credit]               (apikeys.go:173)
        └── pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)        (1435 / 1666)
```

**The credit path (right branch) is correct and independent of the token bug.**
The token path (`in+out`) carries the inflated context-derived number into
client usage, `totalTokens`, per-key `TokensUsed`, and account stats.

---

## 5. Verified code findings

### F1 — Context occupancy reported as request input tokens (H1, H2) — **Confirmed, High**
- **Where:** `handler.go:1302,1370,1418-1419` (stream), `1605,1651-1652` (non-stream),
  `2103,2124-2125` (OpenAI stream), `2220` (OpenAI); `responses_handler.go:170,472`.
- **Evidence:** `realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)`
  then `if realInputTokens > 0 { inputTokens = realInputTokens }`. The real upstream
  `inputTokens` captured in `OnComplete` is unconditionally replaced whenever a
  `contextUsageEvent` arrived (it normally does).
- **Reproduction:** `TestVerify_CaseA` — upstream real input = 1200, ctxPct = 4%,
  window = 1M → **client sees 40,000**. `TestVerify_CaseC` — real 500 vs ctx 10%
  → client sees 100,000 (context wins).
- **Impact:** Client-visible `input_tokens`, dashboard `totalTokens`, and per-key
  `TokensUsed` are the whole-context occupancy, not the request's real prompt size.
  A 4%-full 1M-context chat reports 40k "input tokens" on every turn regardless of
  how small the new message is. **This is the primary reason proxy "token" numbers
  dwarf the IDE.**

### F2 — Output tokens are always an estimate (H3) — **Confirmed, High**
- **Where:** `handler.go:1431,1656,2137,2244`; estimator `token_estimator.go:69-79`.
- **Evidence:** Even though `OnComplete(inTok, outTok)` captures a real upstream
  `outputTokens`, the handler overwrites it: `outputTokens = estimateClaudeOutputTokens(...)`.
- **Reproduction:** `TestVerify_OutputTokenIsEstimateNotUpstream` — upstream
  `outputTokens=9999`, estimator returns `11`, client receives the estimate.
- **Impact:** Output token counts are a byte-heuristic, not upstream truth. Diverges
  from IDE and from real billing.

### F3 — Credits are real and summed correctly (Q4) — **Confirmed, High confidence**
- **Where:** `kiro.go:546-549` (`meteringEvent.usage`), `kiro.go:563-565` (`OnCredits`),
  `kiro_conversation_runner.go:93,143,171` (sum across rounds).
- **Evidence:** Credits are read verbatim from upstream metering; no token→credit
  conversion exists anywhere. `TestVerify_CaseD` proves two metering events in one
  request sum (1.0 + 2.5 = 3.5).
- **Impact:** The billed unit is faithful. **Confirms the discrepancy you see is
  token-display, not credit consumption** — unless Q7/Q8 shift the real model/profile.

### F4 — Full history + system + tools re-serialized every request (H4) — **Confirmed, Medium**
- **Where:** `translator.go:226-279` (`ClaudeToKiro` loops all `req.Messages` into
  `history`), `:284-299` (system prompt prepended as synthetic history), `:334-364`
  (tools attached), `:375` (`truncatePayloadToLimit` only trims if over a byte cap).
- **Evidence:** No server-side session/context reuse; the whole conversation is
  replayed each turn. This is standard Anthropic Messages API behavior.
- **Impact:** Payload grows with history — but this raises **real credits** only if
  Kiro bills by processed input. Since Kiro bills per-prompt credits (§10 source),
  the effect on credit is model-dependent and **not** the 10-40× inflation seen in
  token display. It does inflate the *estimated* fallback and context occupancy.

### F5 — Prompt cache tracker is metadata-only (H9) — **Confirmed, High**
- **Where:** `cache_tracker.go` (entire file is local bookkeeping); no `cache_control`
  is ever sent to Kiro (grep for `cache_control`/`cacheControl` in payload build →
  none; only the local tracker and response `cache_*` fields exist).
- **Evidence:** `buildClaudeUsageMap` (`cache_tracker.go:513`) and
  `billedClaudeInputTokens` (`:509`) only shape the **response** usage object. The
  tracker records per-account prefix fingerprints + TTLs to synthesize
  `cache_creation_input_tokens` / `cache_read_input_tokens`.
- **Impact:** No real backend cache-hit discount. It stabilizes reported numbers and
  produces Anthropic-compatible metadata; it does **not** reduce real Kiro credits.

### F6 — Per-key token quota counts context occupancy; credit quota is real (H10) — **Confirmed, High**
- **Where:** `handler.go:1539` → `config.RecordApiKeyUsage(id, in+out, credits)`;
  `apikeys.go:170` `TokensUsed += (in+out)`, `:173` `CreditsUsed += credits`;
  gate `auth.go:73-78` / `apikeys.go:224-230`.
- **Evidence:** `in` is the context-derived `inputTokens` from F1. So a key with a
  `TokenLimit` is throttled against occupancy tokens, not real prompt tokens. A key
  with a `CreditLimit` is throttled against real credits.
- **Impact:** If you rely on `TokenLimit`, it trips far too early on long-context
  chats. `CreditLimit` is the accurate gate.

### F7 — Forwarded (upstream-route) requests skip Kiro accounting (H11) — **Confirmed, Medium**
- **Where:** `upstream_forward.go:109-115` — `recordSuccess(0,0,0)`; comment: "Token
  counts are not tracked for passthroughs. Per-API-key quota is intentionally left
  untouched."
- **Impact:** Forwarded requests add to the **global request count** only. They cannot
  inflate token/credit numbers, but a dashboard "requests" figure includes them.

---

## 6. Backend-call amplification (Q5)

Each Kiro round = exactly one inference call (`kiro_round.go:55-91`,
`kiro_conversation_runner.go:84`). Credits are summed per round (F3).

| Flow | Min Kiro calls | Max Kiro calls | Condition for extra calls |
|---|---|---|---|
| Plain chat (no web_search) | **1** | 1 | none (runner not engaged; `handler.go:914`) |
| Account retry on failure | 1 | up to `maxAccountRetryAttempts = 3` (`account_failover.go:10`) | only if account errors **and** stream not started (`handler.go:1321,1379`) |
| web_search, no search issued | 1 | 1 | model returns text directly |
| web_search, N search rounds | 2 | `MaxRounds` (default **4**) + 1 finalize (`config.go:1413`, runner `:79,137,165`) | model keeps requesting web_search |
| web_search budget hit | — | forced 1 finalize round (`runner:115-119`) | `SearchCalls > MaxSearches` (default 5) |

- **Web-search fallback SearXNG→Tavily is an HTTP search call, not a Kiro inference**
  (`search_orchestrator.go`, executor `websearch_executor.go`). It does not add Kiro
  credits; Tavily credits are tracked separately (`KiroRunResult.TavilyCredits`).
- **Token refresh is not an inference** (`token_manager.go`, `kiro_api.go` refresh
  path) — does not meter credits.
- Verified by existing tests: `TestRunnerNoSearchSingleRound` (1),
  `TestRunnerSingleSearchThenFinal` (2), `TestRunnerMaxRoundsForcesFinalization`,
  `TestNonStreamRunnerProviderErrorDoesNotFailAccount` (1, no retry).

**Verdict on H5:** For an ordinary chat the proxy makes **one** Kiro call, same as a
single IDE turn. Amplification only occurs on the web_search agentic loop, which the
IDE would also incur for equivalent agentic behavior.

---

## 7. Retry / double-metering (H12)

- Account retry (`handler.go:972,1554` loop over `maxAccountRetryAttempts`) re-issues
  the request on a **different** account only when the first account **errors**.
- On the stream path, retry is gated by `if !messageStarted { continue }`
  (`handler.go:1321,1379`) — once bytes have streamed, it does **not** retry; it
  records a failure and stops.
- A metered credit is only booked on a **successful** round (`OnCredits` fires after
  a clean parse; `recordSuccessForApiKey` runs only on the success branch).
- **Residual risk (not disproved):** if the upstream emitted a `meteringEvent` and
  then the connection failed *before* completion on account A, account A may have been
  billed by Kiro while the proxy retries on account B (which bills again). The proxy
  cannot un-bill account A. This is a genuine but narrow double-billing window; it was
  **not reproduced** here (would require a live upstream that meters mid-stream then
  fails). **Verdict: Partially confirmed / not fully verifiable without live backend.**

---

## 8. Profile / model comparison

### Model mapping (Q7) — evidence `translator.go:24-124`

| Requested model | Actual Kiro `modelId` | Evidence |
|---|---|---|
| `gpt-4o`, `gpt-4`, `gpt-4-turbo`, `gpt-3.5-turbo` | `claude-sonnet-4.5` | `translator.go:30-33` |
| `claude-3-5-sonnet`, `claude-3-opus` | `claude-sonnet-4.5` | `translator.go:26-27` |
| `claude-3-sonnet` | `claude-sonnet-4` | `translator.go:28` |
| `claude-3-haiku` | `claude-haiku-4.5` | `translator.go:29` |
| `claude-opus-4-8` | `claude-opus-4.8` (regex normalize) | `translator.go:39,96-97` |
| `claude-*` (dotted) | passthrough unchanged | `translator.go:101-102` |
| empty / unknown | **verbatim, NO default** in main chat handlers | `translator.go:105` |

- Default only injected on the self-test endpoint (`handler.go:4280` → `claude-sonnet-4`)
  and `/responses` (`responses_handler.go:13` → `claude-sonnet-4.5`), not `/v1/messages`.

### Thinking / effort (Q7b) — evidence `translator.go:42-43,77-119,380-390`

- **Off by default.** Enabled only by (a) model suffix `-thinking`
  (`config.go:1284-1287`), or (b) native Anthropic `thinking.type ∈ {enabled,adaptive}`
  (`translator.go:113-119`).
- `<max_thinking_length>200000</max_thinking_length>` (`translator.go:42-43`) is injected
  **only when thinking is true** (`buildClaudeSystemPrompt:380-390`,
  `OpenAIToKiro:1143-1146`) — never unconditionally.
- Thinking output **is** counted into the output-token estimate when thinking is on
  (`token_estimator.go:71`; zeroed when off at `handler.go:1428-1429` etc).

**Verdict H7:** Disproved for defaults. If your client (Claude Code) *does* send
`thinking`, that is a client choice, not a proxy default.

### Profile / region (Q8) — evidence `kiro_api.go:31,63-73,266-305,321-344,482-487`

- Probe order: **`us-east-1` first, then `eu-central-1`** (`kiro_api.go:31`), starting
  from `account.EffectiveApiRegion()` else `us-east-1` (`:321-328`).
- **Stops at the first region that returns any profile** and takes `Profiles[0]`
  (`:266-280`, `:482-487`), then persists `ProfileArn`+`ApiRegion`
  (`config.go:942-955`).
- **No multi-profile picker exists** (grep for `pickProfile|selectProfile|profileIndex`
  → none). Only lever: set the account's `apiRegion`/`region` *before* first resolution.
- Model discovery, plan/usage lookup, and inference all read the **same**
  `account.ProfileArn` (`kiro_api.go:133-208`, `kiro.go:360-405`), and
  `regionFromProfileArn` makes the ARN authoritative for the data-plane region
  (`kiro_api.go:63-73`).
- `token_manager.go:203,230-232` persists a refresh-sourced ARN (ARN only, no region).

**Verdict H8:** The code **can** pin US while your IDE uses EU Power, because it probes
US first and stops. Whether your running instance is actually pinned to US is
**Chưa xác minh** — I did not read the credential/data store. Operator check:

```bash
grep -oE '"(profileArn|apiRegion|region|authRegion)"[^,}]*' data/*.json \
  | sed -E 's#(codewhisperer:[a-z0-9-]+:)[0-9]+#\1<REDACTED>#g'
```
`:eu-central-1:` = EU; `:us-east-1:`/absent = US.

---

## 9. IDE / CLI comparison

**Not measured.** I could not drive the Kiro IDE/CLI against the same account in this
environment. No fabricated numbers. Use the protocol in §12 to gather an
apples-to-apples matrix. What the code *predicts*:

- The proxy's reported "input tokens" will look 10-40× the IDE's per-message token
  view **because it reports context occupancy, not the new message** (F1). This alone
  explains a large apparent gap with **zero** extra real credit.
- Plain-chat real Kiro calls: 1 (proxy) vs 1 (IDE turn) — parity (§6).
- Real credit parity depends on same model + same effort + same profile (Q7/Q8).

---

## 10. Root-cause ranking (evidence-based, no invented probabilities)

1. **Accounting/reporting bug — context occupancy misreported as request input tokens
   (F1).** This is the biggest and certain contributor to the *perceived* gap. Real
   credits unaffected.
2. **Output tokens are estimates, not upstream truth (F2).** Amplifies display gap.
3. **Credit-vs-token conflation by the observer.** Kiro bills **credits**
   (official source §"A credit is a unit of work in response to user prompts");
   comparing proxy "tokens" to IDE "credits" compares different units.
4. **Model/effort mismatch (Q7)** — *possible* real-credit effect: if the IDE runs
   "Auto" (which may pick a cheaper model) while the proxy pins `claude-sonnet-4.5`/
   opus, real credit rates differ. Conditional, not confirmed for your runtime.
5. **Wrong profile (Q8)** — *possible* real-credit/plan effect: US pin vs EU Power.
   Not verifiable here.
6. **Web_search multi-round amplification (Q5)** — real extra calls, but only on the
   agentic search path, bounded by `MaxRounds`.
7. **Retry double-metering (H12)** — narrow, unproven window.
8. **Full-history replay (F4)** — expected API behavior; not a credit-inflation source
   given credit-per-prompt billing.

---

## 11. Test / instrumentation used

- **Isolated worktree** `git worktree add -d /tmp/kiro-verify 3864c21` (clean HEAD),
  so the concurrent broken `responses_*.go` WIP in the main tree did not block builds.
- **Temporary harness** `proxy/verify_accounting_test.go` (in the throwaway worktree
  only — never added to the main tree). It feeds synthetic AWS event-stream frames
  through the **real** `parseEventStream` and applies the handler's exact override
  arithmetic. Results:

```
TestVerify_CaseA_ContextOverridesRealInput  PASS  upstream in=1200 ctx=4%/1M -> client 40000
TestVerify_CaseB_NoUpstreamTokensContextOnly PASS  ctx=4%/200K -> client 8000
TestVerify_CaseC_ConflictContextWins         PASS  real 500 vs ctx 10% -> client 100000
TestVerify_CaseD_TwoMeteringEventsSum         PASS  1.0 + 2.5 -> 3.5 credits
TestVerify_OutputTokenIsEstimateNotUpstream   PASS  upstream out=9999 -> estimator 11
```

- **Baseline** at clean HEAD `3864c21`: `go vet ./...` clean; `go test ./...`
  all green (`auth`, `config`, `pool`, `proxy`); `go test -race ./proxy/` reports
  **0 DATA RACE** (one flaky `TempDir` cleanup error under parallel tests, passes in
  isolation).
- The worktree will be removed as part of cleanup; nothing is committed.

---

## 12. Minimal fix plan (proposals only — nothing implemented)

### 12a. Accounting-only fixes (no behavior/backend change)

**FIX-1 — Stop overriding real input tokens with context occupancy.**
- Files: `proxy/handler.go` (1418-1419, 1651-1652, 2124-2125, 2220),
  `proxy/responses_handler.go` (170, 472).
- Change: prefer real upstream `inputTokens` when present; use context-derived value
  only as a fallback (or expose it as a **separate** field, not `input_tokens`).
- Real-credit impact: none. Fixes client/dashboard/quota token numbers.
- Test: extend the harness Cases A/C to assert real 1200/500 survive.

**FIX-2 — Use upstream output tokens when reported; estimate only as fallback.**
- Files: `proxy/handler.go` (1431, 1656, 2137, 2244).
- Real-credit impact: none. Test: Case-E-style assertion that upstream 9999 survives.

**FIX-3 — Separate token vs occupancy in quota.** Count real tokens (or credits) in
`TokensUsed`; keep occupancy for compaction hints only.
- Files: `proxy/handler.go:1539`, `config/apikeys.go`. Real-credit impact: none.

### 12b. Behavior / performance fixes (need benchmark + regression tests)

**FIX-4 — Multi-profile picker / EU-first preference.** Let the operator pin
`eu-central-1`/Power explicitly instead of US-first auto-probe.
- Files: `proxy/kiro_api.go` (probe order), `config/config.go` (account field), admin UI.
- Real-credit impact: **yes** — changes which plan/region bills. Compatibility risk:
  medium. Test: probe-order unit tests with a fake ListProfiles.

### 12c. Product decisions

- **Model default/mapping:** decide whether `gpt-*`→`claude-sonnet-4.5` and the
  opus defaults match your intended cost tier vs IDE "Auto".
- **Effort default:** confirm clients aren't sending `thinking` you don't want.
- **Retry semantics (H12):** consider idempotency/metering-dedup if live testing
  shows mid-stream metered failures.

---

## 13. Completion checks

- No production code modified in the main tree (only in the disposable worktree, not committed).
- Kiro bills **credits**, not tokens (official source below) — the token display gap
  is not a credit gap.
- Metrics kept distinct: context occupancy ≠ token count ≠ credit ≠ request count ≠
  cache metadata.

### Source

- **Kiro pricing / credits** — https://kiro.dev/pricing/ (accessed 2026-07-12).
  Confirms: "A credit is a unit of work in response to user prompts"; billing is
  credit-based (metered to 0.01), **not** per-token or per-request; plans Free(50)/
  Pro(1,000)/Pro+(2,000)/Pro Max(5,000)/Power(10,000) credits; add-on $0.04/credit.
  Does **not** confirm any token→credit formula (none exists to cite). Model-specific
  credit *rates* are stated to differ but exact multipliers were not enumerated on the
  page (inference: opus > sonnet). The `kiro.dev/docs/reference/usage/` and
  `/billing/` deep-links returned 404 on 2026-07-12.
