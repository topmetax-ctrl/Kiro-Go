# Credit Optimization — Implementation Plan

- **Revision:** rev1 (2026-07-12)
- **Target SHA:** `ced9eec`
- **Status:** PLAN ONLY. No production code changed in this audit. Each item below is gated on the stated evidence bar from the task's §13.

This plan separates the two goals the task demands be kept apart:
- **Fix statistics** without touching payload or credit → **Phase A** (safe, quality-neutral).
- **Reduce real credit** only when an A/B proves it → **Phase B/C** (needs measurement / operator approval).

---

## Phase A — Accounting-only (safe: no payload change, no credit change, no inference change)

Invariant every Phase-A change must satisfy:
```
Kiro request payload unchanged (byte/semantic hash identical)
→ Kiro inference count unchanged
→ Metered credit unchanged
→ Response content / tool events / SSE ordering unchanged
→ Internal token stats become MORE accurate
```

### A1 — Stop context occupancy overriding input tokens (requires separating the shared variable first)

**Problem (proven, F1):** 6 finalize tails do `if realInputTokens > 0 { inputTokens = realInputTokens }`, replacing real upstream input with `contextPct × window`.

**Blocking prerequisite (from cross-check):** today a single `inputTokens` variable feeds THREE sinks at every tail. Confirmed in the Claude-stream tail:
- `handler.go:1604` → `recordSuccessForApiKey(...)` → `RecordApiKeyUsage` (per-key `TokensUsed`)
- `handler.go:1606` → `pool.UpdateStats(...)` (account `TotalTokens`)
- `handler.go:1620` → `buildClaudeUsageMap(inputTokens, ...)` (client-facing SSE `usage`)

Because all three read the same variable, **A1 and A3 cannot both hold with the current structure**: changing precedence changes internal stats AND the client number together. "Internal accurate, client legacy" is impossible until the variable is split. This was a real contradiction in rev1 of this plan.

**Fix, step 1 — introduce distinct variables at each tail (no behavior change yet):**
```go
upstreamInputTokens    // from OnComplete; 0 if upstream reported nothing
estimatedInputTokens   // request-side byte estimate (already computed)
contextOccupancyTokens // contextPct × window — a DISPLAY metric, never an accounting input

accountedInputTokens   // what feeds RecordApiKeyUsage + UpdateStats
clientReportedInputTokens // what feeds buildClaudeUsageMap
```

**Fix, step 2 — accounting precedence (internal sinks only):**
```go
accountedInputTokens = upstreamInputTokens
if accountedInputTokens <= 0 {
    accountedInputTokens = estimatedInputTokens   // fallback only
}
// contextOccupancyTokens is NEVER assigned into accountedInputTokens
recordSuccessForApiKey(apiKeyID, accountedInputTokens, accountedOutputTokens, credits)
pool.UpdateStats(account.ID, accountedInputTokens+accountedOutputTokens, credits)
```

**Fix, step 3 — client precedence (gated by A3 mode):**
```go
switch reportingMode {
case "legacy":   clientReportedInputTokens = contextOccupancyTokens // current behavior, unchanged
case "accurate": clientReportedInputTokens = accountedInputTokens
}
buildClaudeUsageMap(clientReportedInputTokens, clientReportedOutputTokens, ...)
```

Sites to change: `handler.go:1589-1592`, `handler.go:1843-1846`, `handler.go:2316-2319`, `handler.go:2431-2434`, `responses_handler.go:210-213`, `responses_handler.go:555-558` (plus the matching `buildClaudeUsageMap`/response-encode call in each tail).

Special case `handler.go:1843` (non-stream runner): must **not** discard `run.TotalInputTokens` (computed at 1793). Guard the override behind `!usedRunner`, mirroring the stream path's `usedRunner` flag.

**Credit impact:** none. **Payload impact:** none. **Quality impact:** none. **Reporting impact:** with default `legacy` mode, the client sees NO change on day one; only internal `TokensUsed`/`TotalTokens` become upstream-accurate. The client number moves only when an operator opts into `accurate`.

### A2 — Prefer upstream output tokens, estimator only as fallback

**Problem (proven, F2):** every tail does `outputTokens = estimate*(...)`, discarding upstream `OnComplete` output.

**Minimal fix (same split as A1):**
```go
// upstreamOutputTokens = value from OnComplete (0 if upstream sent none)
accountedOutputTokens := upstreamOutputTokens
if accountedOutputTokens <= 0 {
    accountedOutputTokens = estimate*(...)   // fallback only
}
// legacy client keeps the estimator value; accurate mode uses accountedOutputTokens
clientReportedOutputTokens := estimatedOutputTokens // legacy default
```

**Impact:** internal output-token stats become upstream-accurate when upstream reports it; under `legacy` the client sees no change; credit/payload/quality unchanged.

### A3 — Client-compatibility mode (do NOT change client-facing usage silently)

Add a reporting mode so existing clients that depend on the current (inflated) `usage.input_tokens` are not broken by A1:
- `legacy` (default): keep current client-facing behavior.
- `accurate`: emit upstream/estimate.
- `dual`: keep legacy client field, expose accurate values + context occupancy on admin/stats only.

Default must remain `legacy` until client-compaction behavior is tested (§18 stop condition: an accounting fix must not change client compaction).

**Note:** A3 is only implementable *because* A1/A2 split the accounted variable from the client-reported one. At `ced9eec` a single `inputTokens` variable feeds `recordSuccessForApiKey` (1604), `pool.UpdateStats` (1606) **and** `buildClaudeUsageMap` (1620) — so changing precedence on that one variable would move internal stats and the client number together. "Internal accurate, client legacy" is impossible without the variable split; that split is a prerequisite of A1, not an optional extra.

---

## Phase B — Verified-duplicate bug fixes (reduce real credit, keep semantics)

**None qualify at `ced9eec`.** The payload-equivalence audit (full-audit §5) found no duplicate system prompt, no duplicated history turn, no double tool-result replay, and retry re-sends the original payload on a different account (no double metering). **Do not create commits 7/8 from the task's list** — the bugs they target are not present.

If a future duplicate is found, each fix must prove: semantic payload keeps all context, response quality unchanged, metered credit or call-count drops, plus a regression test.

---

## Phase C — Product optimization (NOT auto-implemented; operator approval required)

Per task §13-C, these trade quality/behavior and must be proposed separately, never shipped as "no-impact":
- Pin a cheaper model / rely on Auto (the documented 1.3× lever).
- Reduce web-search `MaxRounds`/`MaxSearches`.
- History summarization / context compression.
- Reduce reasoning effort.

Each needs an operator-approved A/B (Phase §12) proving `meteringEvent.usage` drops under identical account/model/profile/effort/tools/context/rounds.

---

## Commit strategy (only the proven ones)

| # | Commit | Gate | Ship now? |
|---|---|---|---|
| 1 | `test: add usage accounting verification harness` | tests pass | **Yes** (already written: `credit_audit_test.go`) |
| 2 | `test: add end-to-end usage/quota/credit integration tests for all 7 tails` | see prerequisite below | **Blocker for A1/A2/A3** |
| 3 | `docs: add credit usage audit` | — | **Yes** (this audit) |
| 4 | `fix: separate context occupancy from internal token accounting` (A1) | commit #2 exists + green; payload hash identical, credit unchanged | **Only after commit #2** |
| 5 | `fix: prefer upstream output tokens with estimator fallback` (A2) | same | **Only after commit #2** |
| 6 | `feat: add legacy/accurate/dual usage reporting modes` (A3) | commit #2 green; default=legacy | **Only after commit #2** |
| 7 | `fix: remove verified duplicate payload behavior` | **NO bug found** | **Skip** |
| 8 | `fix: prevent verified duplicate metering retries` | **NO bug found** | **Skip** |

### Prerequisite for A1/A2/A3: end-to-end integration tests (commit #2)

The current harness (`credit_audit_test.go`) proves the decoder (`parseEventStream`, real) and the finalize arithmetic (**re-simulated**, not driven through the live HTTP handler). That is enough to *confirm the bug*, but not enough to *safely change* it: the token variable that feeds the client `usage` map is the **same** `inputTokens` that feeds `recordSuccessForApiKey` / `pool.UpdateStats` (proven: `handler.go:1604`, `:1606`, `:1620` all read one variable). A1's variable split therefore touches the client-visible path and MUST be covered end-to-end before it ships.

The seam already exists in the test suite — no new production hooks needed:
- `swapKiroEndpointsForTest(t, server)` (`responses_handler_test.go:290`) swaps the package-level `kiroEndpoints` to an `httptest.Server`.
- `kiroHttpStore.Store(&http.Client{...})` (`handler_test.go:83-85`) points the real client at it.

Commit #2 must drive a real request through each of the **7 tails** — Claude stream, Claude non-stream, Claude web-search runner, OpenAI stream, OpenAI non-stream, Responses stream, Responses non-stream — against a fake Kiro server emitting a scripted stream (upstream tokens + context% + metering), and assert **all four sinks at once**: client `usage`, per-key `TokensUsed` delta, account `TotalTokens` delta, and credit delta. Only against these fixtures can A1/A2 claim "internal accurate, client unchanged under `legacy`" with proof rather than by re-simulation.

---

## Rollback

Phase A changes are pure accounting reads at the finalize tails; rollback = revert the commit. No data migration, no payload/credit state to unwind. A3 default `legacy` means even a bad `accurate` rollout is a one-flag revert.
