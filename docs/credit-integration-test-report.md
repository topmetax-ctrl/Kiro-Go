# Usage Accounting — End-to-End Integration Test Report

- **Revision:** rev1 (2026-07-12)
- **Source SHA:** `3769496` (branch `audit/credit-usage-correctness`, cut from `ced9eec`)
- **Scope:** test-only. NO production `.go` file changed. NO shadow instrumentation. NO live Kiro call. NO real credential. NO merge/deploy.
- **New files:** `proxy/usage_accounting_integration_test.go`, `proxy/usage_accounting_test_helpers_test.go`.

## Purpose

Build the safety net the implementation plan requires *before* any Phase-A change:
characterize — through the **real HTTP handlers** against a **fake Kiro backend** — exactly
which usage sink receives which number on every logical path. These are
**characterization tests**: they assert current behavior, *including* the known bug where
context occupancy overrides the real upstream input token count. When Phase A lands, the
internal-accounting and client expectations change on purpose; the credit / payload /
response / SSE-ordering invariants must not.

## Method

- **6 direct paths** drive a request through `Handler.ServeHTTP` (auth middleware included:
  `RequireApiKey=true`, a real API key in the `Authorization` header), then into the handler,
  then to a fake Kiro `httptest.Server` via the existing `swapKiroEndpointsForTest` +
  `kiroHttpStore` seam. No production hook was added.
- **2 Claude web_search runner paths** inject a `fakeConversationRunner` into the `Handler`
  (the existing injection seam) and call the handler method directly with an explicit
  `apiKeyID`. Engaging the runner through `ServeHTTP` would require live web_search config
  (SearXNG/Tavily); the direct handler call exercises the exact finalize tail under test
  without that dependency. This is the one place a full end-to-end HTTP drive was not used,
  and it is called out here rather than hidden.
- Every test captures **all sinks at once** as before→after deltas: client-visible usage,
  per-API-key counters, per-account counters, global handler counters, upstream call count,
  captured request payloads, and (streaming) ordered SSE events.
- Isolation: each test gets its own `t.TempDir()` config, its own account ID and API-key ID,
  a fresh `Handler`, and restores `kiroEndpoints`/`kiroHttpStore` via `t.Cleanup`. No
  `t.Parallel()` (shared package globals). Verified with `-count=20`, `-shuffle=on`, `-race`.

## Canonical scripted upstream (Case A)

`upstream input = 1200`, `upstream output = 300`, `contextUsagePercentage = 4`,
model = `claude-sonnet-4.6` (1M window ⇒ 4% = **40,000**), `metering usage = 1.25`.
Short visible text (`"Hi."`) so the output estimator is unmistakably ≠ 300.

## Results — the 8 paths

| # | Path | Client input | Client output | Per-key tok/cred Δ | Account tok/cred Δ | Global tok/cred Δ | Kiro calls | Credit correct? |
|---|------|-------------:|--------------:|-------------------|--------------------|--------------------|:---------:|:---------------:|
| 1 | Claude stream, direct | **40,000** (context) | estimator (≠300) | 40,00x / 1.25 | same / 1.25 | same / 1.25 | 1 | ✅ |
| 2 | Claude stream, runner | **1,200** (run total) | estimator | 1,20x / 1.25 | same / 1.25 | same / 1.25 | (runner) | ✅ |
| 3 | Claude non-stream, direct | **40,000** (context) | estimator (≠300) | 40,00x / 1.25 | same / 1.25 | same / 1.25 | 1 | ✅ |
| 4 | Claude non-stream, runner | **40,000** (context, discards run total 1,200) | estimator | ≥40,000 / 1.25 | same / 1.25 | same / 1.25 | (runner) | ✅ |
| 5 | OpenAI stream, direct | **40,000** (context) | estimator | 40,00x / 1.25 | same / 1.25 | same / 1.25 | 1 | ✅ |
| 6 | OpenAI non-stream, direct | **40,000** (context) | estimator | 40,00x / 1.25 | same / 1.25 | same / 1.25 | 1 | ✅ |
| 7 | Responses stream, direct | **40,000** (context) | estimator | 40,00x / 1.25 | same / 1.25 | same / 1.25 | 1 | ✅ |
| 8 | Responses non-stream, direct | **40,000** (context) | estimator | 40,00x / 1.25 | same / 1.25 | same / 1.25 | 1 | ✅ |

Key: "same" = account/global token delta equals the per-key delta (the same inflated
`input+output` sum flows to every internal sink). "estimator" = client output is the local
heuristic, never the upstream 300.

### The special case (path 4) — proven

Path 4 (Claude non-stream + runner) is the dangerous one the implementation plan flagged:
the runner computes the correct multi-round `run.TotalInputTokens = 1,200`, then the
finalize tail overwrites it with the final round's context occupancy (40,000). Path 2
(the *stream* runner) is the only tail that preserves the run total. The two runner paths
diverging on identical inputs is exactly why the plan splits Claude non-stream into two
test cases, not one.

## Cross-cutting cases

| Case | What it pins | Result |
|------|--------------|--------|
| B — no upstream tokens | input falls back to estimator (not context, none present) | ✅ estimator used |
| C — two metering events (1.0 + 2.5) | credits sum to exactly 3.5, no double count | ✅ 3.5 |
| Context = 0 | `realInputTokens` stays 0 ⇒ upstream input (1,200) survives | ✅ 1,200 |
| Output = 9999 upstream | tail emits estimator, discards 9999 | ✅ estimator |

## Invariants captured for Phase A

Each test records the values a Phase-A accounting fix must **not** change:
- **Credit delta** = 1.25 on every path (per-key, account, global all agree).
- **Upstream Kiro calls** = 1 on every direct path.
- **SSE framing** present and parseable on all three streaming paths (Claude
  `message_delta.usage`, OpenAI `data`-chunk `usage`, Responses `response.completed`).
- **Captured request payload** available via `semanticPayloadHash` (strips only
  `agentContinuationId`/`conversationId`; never system prompt, history, tools, model,
  profile) so a later diff can prove the payload is byte/semantic-identical.

After Phase A, only the input/output token expectations (client + internal) should move;
credit, payload hash, upstream-call count, response text, and SSE order must be unchanged.

## Validation

```
go vet ./...                         # clean
go build ./...                       # clean
gofmt -w proxy/*_test.go             # clean
git diff --check                     # clean
go test ./proxy -run 'UsageIntegration' -count=20        # ok (85.9s)
go test ./proxy -run 'UsageIntegration' -shuffle=on -count=10   # ok (43.4s)
go test -race ./proxy -run 'UsageIntegration' -count=5   # ok (22.9s)
go test ./...                        # ok (full suite green)
```

No flakiness, no data race, no ordering dependence.

## What this proves — and what it does NOT

**Proves (executable, end-to-end):**
- All 6 direct tails report context occupancy as input tokens; both the client response
  and every internal counter (per-key, per-account, global) receive the inflated number.
- The Claude stream runner tail is fixed; the Claude non-stream runner tail is not
  (discards the correct run total).
- Output is always the estimator on every path.
- Credits come solely from `meteringEvent.usage`, sum correctly, hit all three sinks
  identically, and are independent of the token bug.

**Does NOT prove (unchanged from prior audit):**
- Whether Kiro's **backend** bills an interrupted attempt (not proxy-observable).
- Any credit-delta between IDE-Auto and proxy-pinned-model (no live A/B).
- That Profile Power reduces credit-per-request (documented multiplier is per-model).

## Next step (not done here)

Phase A remains gated. With this net in place the safe sequence is:
1. split `accountedInputTokens` from `clientReportedInputTokens` (plan A1);
2. prefer upstream tokens for internal accounting, estimator fallback (A1/A2);
3. keep client-facing usage on `legacy` default (A3);
4. re-run these tests — only the internal-token expectations flip; every invariant above holds.
