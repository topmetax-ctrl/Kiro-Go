# Credit Shadow-Metrics Report

- **Revision:** rev1 (2026-07-12)
- **Target SHA:** `ced9eec`
- **Status:** **NO SHADOW INSTRUMENTATION WAS ADDED IN THIS AUDIT.**

## Why no shadow run

Task §7 asks for a `KIRO_USAGE_SHADOW=1` instrumentation mode. Building it requires **production-code changes** (a new observation path threaded through all five handlers). The audit's standing constraint is read-only + test-only instrumentation that is reverted before finishing, and §18 lists "accounting fix changes client behavior" / unproven changes as stop conditions. Adding a shadow path to the hot handlers is a production change that should be reviewed on its own, not smuggled into an audit branch.

**Therefore:** the metric model below is specified so it can be implemented later, but **no runtime observations were collected**. Any table that would hold real numbers is marked `NOT COLLECTED`.

## Metric model (spec, not yet wired)

```go
type UsageObservation struct {
    UpstreamInputTokens    int      // OnComplete inTok — real
    UpstreamOutputTokens   int      // OnComplete outTok — real
    EstimatedInputTokens   int      // estimateClaude/OpenAIRequestInputTokens
    EstimatedOutputTokens  int      // estimate*OutputTokens
    ContextUsagePercent    float64  // contextUsageEvent
    ContextOccupancyTokens int      // pct × window — MUST NOT feed token stats
    ContextWindowTokens    int      // getContextWindowSize(model)
    ClientReportedInput    int      // what buildClaudeUsageMap emitted
    ClientReportedOutput   int
    AccountedInputTokens   int      // what RecordApiKeyUsage/UpdateStats received
    AccountedOutputTokens  int
    MeteredCredits         float64  // sum of meteringEvent.usage — the ONLY real credit
    KiroInferenceCalls     int      // rounds actually sent to Kiro
    RetryCount             int
    ToolRounds             int
    WebSearchRounds        int
    RequestPayloadBytes    int
    HistoryTurns           int
    ToolSchemaBytes        int
    ToolResultBytes        int
}
```

## What the fake-backend harness already establishes (no live traffic needed)

These are from `proxy/credit_audit_test.go`, executed at `ced9eec`:

| Observation | Value | Source |
|---|---|---|
| `ContextOccupancyTokens` vs `UpstreamInputTokens` (Case A) | 40000 vs 1200 (33×) | test log |
| `ContextOccupancyTokens` vs `UpstreamInputTokens` (Case C) | 100000 vs 500 (200×) | test log |
| Upstream output discarded (Case D) | real 9999 → estimator 1 | test log |
| Multiple metering events summed (Case E) | 1.0 + 2.5 = 3.5, no double-count | test |
| Mid-stream fail after metering (Case F) | proxy attributes 0 credits this attempt | test |

## Runtime observations

`NOT COLLECTED` — requires the shadow path (production change, out of audit scope) and/or a live staging run with operator-approved credit budget (§12, not granted).
