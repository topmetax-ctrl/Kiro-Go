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
| `cache.prefix` | `"k:digest"` checkpoints; the digest covers tools, system and the first `k` messages. The newest 4 are always listed, plus one every 16 (wider for very long conversations, at most 24), plus the boundary after every message that carries a breakpoint. |
| `cache.marks` | The breakpoint signature: `t<i>` tool, `s<i>` system block, `m<i>` message, suffixed `:<ttl>` and `@<scope>` when the body names them (`s0:1h@global`). |
| `cache.policy` | Token Optimization settings in force: `off`, `observe`, or `on+headroom-cache+caveman`. |

Only Claude-shaped routes (`/v1/messages`) are traced.

### Content digests and the breakpoint signature are two things

`cache.prefix` (with `conv`, `tools`, `system`) says **what** the prompt
contained. It ignores `cache_control` markers on purpose: Claude Code moves its
breakpoint to the newest message every turn, so a digest that counted the marker
would never match the turn before.

`cache.marks` says **where** the client asked the upstream to cache, for how
long, and with what scope. The upstream keys on both, so equal content digests do
not make equal cache entries: two requests with the same messages and a
breakpoint in a different place, a different `ttl` or a different `scope` write
and read different entries. Compare marks as well. The two meet at one point: the
boundary after each marked message is always among the `prefix` checkpoints, so
the digest at a breakpoint is the content that breakpoint asks to cache. A later
request can read that entry only if it lists the same digest.

A `ttl` or `scope` that is not a short plain token (lower case, digits, `-_.`,
at most 16 characters) is recorded as `?`. The body is client input; the event
copies no free text from it.

### Digests

Truncated (96-bit) HMAC-SHA256. The key is derived from the api-key pepper
(`apiKeyPepper` in `config.json`, or `KIRO_APIKEY_PEPPER`) by a fixed label, so it
survives restarts and is not the pepper itself. A digest equals another only when
the content is identical; it reveals nothing else. No prompt text, source code,
tool arguments, session id or credential is stored.

Rotating the pepper (which also invalidates stored API-key hashes) makes digests
recorded before and after it incomparable. An install with no pepper creates one
on first use, as the API-key store does.

The scheme, for anyone recomputing a digest outside Kiro-Go (a Python HMAC
reproduces the pinned test values in `cachetrace/trace_test.go`):

```
key       = HMAC-SHA256(pepper, "kiro-go/cache-trace/v1")
D(parts…) = HMAC-SHA256(key, ⧺ over parts of: uint64-big-endian(len(part)) ‖ part)
tools     = D(tools JSON)          system = D(system JSON)         (markers removed)
msg_i     = D(message i JSON)                                     (markers removed)
prefix(k) = HMAC-SHA256(key, tools ‖ system ‖ msg_1 ‖ … ‖ msg_k)   (the full 32-byte sums)
conv      = msg_1                  settings = D(model, thinking, output_config, tool_choice)
```

Every digest is the first 12 bytes of its value, hex encoded. Parts are the raw
JSON values exactly as the client sent them; an absent field is an empty part.

`cache_control` markers are removed before hashing. Only flat markers
(`{"type":"ephemeral"}`, with an optional `ttl`/`scope`, compact as clients send
them) and `null` are removed. Anything else is left in, so a digest that differs
is a visible miss, never a silent wrong match.

A conversation's next request extends the previous one by one to three
messages, so its `prefix` list contains the previous request's `k:digest` for
`k = msgs`. If it does not, the history changed, and the largest `k` that both
lists share with equal digests bounds where.

### Cost and bounds

About 9 ms and 2 MB of allocation per 1.8 MB body (Intel i5-8500B), computed in
parallel with the upstream call, so a request waits for it only if the upstream
answers faster than the hash. The goroutine always finishes (no I/O, no locks)
and holds nothing once the event is built. An event gains 300 to 1100 bytes (up
to 3000 messages; at most 44 prefix checkpoints and 16 marks).

The event ring holds `metrics.EventRingCapacity` = 12000 events, about 1.7 KB
each with a trace: roughly 20 MB when full, about two days of the traffic this
gateway has seen. The JSON export covers the whole ring.

### Switching it off

`"cacheTrace": false` in `config.json`, then restart. Events then carry no
`cache` object; the TTL split and `upstreamModel` are plain response telemetry
and stay.

## Classifying rewrites

```
python3 scripts/classify_cache_rewrites.py events.json [--min-write 100000]
```

`events.json` is the output of `/admin/api/forward-events/export?format=json`
(header `X-Admin-Password`). A rewrite is a request whose
`cacheCreationInputTokens` reaches `--min-write`. Each gets one label and one
confidence:

| Label | Evidence required |
|---|---|
| `concurrent_write` | An earlier-started request of the same conversation (same `conv` and `settings`) that asked to cache a prefix this request also contains had not produced a response byte when this one started, so that entry was not readable yet. The two prompts may differ after the shared prefix (a branch); the evidence names where. |
| `expired` | The previous request of the conversation ended more than 3600 s earlier (no tier lives longer), or it wrote the 5m tier and ended more than 300 s earlier. |
| `prefix_changed` | The history no longer extends the previous request's, and expiry is excluded (gap within 300 s, or the previous write was the 1h tier). Names the changed parts and the message window. |
| `scope_changed` | The previous request was served by a different credential (needs a per-request credential; see below). |
| `unknown` | Anything else, with the open candidates in brackets. |

| Confidence | Meaning |
|---|---|
| `confirmed` | The events show it: prompt digests, the upstream's own TTL split, request timing. |
| `likely` | It fits but rests on a guess: a predecessor inferred from sizes (`heuristic`), a size match where a digest would be needed, or a credential change (a different credential does not by itself show that the upstream scopes its cache per credential). |
| `unknown` | The evidence does not decide. |

When two linked requests also differ in breakpoint layout or TTL tier, the
evidence says so (`breakpoints changed m-0:1h -> m-0`, `TTL tier changed 1h ->
5m`). That is reported, not blamed: whether it caused the miss is for the reader
to decide. A message breakpoint is compared by its distance from the newest
message, so a marker that moves along with the history is not a change.

Events recorded before this feature have no digests and no TTL split. For those
the previous request is a guess from sizes, labels that depend on it say
`heuristic`, and `prefix_changed` and `scope_changed` cannot be reached.

## In the dashboard

Forwarding → Stats has a **Cache analytics** card, and Forwarding → Activity can
filter on it. Both read the event ring, because only events carry the 5m/1h
split; neither changes a request.

The card follows the Stats range selector and provider filter and shows, for
requests whose upstream reported cache usage:

- **Cache reuse**: cache read ÷ input, token-weighted, over the requests that
  reported a read figure (a write-only report never dilutes it).
- **Cache writes**, split by the upstream's own TTL tier (`5m`, `1h`) and a
  separate "no tier" part when the upstream sent no split. A tier is never
  guessed.
- **Uncached input** (`max(0, input − read − write)` per request) and **large
  writes** (a single write of at least 100k tokens, the default of `bigWrite`).
- A stacked chart (writes by TTL tier, or the input as read / write / uncached
  shares) and the ten largest writes. A bar or a listed write opens Activity on
  the requests it was drawn from: the bar's time slice (clamped to the chosen
  range), the selected providers, and the same population (`cacheEligible` for
  the share view, `minCacheWrite=1` for the writes view). While such a window is
  set, Activity shows it as a chip and switches its range select off.

Left out, and counted in the card: **Kiro pool** requests (their cache figures
are synthesised for the client, not reported by an upstream), requests whose
usage is **estimated**, and successful requests with **no cache figure** or no
input total. The card does not say why a write happened; for that, export the
events and run the classifier below.

Every window is labelled with where its data starts. The event log begins at
process start, or at the oldest event once the ring has wrapped at any point
(`retention.eventLogSinceMs`; a per-provider stats reset does not undo that);
the per-minute charts begin at the later of process start and the two-hour
window they are pruned to (`retention.minuteSinceMs`). Time before that is
drawn as "no data", not as zero requests.

### API

| Endpoint | Purpose |
|---|---|
| `GET /admin/api/forward-cache-series?hours=N` or `from=&to=` (ms) | Buckets of `read`, `write`, `write5m`, `write1h`, `writeUntiered`, `uncached`, `readInput`, `bigWrites`, `maxWrite`; `totals`; `topWrites` (10, with provider, model, request id and tier split; `read` is omitted when the upstream reported no read figure); `eligible` and `excluded` (`pool`, `estimated`, `notReported`); `coverageFrom`; `retention`. Optional `provider=a,b` and `bigWrite=tokens`. Bucket width follows the window (1 min up to 1 h, 5 min up to 6 h, 15 min up to 24 h, then 1 h; windows, and `hours`, are capped at 30 days). |
| `GET /admin/api/forward-events?cacheEligible=1` and `minCacheWrite=N` | The population a cache chart counts: usage the upstream reported (not estimated, not the Kiro pool) with a cache figure and an input total. `minCacheWrite` additionally requires a cache write of at least `N` tokens (and implies `cacheEligible`). `provider` accepts a comma separated list; combine with `since`/`until`. |
| `GET /admin/api/forward-stats` | Gains `retention`: `eventRingCapacity`, `eventRingSize`, `eventRingOldestMs`, `memorySinceMs`, `minuteSinceMs`, `eventLogSinceMs`. |
| `GET /admin/api/forward-history` | Gains `minuteSinceMs`. |

All of these are additive; existing fields are unchanged.

## What it cannot see

Every label describes the body **this gateway sent**.

- **The final upstream body.** In the deployed chain a CLIProxyAPI hop sits in
  between. Its Claude executor (v7.2.144 source; the deployed v8.0.8 was not
  diffed) can rewrite `cache_control` for non-native clients (inject
  breakpoints, upgrade a marker with no `ttl` to `1h` for OAuth credentials, cap
  at four, repair TTL order) and adds `context_management` with `keep: "all"`,
  which clears nothing. A digest that stays equal proves the client side was
  stable. When it stays equal, the gap is inside the TTL and the cache still
  missed, the cause is downstream of this gateway; the classifier says exactly
  that and leaves the label `unknown`. The response-side TTL split is the
  ground truth of what the upstream wrote.
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

- The event ring is in memory only. A container recreate empties it: export
  first (`/admin/api/forward-events/export?format=json`).
- Before analysing a window, check that the oldest exported event is older than
  the window's start; if not, the ring overwrote part of it.
- Roll back by deploying the previous image. Events recorded by the new build
  are plain JSON with extra fields; nothing persisted depends on them.
