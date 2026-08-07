# Phase A — Usage Accounting Correctness (Implementation Report)

- **Revision:** rev1 (2026-07-13)
- **Branch:** `fix/usage-accounting-phase-a` (cut from `audit/credit-usage-correctness` @ `91a4073`)
- **Scope:** production code change. Splits internal token accounting (now upstream-accurate) from client-visible usage (legacy by default). Credit path, Kiro payload, response content, inference count, and SSE ordering are unchanged by construction.

## What changed

### 1. `config.GetUsageReportingMode()` (config/config.go)

New env-driven getter (`KIRO_USAGE_REPORTING`), values `legacy` (default) or `accurate`. Anything else, including unset, means `legacy`. Env-driven rather than a config-schema field so enabling accurate client reporting is a single operator toggle with trivial rollback (unset the var) and no persisted state. `dual` from the pushed plan is intentionally **not** implemented: without an admin-stats surface it would be a no-op alias of `legacy` (a half-feature). It can be added when there is a stats surface to carry the second number.

### 2. `usageSplit()` helper (proxy/handler.go)

One pure function computes the two numbers a tail needs:

```
accounted = upstream  (if > 0)  else estimated      // internal sinks; NEVER context occupancy
client    = legacyClient                             // legacy mode (default): the tail's historical value, verbatim
client    = accounted                                // accurate mode: matches the internal number
```

`legacyClient` is the exact value each tail reported before Phase A — for 6 of 7 tails that is context occupancy, preserved byte-for-byte in legacy mode so no client that keys off the old number breaks.

### 3. All 7 finalize tails rewired (proxy/handler.go, proxy/responses_handler.go)

Every tail now:
- captures the raw upstream token count **before** any override (`upstreamInput := inputTokens`);
- reproduces the old client number as `legacyInput` using the original branch logic;
- calls `usageSplit` for input and output;
- feeds **accounted** to `recordSuccessForApiKey` + `pool.UpdateStats` (internal sinks);
- feeds **client** to the response usage map / `buildResponsesObject` / `buildClaudeUsageMap` / `billedClaudeInputTokens`.

Tails: Claude stream (already-fixed runner branch + non-runner), Claude non-stream (incl. the special-case runner sub-path that previously discarded `run.TotalInputTokens`), OpenAI stream, OpenAI non-stream, Responses stream, Responses non-stream.

Output accounting also improved: internal sinks now record the **upstream** output count (upstream-first, estimator fallback) instead of always estimating.

## Invariants held (proven by tests)

| Invariant | How verified |
|---|---|
| Kiro payload byte/semantic identical | translator/handler untouched; only post-response accounting changed |
| Inference count unchanged | `assertOneUpstreamCall`; runner call count unchanged |
| Metered credit unchanged | `assertCreditDelta` (per-key/account/global) = `meteringEvent.usage` on every path |
| Response content / tool events unchanged | response text/stop-reason assertions unchanged |
| SSE ordering unchanged | `assertSSEFraming` (one message_start first, one message_stop last) |
| Client legacy number unchanged | every `_ContextOverridesInput` test still asserts the old 40000 client value |
| Internal sinks now accurate | `assertInternalTokensAccurate`: per-key/account/global all = 1500 (1200 upstream in + 300 upstream out), in lockstep |

## Behavior before → after (Case A: upstream in=1200, out=300, ctx=4% of 1M, credit=1.25)

| Sink | Before | After (legacy) | After (accurate) |
|---|---|---|---|
| Client `input_tokens` | 40000 | **40000** (unchanged) | 1200 |
| Client `output_tokens` | estimator | estimator (unchanged) | 300 |
| Per-key `TokensUsed` | 40000+est | **1500** | 1500 |
| Account `TotalTokens` | 40000+est | **1500** | 1500 |
| Global `totalTokens` | 40000+est | **1500** | 1500 |
| Credits (all sinks) | 1.25 | 1.25 | 1.25 |

## Tests

- Existing 8-path + cross-cutting characterization tests updated: internal-sink assertions flipped from "inflated" to "accurate" (the intended Phase A change); every client-facing legacy assertion left untouched and still passing.
- New accurate-mode tests: with `KIRO_USAGE_REPORTING=accurate`, client input becomes the upstream-accurate number while credits and upstream call count stay identical.

## Validation

- `gofmt` clean, `go vet ./...` clean, `go build ./...` clean, `git diff --check` clean.
- `go test ./...` green.
- Accounting tests: `-count=20` green (no flakiness), `-shuffle=on` green (no order dependence), `-race -count=5` green (no data race).
- Benchmarks: `docs/credit-benchmarks-phase-a-before.txt` / `-after.txt`. **Caveat:** the repo's only benchmarks (`BenchmarkRoute`, `BenchmarkPromptCache*`) do not exercise the finalize tails changed here, so before/after is noise-identical by construction. This is not evidence of the tails' hot-path cost; `usageSplit` adds one map-free comparison and a single env read per request, no allocation.

## What this does and does NOT do

- **Does:** make internal token stats (per-key quota, account stats, global dashboard totals) upstream-accurate instead of inflated by context occupancy; add an opt-in accurate client mode.
- **Does NOT reduce real credits.** Credits were already correct (straight from `meteringEvent.usage`); there is nothing to reduce. A per-key **token** limit will now trip on real usage instead of prematurely on inflated numbers — a correctness change, not a credit change.

## Rollback

Revert the commit. No data migration, no persisted state. The accurate client mode is off unless `KIRO_USAGE_REPORTING=accurate` is set, so even shipping this is a no-op for existing clients until an operator opts in.
