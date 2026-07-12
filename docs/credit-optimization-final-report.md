# Credit Optimization — Final Report

- **Revision:** rev1 (2026-07-12)
- **Target SHA:** `ced9eec` (branch `audit/credit-usage-correctness`, cut from `origin/feat/hardening-profile-picker-lru` + 5 profile-picker commits, per operator instruction to audit latest code)
- **Scope:** read + offline harness only. No production code changed. No shadow instrumentation added. No live credit spend. No deploy/merge/push of source changes.
- **Baseline:** `go vet`, build, `go test ./...`, `go test -race ./...` all green at `ced9eec`.

## What was done vs. what the prompt scoped

This audit is **investigation + executable evidence + written deliverables**. It deliberately did NOT:

- add shadow instrumentation (`KIRO_USAGE_SHADOW`) — that is production code; the prompt marks it staging-only and behind a benchmark gate, and no approval to add it was given;
- run a live A/B — hits the §18 stop condition (real credit spend, no approved budget);
- apply any Phase A/B fix — the prompt sequences fixes *after* shadow metrics + A/B, neither of which is authorized yet.

The one artifact added to the audit worktree is `proxy/credit_audit_test.go` (a test-only harness) plus the five docs. Per the audit prompt's §17, the harness is **kept as a committed test** (`test: add usage accounting verification harness`) — it touches no production code and documents the accounting behavior as executable evidence.

## The three required answers

### 1. Where is the displayed token count wrong?

**Input tokens: context occupancy is displayed as input tokens.** When a `contextUsageEvent` arrives, the handler computes `realInputTokens = contextPct × getContextWindowSize(model) / 100` and, in **6 of 7 logical tails**, overwrites the real upstream input token count with it. For a 1M-window model at 4% occupancy that is 40,000 "input tokens" reported when the real upstream input was ~1,200 — a **~33× inflation** (Case A, proven). At 200x in the conflict case (Case C). This is context-window *occupancy*, not request input, and it does not correspond to any billed quantity.

Confirmed override sites (current code):
- `handler.go:1589-1592` — Claude stream, non-runner path
- `handler.go:1843-1846` — Claude non-stream (both runner and live sub-paths; for the runner sub-path it discards the correctly-summed `run.TotalInputTokens` from :1793)
- `handler.go:2316-2319` — OpenAI stream
- `handler.go:2431-2434` — OpenAI non-stream
- `responses_handler.go:210-213` — Responses non-stream
- `responses_handler.go:555-558` — Responses stream

The **only** corrected tail is `handler.go:1582-1588` (Claude stream + web_search runner), which falls back to `realInputTokens` only when `inputTokens <= 0`. This asymmetry means the *same* stream is reported differently depending on which endpoint/path served it (proven: `TestAudit_HandlerTail_ContextOverridesUpstream`).

**Output tokens: always the local estimator, never upstream.** Every tail calls `estimateClaudeOutputTokens` / `estimateApproxTokens` / `estimateOpenAIOutputTokens` and overwrites the upstream-reported output. When upstream said 9,999 and the estimator said 1, the client/stats see 1 (Case D, proven).

**This inflation flows into internal accounting**, not just the client response: `recordSuccessForApiKey → RecordApiKeyUsage` adds `inputTokens+outputTokens` to the API key's `TokensUsed`, and `pool.UpdateStats` adds the same to the account's `TotalTokens`. So the inflated number can trip a per-key **token** limit early. It does **not** affect credits (next section).

### 2. Why do real credits increase, and how much was measured?

**Real credits do NOT come from any token count.** They come exclusively from `meteringEvent.usage`, summed in `parseEventStream` (`kiro.go:580-583`) and carried untouched through `OnCredits → credits → recordSuccessForApiKey(..., credits)` and `pool.UpdateStats(..., credits)`. Multiple metering events sum correctly with no double-count (Case E, proven: 1.0 + 2.5 = 3.5). This matches Kiro's official model: **billing is in credits, not tokens** (kiro.dev/pricing, kiro.dev/faq, accessed 2026-07-12).

Therefore the token inflation in answer (1) is a **display/accounting bug, not a real credit increase.** Comparing the proxy's token display against the IDE's credit figure is comparing two different units.

**Genuine real-credit causes (semantics confirmed from official docs; runtime magnitude NOT measured — no A/B was authorized):**

- **Model multiplier (Confirmed semantics, `Chưa xác minh` runtime).** Kiro docs state different models bill at different rates — a task costing X credits in **Auto** costs **~1.3X via Sonnet 4.6**. The proxy pins concrete models (`gpt-*` → `claude-sonnet-4.5`; `claude-*` passthrough). If your IDE runs **Auto** while the proxy pins a specific higher-multiplier model, the proxy path can cost **genuinely more credits per identical prompt**. This is the most likely real-credit difference. **Not measured** — needs an approved A/B (matrix #8/#9).
- **Web-search multi-round (Confirmed mechanism).** Each runner round is one Kiro inference and bills its own credits; a search conversation is 2-4+ inferences vs. 1 for a plain chat. Credits sum across rounds (correct, not a bug), but a search-augmented request legitimately costs more than a bare one. Not a defect; a cost driver to be aware of when comparing "same prompt" across interfaces.

**Ruled out as real-credit causes:**
- **Retry double-metering (Disproved).** Retries fire only pre-stream (`!messageStarted`) and only on a **different** account; credits are recorded once on success. A mid-stream failure after a metering event attributes 0 credits/0 tokens for that attempt on the proxy side; any upstream double-bill is not proxy-observable (Case F). No proxy-side double count.
- **Duplicate payload (Disproved).** System-prompt priming is prepended once (`translator.go:283-299`); history is serialized once; no duplicated turn, schema, or tool result found. Full-history replay is stateless-protocol compatibility, not a duplication bug.
- **Forwarded requests (not a Kiro-credit path).** `upstream_forward.go` never touches Kiro credits; it now records real forwarded tokens for per-key quota (`:146`) instead of the old `recordSuccess(0,0,0)`, with credits fixed at 0 — no fabricated pricing.
- **Prompt cache (metadata only).** `billedClaudeInputTokens` subtracts local cache-tracker tokens from the *displayed* input, but the payload sends no backend `cache-control` and no backend cache-hit is consumed. It affects display only, not credits.

**Measured magnitude of real-credit increase: none.** No A/B was authorized, so no credit delta between IDE and proxy was measured. The honest verdict: the *observed* discrepancy is dominated by the token-display bug; the only plausible *real* credit difference (model/Auto multiplier) is confirmed in principle but unquantified here.

### 3. Which changes reduce credits while preserving context and quality?

**None are implemented in this audit** (the prompt sequences implementation after shadow metrics + approved A/B). What the evidence supports, in priority order:

**Accounting-only (Phase A) — reduces the *displayed* number, spends the same credits.** Stop using context occupancy as input tokens; prefer upstream-reported input, fall back to estimator only when upstream is absent; prefer upstream output over the estimator. Keep context occupancy as a separate metric. This is payload-identical and credit-identical by construction — the correct fix for the *reported* inflation. It does not reduce real credits (there is nothing to reduce; the credits were always correct). Client-facing `usage.input_tokens` should change only behind a `legacy/accurate/dual` mode defaulting to current behavior, to avoid breaking any client that keys off the old number.

**Real-credit reduction (Phase B/C) — requires operator approval + measured A/B:**
- The one lever with real credit impact and confirmed semantics is **model selection** (use Auto or a lower-multiplier model). But this is a **product decision with a quality trade-off**, explicitly out of bounds for silent "optimization" per the prompt. It must be operator-approved and A/B-measured, not applied as a correctness fix.

**Bottom line:** the thing that looks like a cost problem is a reporting problem; the thing that might be a real cost problem (model multiplier) is a product choice, not a bug to be fixed silently.

## Findings table

| ID | Finding | Evidence | Confidence | Credit impact | Token-report impact | Context impact | Quality impact |
|----|---------|----------|-----------|---------------|--------------------|--------------------|----------------|
| F1 | Context occupancy overrides input tokens in 6/7 tails | Cases A/C; override sites listed | Confirmed | none | severe over-report | none | none |
| F2 | Upstream output always discarded for estimator | Case D | Confirmed | none | under/over per estimator error | none | none |
| F3 | Credits sourced only from meteringEvent, summed correctly | Case E | Confirmed | accurate | n/a | none | none |
| F4 | Fix applied to only 1 of 7 tails (asymmetric reporting) | override inventory | Confirmed | none | path-dependent | none | none |
| F5 | Model/Auto multiplier can raise real credits | kiro.dev docs | Confirmed semantics / runtime `Chưa xác minh` | up to ~1.3× | none | none | quality trade-off if changed |
| F6 | Retry does not double-meter on proxy side | Case F; failover code | Disproved (as a bug) | none | none | none | none |
| F7 | No duplicate payload / system prompt / history | translator read | Disproved (as a bug) | none | none | none | none |

## Unknowns (`Chưa xác minh`)

- Exact upstream semantics of `inputTokens`/`outputTokens` event fields vs. Kiro's internal credit formula — no official doc for event-level token semantics.
- Runtime credit delta between IDE-Auto and proxy-pinned-model — needs approved A/B.
- Which profile/region/plan the operator's live account actually runs — not inspected (no live account use).

## Performance

No before/after benchmarks were taken because no production code was changed. The prompt's benchmark harness (`docs/credit-benchmarks-*.txt`) is a **prerequisite for the fix commits**, not for this read-only audit.

## Rollback

No production code changed, so there is nothing in the running proxy to roll back. The only added artifact is `proxy/credit_audit_test.go` (test-only) plus these docs, on the isolated branch `audit/credit-usage-correctness` in worktree `../kiro-credit-audit`. To discard entirely: `git worktree remove ../kiro-credit-audit` and delete the branch. The branch is pushed to `origin` per §19 for operator review; it is never merged.

## Artifact status

`proxy/credit_audit_test.go` is **kept and committed** as the accounting verification harness (§17 commit #1). It adds no production code, imports nothing new, and runs in the normal `go test ./proxy` suite. Its evidence is mirrored in this report and in `credit-usage-full-audit.md`.
