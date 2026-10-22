# Cache observability

Forward events record what is needed to explain a prompt-cache write after the
fact: how the upstream split it by TTL tier, and keyed digests of what the
request asked it to cache. Nothing here changes a request or a response.

The question it answers: when a request writes most of its prompt to cache
instead of reading it, was the cache **expired**, did the **prefix change**, did
the **credential change**, or was it a **concurrent write**? Without these
fields the gateway can only see totals.

## What an event carries

All fields are on `GET /admin/api/forward-events` and its JSON export. Events
without them serialize exactly as before.

| Field | Meaning |
|---|---|
| `usage.cacheCreation5mTokens` / `cacheCreation1hTokens` | The upstream's own split of `cacheCreationInputTokens` by TTL tier (`usage.cache_creation`). Absent when the upstream sent no breakdown; the tier is then unknown, never derived. |
| `upstreamModel` | The model the response named. May differ from `targetModel` if a gateway in between remaps it. |
| `cache.conv` | Digest of the first message: anchors one conversation until the client rewrites history. |
| `cache.tools`, `cache.system` | Digests of the tool definitions and the system prompt. |
| `cache.settings` | Digest of `model`, `thinking`, `output_config`, `tool_choice`: the fields that invalidate cached messages. |
| `cache.msgs` | Message count. |
| `cache.prefix` | `"k:digest"` checkpoints; the digest covers tools, system and the first `k` messages. The newest 4 are always listed, plus one every 16 (wider for very long conversations, at most 24). |
| `cache.marks` | Breakpoints on top-level blocks: `t<i>` tool, `s<i>` system block, `m<i>` message, suffixed `:<ttl>` when the body names one. |
| `cache.policy` | Token Optimization settings in force: `off`, `observe`, or `on+headroom-cache+caveman`. |

Only Claude-shaped routes (`/v1/messages`) are traced.

### Digests

Truncated (48-bit) HMAC-SHA256. The key is derived from the api-key pepper
(`apiKeyPepper` in `config.json`, or `KIRO_APIKEY_PEPPER`) by a fixed label, so it
survives restarts and is not the pepper itself. A digest equals another only when
the content is identical; it reveals nothing else. No prompt text, source code,
tool arguments, session id or credential is stored.

`cache_control` markers are removed before hashing. Claude Code moves its
breakpoint to the newest message every turn; if the marker counted, no two
requests would ever match. Only flat markers (`{"type":"ephemeral"}`, with an
optional `ttl`/`scope`) and `null` are removed, which is what clients send.

A conversation's next request extends the previous one by one to three
messages, so its `prefix` list contains the previous request's `k:digest` for
`k = msgs`. If it does not, the history changed, and the largest `k` that both
lists share with equal digests bounds where.

Cost: about 9 ms and 2 MB of allocation per 1.8 MB body (Intel i5-8500B),
computed in parallel with the upstream call. An event gains 250 to 730 bytes
(up to 3000 messages). The event ring holds 5000 events, so the log grows by
about 2.5 MB.

### Switching it off

`"cacheTrace": false` in `config.json`, then restart. Events then carry no
`cache` object; the TTL split and `upstreamModel` are plain response telemetry
and stay.

## Classifying rewrites

```
python3 scripts/classify_cache_rewrites.py events.json [--min-write 100000]
```

`events.json` is the output of `/admin/api/forward-events?limit=5000` (header
`X-Admin-Password`) or the JSON export. A rewrite is a request whose
`cacheCreationInputTokens` reaches `--min-write`. Each gets one label:

| Label | Evidence required |
|---|---|
| `concurrent_write` | An earlier-started request for the identical prompt (same `conv` and full-prefix digest) had not produced a response byte when this one started. |
| `expired` | The previous request of the conversation ended more than 3600 s earlier (no tier lives longer), or it wrote the 5m tier and ended more than 300 s earlier. |
| `prefix_changed` | The history no longer extends the previous request's, and expiry is excluded (gap within 300 s, or the previous write was the 1h tier). Names the changed parts and the message window. |
| `scope_changed` | The previous request was served by a different credential (needs a per-request credential; see below). |
| `unknown` | Anything else, with the open candidates in brackets. |

Events recorded before this feature have no digests and no TTL split. For those
the previous request is a guess from sizes, labels that depend on it say
`heuristic`, and `prefix_changed` and `scope_changed` cannot be reached.

## What it cannot see

- **The final upstream body.** The digests describe what this gateway sent. In
  the deployed chain a CLIProxyAPI hop sits in between. Its Claude executor
  (v7.2.144 source; the deployed v8.0.8 was not diffed) can rewrite
  `cache_control` for non-native clients (inject breakpoints, upgrade a marker
  with no `ttl` to `1h` for OAuth credentials, cap at four, repair TTL order) and
  adds `context_management` with `keep: "all"`, which clears nothing. A digest
  that stays equal proves the client side was stable. The response-side TTL
  split is the ground truth of what the upstream wrote.
- **Which credential served a request.** The upstream hop exposes no per-request
  credential header and generates its own request ids. Its usage records carry
  `auth_index`, token counts and timestamps, but are off by default
  (`usage-statistics-enabled: false`) and kept in memory for 60 s by default.
  Joining them to events on exact token counts and time would give the
  credential per request; that needs a change on the CLIProxyAPI side and is not
  done here.
- **Cache scope of the upstream.** Anthropic documents isolation per workspace
  on the Claude API and per organization on Bedrock and Google Cloud. Neither
  covers OAuth subscription credentials, so two credentials sharing one
  organization id do not prove they share a cache.

## Operating notes

- The event ring is in memory only and holds 5000 events. A container recreate
  empties it: export first (`/admin/api/forward-events/export?format=json`, at
  most 5000).
- Roll back by deploying the previous image. Events recorded by the new build
  are plain JSON with extra fields; nothing persisted depends on them.
