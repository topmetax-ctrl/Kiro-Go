# Credit A/B Test Plan

- **Revision:** rev1 (2026-07-12)
- **Target SHA:** `ced9eec`
- **Status:** **PLAN ONLY — NO LIVE A/B WAS RUN.** Live runs consume real credits and require explicit operator approval + a credit budget (§12). Neither was granted in this audit.

## Hard preconditions before any live run (§12)

A live A/B may run ONLY when every item below is true:

- [ ] Staging fully isolated: separate data dir (`/tmp/kiro-credit-audit-data`), separate config file, separate response store, separate prompt-tracker file.
- [ ] No background refresh worker touching the same live account concurrently.
- [ ] Loopback bind only (`127.0.0.1:18081` or a free high port), never `0.0.0.0`.
- [ ] Operator has approved a specific max-credit budget.
- [ ] Same account, same **pinned** profile, same model, same effort/thinking, same tools, same request bodies for A and B.
- [ ] No production traffic on the instance.

Before running, print: expected request count, max inference count, max credit budget, profile (redacted ARN), model, thinking flag, tool/search settings. Then STOP for approval.

## The critical design rule

The primary finding (context-occupancy overriding input tokens) is an **accounting-only** bug. The correct fix is **byte-for-byte payload-identical**: it must NOT change the Kiro request, inference count, or metered credits. That means the strongest evidence for the accounting fix is **not** an A/B credit comparison at all — it is the **payload-equivalence + metering-invariance** test (offline, already expressible with the fake backend). A live credit A/B for an accounting-only change would show **zero credit delta by design**; a non-zero delta would mean the "accounting-only" fix accidentally changed the payload — a regression, not a win.

So the A/B matrix below is for **Phase B** candidates (proven duplicate/no-op removals) and **Phase C** product options (model/effort/history — operator-approved only), NOT for the Phase A accounting fix.

## Invariant to assert offline (no credits spent)

```
Accounting-only fix
  → semantic payload hash unchanged
  → Kiro inference count unchanged
  → summed metered credits unchanged
  → response content / tool events / SSE ordering unchanged
  → internal token stats become accurate (context occupancy no longer counted as input)
```

## A/B matrix (Phase B / C only, when approved)

Baseline A = `ced9eec`. Candidate B changes exactly one factor. Each pair: identical request body, identical semantic payload hash (for Phase B), same profile/model/effort, fresh conversation, no injected retry (except the retry-specific test).

| # | Scenario | What it isolates |
|---|---|---|
| 1 | One-shot text | floor cost |
| 2 | 10-turn history | history serialization cost |
| 3 | Large system prompt | priming cost |
| 4 | Large tool schemas, no tool call | schema payload cost |
| 5 | One tool call | single round-trip |
| 6 | Multi tool rounds | per-round credit |
| 7 | Web search | runner round count |
| 8 | Thinking on/off (product) | effort credit delta |
| 9 | US vs EU profile (product) | profile/plan credit rate |
| 10 | Injected retry | double-metering check |

Per test, record: payload bytes, Kiro calls, metered credit, upstream tokens, context occupancy, client usage, account/api-key stat deltas, stop reason, tool-call structure, response length, error/retry.

## Quality-preservation definition

Quality is "unchanged" when: same model/profile/effort, same semantic payload, same tools, same stop-reason class, valid tool calls, no lost instruction/context, no increased error/overflow. Do NOT compare exact text between two stochastic model calls.

## Results

`NOT RUN` — pending operator approval and credit budget.
