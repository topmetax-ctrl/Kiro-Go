# Web Search (server-side `web_search` tool)

Kiro-Go executes the Anthropic `web_search` tool itself, in-process, instead of
handing an unresolved `tool_use` back to the client.

## Why this exists

The Kiro/CodeWhisperer backend recognizes a `web_search` tool and emits a
`tool_use(web_search, {query})`, then stops and waits for a `tool_result` — the
same protocol as any client tool. But `web_search` is an Anthropic **server
tool**: the client (Claude Code) assumes the server executes it and never runs
the search itself. So nothing produced the `tool_result`, and the turn stalled.

Kiro-Go closes that gap: when the model asks to search, the proxy runs the
search, feeds the results back as a structured `tool_result`, and loops until
the model produces a final answer. The client only ever sees the final answer —
never the intermediate `web_search` tool calls.

This is a **functional emulation**, not byte-for-byte Anthropic native
compatibility. See "Limitations" below.

## Architecture

```
Claude handler
   └── ConversationRunner            (shared by stream + non-stream)
         ├── KiroRoundCaller         → CallKiroAPIContext (one buffered round)
         └── ServerToolExecutor (web_search)
               └── SearchOrchestrator
                     ├── SearchCache            (bounded LRU, per-params key)
                     ├── ProviderRouter         (free-first)
                     │     ├── SearXNGProvider   (primary, free)
                     │     └── TavilyProvider    (optional fallback, budget-capped)
                     ├── SearchQualityEvaluator  (deterministic gate)
                     └── SearchReranker          (deterministic, heuristic)
```

Handlers do not call a provider directly. They resolve a `WebSearchPolicy` from
the request, call the runner, render, and account once. Providers, quality
gating, caching, and reranking all live behind `SearchOrchestrator`.

## Free-first routing

The default is **free-first**: SearXNG (self-hosted, no API cost) is the primary
provider and answers most requests. Tavily is an **optional** fallback that is
off unless explicitly enabled, and even then it is **budget-capped** and never
incurs paid usage unless you opt in.

The router falls back to the next provider only when the primary **errors, is
unhealthy (circuit open), or returns results the quality evaluator rejects** —
never merely on an HTTP status. The decision is quality-driven.

`routing.allowPaidUsage` is a hard gate. When `false` (default), a paid provider
(Tavily) is used only while its monthly free-credit budget still allows a call;
once the budget is spent, the router drops back to SearXNG-only for the rest of
the month. No configuration silently spends money.

## Configuration

`data/config.json`, under the `webSearch` key:

```json
{
  "webSearch": {
    "enabled": true,
    "routing": {
      "mode": "free-first",
      "primaryProvider": "searxng",
      "fallbackProviders": ["tavily"],
      "allowPaidUsage": false
    },
    "limits": {
      "maxRounds": 4,
      "maxSearchesPerRequest": 5,
      "maxConcurrentSearches": 2,
      "maxResultsPerSearch": 8,
      "totalTimeoutSeconds": 90
    },
    "searxng": {
      "enabled": true,
      "baseUrl": "http://searxng:8080",
      "timeoutSeconds": 12,
      "language": "auto",
      "safeSearch": 1,
      "categories": ["general"],
      "minimumResults": 3
    },
    "tavily": {
      "enabled": false,
      "apiKey": "",
      "freeOnly": true,
      "monthlyCreditLimit": 1000,
      "searchDepth": "basic",
      "autoParameters": false,
      "timeoutSeconds": 15,
      "retryMax": 2,
      "retryBaseDelayMs": 300
    },
    "cache": {
      "enabled": true,
      "provider": "memory",
      "ttlSeconds": 900,
      "maxEntries": 1000
    },
    "reranking": {
      "provider": "heuristic",
      "maxCandidates": 20,
      "maxFinalResults": 8
    },
    "appendSources": true
  }
}
```

### Minimal free setup

Only `enabled` and a reachable `searxng.baseUrl` are required. Everything else
has a working default:

```json
{ "webSearch": { "enabled": true, "searxng": { "baseUrl": "http://searxng:8080" } } }
```

No API key. No paid provider. No credit card.

### Enabling Tavily fallback (optional)

Set `tavily.enabled: true` and provide a key via `tavily.apiKey` or the
`TAVILY_API_KEY` environment variable (env wins). It stays free-only:
`freeOnly` forces `basic` depth and forbids `auto_parameters`, and
`monthlyCreditLimit` (default 1000) stops Tavily once the month's free credits
are spent. The budget is persisted to `tavily_budget.json` beside the config so
it survives restarts and rolls over on the calendar month (UTC).

## Behavior

- Enabled **and** a provider usable (SearXNG base URL, or Tavily enabled+key),
  request carries a `web_search` tool → proxy runs the search loop.
- Enabled toggle on but **no usable provider** → request with `web_search` is
  rejected with a clear configuration error (500). It is not silently passed
  through, because the client cannot execute the tool and would stall.
- Toggle off → legacy pass-through, unchanged.
- No `web_search` tool in the request → unchanged, runner not engaged.

Stream and non-stream share one orchestrator (`ConversationRunner`), so
semantics are identical. On the stream path, intermediate search rounds are
buffered (not streamed); only the final round's events are replayed to the
client. Expect a pause during search rounds.

Within one round the model may issue several searches; they run concurrently,
bounded by `limits.maxConcurrentSearches`, and their `tool_result`s are returned
in tool-use order (not completion order). Duplicate queries — within a round or
across rounds — resolve from the per-request cache and never hit a provider (or
spend a Tavily credit) twice.

## SearXNG deployment

SearXNG runs as a Docker sibling on a private network (`docker-compose.yml`):

- **Not published to the host** — no `ports:` mapping, so nothing outside the
  Docker network can reach it. kiro-go reaches it at `http://searxng:8080`.
- The network is a normal bridge (not `internal`), because SearXNG needs
  outbound internet to query upstream engines. Isolation is "no published
  port", not "no egress".
- **JSON output must be enabled** in `searxng/settings.yml`
  (`search.formats: [html, json]`); a stock instance returns **403** for
  `format=json`. The 403 error message calls this out explicitly.
- The bot limiter is off (private instance), so no token dance is needed.
- Healthcheck hits `/healthz`; logs are rotated (json-file, 1 MB × 3).

The base URL is fixed from server config and validated once at wiring time; it
is **never** derived from a request (SSRF guard).

## Error mapping

| Condition | Client result | Kiro account |
|-----------|---------------|--------------|
| Feature disabled | Legacy pass-through | unaffected |
| Enabled, no usable provider | 500 config error | not failed |
| Both `allowed_domains` + `blocked_domains` | 400 invalid request | not failed |
| SearXNG 403 (JSON disabled) | fallback, then surfaced | **not** failed |
| Provider timeout/5xx | fallback, then surfaced | **not** failed |
| Tavily budget exhausted (free-only) | SearXNG-only | not failed |
| Empty / low-quality results | fallback, then success | unaffected |
| Kiro round error | account failed, retried on next account | failed |
| Client disconnect | request aborted, no retry | not failed |
| Mixed `web_search` + client tool in one round | typed error surfaced | not failed |

A search/provider failure never fails a Kiro account. A Kiro round failure
retries from the **original** payload on a different account (account affinity:
all rounds of one request use one account).

## Usage and accounting

Kiro tokens and credits are aggregated across every round (not just the final
one). Tavily credits are tracked **separately** from Kiro credits and booked
against the monthly budget — they are never added to the Kiro credit total.
API-key usage and account stats are recorded once per logical request.

## Source attribution

Result snippets are framed as untrusted external data (prompt-injection defense)
and the model is asked to cite `[1]`, `[2]`, etc. When `appendSources` is on, a
deduplicated `Sources:` list is appended to the final answer. The proxy does
**not** reproduce Anthropic's `encrypted_content` / signed citations — it cannot
mint those.

## Security

- SearXNG base URL validated at startup; never taken from a request (SSRF).
- Tavily key redacted in all logs/errors; only ever placed in the Bearer header.
- Query is logged as a 12-char hash, never in the clear.
- Result URLs must be http(s); response bodies are size-limited; query length
  is capped; no raw HTML is fed to Kiro.
- Caches are bounded; no global store of sensitive query/result content.

## Observability

The orchestrator emits one structured debug line per search with
`query_hash`, `provider`, `fallback`, `cache_hit`, `results`,
`tavily_credits`, `latency_ms`, and `quality_score`. No API key, no
Authorization header, no raw query or URL is logged. A Prometheus-style metrics
surface is a documented extension point (not built in phase 1).

## Limitations

- **Mixed tools**: a single round emitting both `web_search` and a client tool
  (e.g. Bash) is refused with a typed error rather than executed half-way.
- **Native metadata**: `max_uses` and domain filters are honored; other native
  `web_search` fields are parsed but not all are forwarded to a provider.
- **No citation encryption**: functional sources only.
- **Buffered streaming** during search rounds.
- **Providers**: SearXNG + optional Tavily. The `SearchProvider` interface
  admits more without touching the runner.

## Extending

- **New provider**: implement `SearchProvider` (`search_provider.go`) and add it
  in `newSearchOrchestratorFromConfig`. Router, quality, cache, rerank, and
  handler paths are provider-agnostic.
- **Distributed cache**: implement `SearchCache` (`search_cache.go`) with a
  Redis backend; the orchestrator is unchanged.
- **Content fetcher / local corpus**: `ContentFetcher` and `SearchCorpus` are
  reserved extension points (e.g. Meilisearch for ingested docs). Meilisearch is
  a document index, **not** a live-web provider, so it is not wired as one.

## Testing

```bash
go test ./proxy/            # unit + handler integration
go test -race ./proxy/      # race
```

Live integration (opt-in, not run by default):

```bash
# Tavily (needs a key):
TAVILY_API_KEY=tvly-... KIRO_WEBSEARCH_INTEGRATION=1 go test ./proxy/ -run Tavily

# SearXNG (needs a reachable instance):
KIRO_WEBSEARCH_INTEGRATION=1 KIRO_SEARXNG_URL=http://localhost:8080 \
  go test ./proxy/ -run SearXNGLive
```
