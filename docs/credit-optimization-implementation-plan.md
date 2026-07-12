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

### A1 — Stop context occupancy overriding input tokens

**Problem (proven, F1):** 6 finalize tails do `if realInputTokens > 0 { inputTokens = realInputTokens }`, replacing real upstream input with `contextPct × window`.

**Minimal fix:** apply the already-correct precedence the Claude-stream+runner tail uses — **upstream first, estimator fallback, never context occupancy** — to the other 5 sites:

```go
// desired precedence at every tail:
if inputTokens <= 0 {          // upstream OnComplete gave nothing
    inputTokens = estimatedInputTokens
}
// context occupancy is NEVER assigned into inputTokens; keep it as a separate metric
```

Sites to change: `handler.go:1589-1592`, `handler.go:1843-1846`, `handler.go:2316-2319`, `handler.go:2431-2434`, `responses_handler.go:210-213`, `responses_handler.go:555-558`.

Special case `handler.go:1843` (non-stream runner): must **not** discard `run.TotalInputTokens` (computed at 1793). Guard the override behind `!usedRunner`, mirroring the stream path's `usedRunner` flag.

**Credit impact:** none. **Payload impact:** none. **Quality impact:** none. **Reporting impact:** input-token display becomes upstream-accurate.

### A2 — Prefer upstream output tokens, estimator only as fallback

**Problem (proven, F2):** every tail does `outputTokens = estimate*(...)`, discarding upstream `OnComplete` output.

**Minimal fix:**
```go
if outputTokens <= 0 {
    outputTokens = estimate*(...)   // fallback only
}
```

**Impact:** output-token display becomes upstream-accurate when upstream reports it; credit/payload/quality unchanged.

### A3 — Client-compatibility mode (do NOT change client-facing usage silently)

Add a reporting mode so existing clients that depend on the current (inflated) `usage.input_tokens` are not broken by A1:
- `legacy` (default): keep current client-facing behavior.
- `accurate`: emit upstream/estimate.
- `dual`: keep legacy client field, expose accurate values + context occupancy on admin/stats only.

Default must remain `legacy` until client-compaction behavior is tested (§18 stop condition: an accounting fix must not change client compaction).

**Note:** A1/A2 as written above fix **internal** stats (`TokensUsed`, `TotalTokens`, dashboards). The **client-facing** map is built by `buildClaudeUsageMap`; changing it is gated behind A3's mode flag.

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
| 3 | `docs: add credit usage audit` | — | **Yes** (this audit) |
| 4 | `fix: separate context occupancy from internal token accounting` (A1) | payload hash identical, credit unchanged | Yes, after review |
| 5 | `fix: prefer upstream output tokens with estimator fallback` (A2) | same | Yes, after review |
| 6 | `feat: add legacy/accurate/dual usage reporting modes` (A3) | default=legacy | Yes, after review |
| 7 | `fix: remove verified duplicate payload behavior` | **NO bug found** | **Skip** |
| 8 | `fix: prevent verified duplicate metering retries` | **NO bug found** | **Skip** |

---

## Rollback

Phase A changes are pure accounting reads at the finalize tails; rollback = revert the commit. No data migration, no payload/credit state to unwind. A3 default `legacy` means even a bad `accurate` rollout is a one-flag revert.
