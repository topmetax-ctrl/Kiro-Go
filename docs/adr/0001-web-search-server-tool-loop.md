# ADR 0001: Free-first proxy-side `web_search` server-tool loop

Status: Accepted
Date: 2026-07-11

## Context

Claude Code sends Anthropic's `web_search` tool. Kiro-Go forwards it to the
Kiro/CodeWhisperer backend, which emits `tool_use(web_search, {query})` and then
waits for a `tool_result` (same protocol as any client tool, verified by runtime
probe against the live backend).

But `web_search` is an Anthropic **server tool**: the client expects the *server*
to execute it and never runs the search itself. Kiro is not Anthropic's server
and has no search executor, so no `tool_result` was ever produced. The turn
stalled or the model hallucinated. VibeCC (which sits between Claude Code and
Kiro-Go) only forwards bytes; it is not a place to add logic.

Runtime probe findings (verified, not assumed):
- Kiro emits `tool_use(web_search, {query:...})` for both the native
  server-tool spec (no `input_schema`) and a generic client tool with a `query`
  schema.
- With an empty `{"type":"object"}` schema, Kiro still fills `query` — but
  production should not depend on that implicit behavior, so we inject an
  explicit `{query}` schema.
- Production code only ever sends `KiroToolResult.Status = "success"`; no other
  status value is known to round-trip.

A secondary requirement drove the provider design: the feature must work
**free**, with no API key and no risk of accidental paid usage.

## Decision

Execute `web_search` inside Kiro-Go via a `ConversationRunner` state machine,
with search itself delegated to a **free-first `SearchOrchestrator`**:

1. Call Kiro. If the round emits internal `web_search` calls, run them through
   the orchestrator, build structured `tool_result`s, advance the conversation
   preserving the single-active-tool-turn invariant, and loop.
2. When a round has no internal search calls, it is terminal — its content (or a
   genuine client tool_use) goes to the client.
3. Bound the loop with `maxRounds` and `maxSearchesPerRequest`; on limit, run one
   finalization round with `web_search` removed so the model must answer.

The orchestrator owns provider routing, quality gating, caching, and reranking:

- **SearXNG is the primary provider** — self-hosted, free, no API key.
- **Tavily is an optional fallback** — off by default, budget-capped, free-only
  unless `routing.allowPaidUsage` is explicitly set.
- The router falls back only on provider error, open circuit, or a
  quality-rejected result set — never merely on an HTTP status.
- A deterministic `SearchQualityEvaluator` decides "acceptable" (result count,
  domain diversity, duplicate concentration, topical overlap). No paid LLM.
- A bounded LRU `SearchCache` and a per-request query cache avoid re-hitting a
  provider (and re-spending Tavily credits) for identical queries.
- A persistent monthly `tavilyBudget` enforces the free-tier ceiling across
  restarts; when spent, the router drops to SearXNG-only.

Both stream and non-stream handlers use the same runner. Native metadata
(`max_uses`, domain filters) is parsed into a `WebSearchPolicy` and never
forwarded to Kiro. The `web_search` tool gets an explicit `{query}` schema
injected before forwarding.

## Alternatives rejected

- **Paid-first / Tavily-only** (the initial implementation): forces an API key
  and risks spend. Replaced by SearXNG-primary with Tavily as a capped fallback.
- **Heuristic pre-search** (guess when to search and inject results): the model
  already asks explicitly via `tool_use`; guessing is strictly worse.
- **Implement in VibeCC**: VibeCC only forwards; splitting the loop across a
  byte-forwarder is the wrong seam.
- **Concatenate search text into the original question**: loses the structured
  tool_use⟺tool_result linkage the backend expects and muddles the turn.
- **Expose the unresolved `tool_use` to Claude Code**: proven to stall — the
  client does not execute this server tool.
- **Fake Anthropic native `encrypted_content`/signed citations**: the proxy
  cannot mint those; pretending would be dishonest and brittle.
- **Meilisearch as a live-web provider**: it is a document index for ingested
  data, not a discovery engine. Reserved as a `SearchCorpus` extension point,
  not wired as a provider.
- **Integrate Vane wholesale**: Vane is an answering/research engine that itself
  drives an LLM and SearXNG. Nesting it adds latency, token cost, and failure
  modes; Kiro already does the reasoning. We reuse patterns (multi-query,
  dedup, citation), not the dependency.
- **`KiroToolResult.Status = "error"` for failures**: unverified that Kiro
  accepts it. We encode failures in the result *content* with `Status:"success"`
  (the only value proven to round-trip).

## Consequences

- Works free out of the box (SearXNG); no key, no card, no accidental spend.
- Client sees only final answers; no leaked `web_search` tool_use.
- Streaming is buffered during search rounds (unavoidable: we only know a round
  was terminal after it completes).
- Usage/credits are aggregated across rounds; Tavily credits tracked separately
  from Kiro credits and booked against a persistent monthly budget.
- SearXNG must be deployed (Docker sibling, private network, JSON format
  enabled). Operational surface is larger than a hosted API, but free.

## Limitations

Mixed server/client tool use in one round (typed error, not executed); native
citation encryption (not reproduced); buffered per-round streaming; two
providers (interface admits more).
