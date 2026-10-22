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
| `cache.sess`, `cache.parent` | Keyed digests of the session id and of the parent session id in the body's `metadata.user_id`, scoped to the API key. A child's `parent` equals its parent's `sess`. Omitted when the body names none. See [Session fingerprints](#session-fingerprints). |
| `cache.signals` | What the request carried that could classify it (parent session, subagent marker, agent header, probe, title helper, extended-TTL beta), each `present`, `absent` or `not-observable`. See [Classification signals](#classification-signals). |

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
tool arguments or credential is stored, and a session id appears only as the
separate keyed digest described under [Session fingerprints](#session-fingerprints).

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

sessKey   = HMAC-SHA256(pepper, "kiro-go/cache-trace/session/v1")
S(p, id)  = first 12 bytes of HMAC-SHA256(sessKey, be64(len(p)) ‖ p ‖ be64(len(id)) ‖ id)
sess      = S(principal, session_id)       parent = S(principal, parent_session_id)
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
and holds nothing once the event is built. An event gains up to about 1350 bytes
(3000 messages, at most 44 prefix checkpoints and 16 marks, a session and its
parent, every signal; 570 bytes for a 3-message request). Reading the signals
parses `metadata`, `output_config` and `system`, and the one message of a
probe-shaped request; only a title-only output schema makes it read every message.
The benchmark body (300 messages, a short system prompt) cost 9.0 ms before and
after, with 0.8 KB and 13 more allocations.

The event ring holds `metrics.EventRingCapacity` = 12000 events, about 1.9 KB
each with a trace: roughly 23 MB when full, about two days of the traffic this
gateway has seen. The JSON export covers the whole ring.

### Switching it off

`"cacheTrace": false` in `config.json`, then restart. Events then carry no
`cache` object, so no digests, session fingerprints or signals; the TTL split and
`upstreamModel` are plain response telemetry and stay.

## Classifying rewrites

```
python3 scripts/classify_cache_rewrites.py events.json [--min-write 100000] [--sessions]
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

A snapshot kept by `scripts/cache_audit_store.py` (below) is read the same way.

`--sessions` adds a table of the sessions in the events, by digest: requests,
tokens written, the TTL tiers they wrote, the `parentSession` values they carried,
and the parent each names with whether that parent's own events are in the file
(`in events`) or not (`outside`). That is how to tell a child session from a main
one. It needs events recorded with session digests.

## Classification signals

`cache.signals` records facts that a gateway further upstream may use to classify
a request, for instance to decide how long its cache lives. They are what this
gateway **observed**, not what that gateway decided: the event says nothing about
whether anyone acted on a signal, and a signal this gateway does not forward is
still recorded as seen.

| Field | Read from | Present when |
|---|---|---|
| `userId` | body `metadata.user_id` | not a signal; its shape: `json` (the envelope gateways write), `native` (`user_<hex>_account_<uuid>_session_<uuid>`), `invalid` (present, neither, or over 4096 bytes), `none` |
| `parentSession` | body `metadata.user_id` | it names `parent_session_id`: the JSON envelope has the key, an object-form `user_id` has it, or the text holds `"parent_session_id"` |
| `subagentMarker` | body `system` | `system[0].text` (or a string `system`) holds `cc_is_subagent=true` |
| `agentHeader` | client request headers | `X-Claude-Code-Agent-Id` or `X-Claude-Code-Parent-Agent-Id` was sent. **Never forwarded**: this gateway's header allow-list leaves them out, so the next hop cannot see them |
| `probe` | body | one token allowed, no tools, one short user message such as `quota`, `test`, `.`, `probe` |
| `titleHelper` | body | the request asks for a session title: a system prompt holding the instruction, or a title-only output schema |
| `extendedTtlBeta` | `Anthropic-Beta` as forwarded | it lists `extended-cache-ttl-2025-04-11` |

A breakpoint that asks for `ttl: "1h"` is already in `cache.marks` (`m5:1h`).

**The three values.** `present`: the signal was in the data checked. `absent`: the
data was checked in full, in a form that can carry the signal, and it is not
there. `not-observable`: this gateway cannot tell. That is the case for
`parentSession` when `userId` is `native`, `none` or `invalid` (those forms cannot
carry a parent, so only the JSON envelope can show there is none), and for the two
header fields when the client's headers were not available. Do not read
`not-observable` as `false`.

To ask whether CLIProxyAPI v8.0.8 would have treated a request as a subagent from
what it was sent, use `parentSession = present` or `subagentMarker = present`;
`agentHeader` cannot reach it. That version also keeps a subagent's `1h` marker
when `extendedTtlBeta = present` or a mark says `:1h`, and strips the TTL of a
probe or title helper.

The body-derived rules follow that version's `claude_diagnostics.go`. They were
run against it over 1598 generated bodies: `parentSession`, `subagentMarker`,
`agentHeader` and the `1h` request agreed on every one, and `probe` and
`titleHelper` on every one except two inputs the Messages API rejects (a
`max_tokens` sent as the string `"1"`, and messages with the role `system`),
which are read as not a probe and not a helper. Requests with no message are not
traced at all.

## Session fingerprints

`cache.sess` and `cache.parent` exist to answer one question: is this
conversation a child of another? A child's body names its parent in
`parent_session_id`; the parent's own events carry that id as their session.
Digesting both with one key, one framing and one scope makes them equal exactly
when they are the same session of the same API key, so the link can be read from
events that hold no session id.

- Key and scheme: see [Digests](#digests). The session key is a separate
  derivation from the content key, so a session digest never equals a content
  digest of the same bytes.
- Scope: the API key id goes into every digest. One session id sent through two
  keys gives two unrelated digests, so a digest cannot tie one person's sessions
  to another's. Requests with no API key share the scope `""`.
- Normalisation: an id in canonical UUID form is lower-cased first; any other id
  is used as sent. Both digests pass through the same function.
- Source: the body's `metadata.user_id` only, parsed by the same rules as session
  affinity (the JSON envelope's `session_id`; the native form's UUID). The
  session headers are not consulted, so a request whose body carries no usable
  `user_id` has no `sess`, even when a header named a session. Omitted means
  unknown; no id is ever made up.
- A stable digest is a pseudonym, not anonymity. It cannot be reversed to the id,
  but it links one session's events to each other (that is its use). Treat an
  export that holds it as private data: see the store below.

## Durable audit store

The ring is memory only. `scripts/cache_audit_store.py` keeps what the analysis
needs outside the repository, in `~/.kiro-go-audit/` by default:

```
KIRO_GO_ADMIN_PASSWORD=... python3 scripts/cache_audit_store.py save --url http://127.0.0.1:8080
python3 scripts/cache_audit_store.py save --source events.json     # an export you already have
python3 scripts/cache_audit_store.py prune [--dry-run]
```

`save` writes one snapshot, `exports/audit-<UTC time>.json`, then prunes. It keeps
an allowlist of fields: time, model, status, latency, TTFB, token usage including
the 5m/1h split, the `cache` object (digests, marks, signals, `sess`, `parent`)
and the session source and scope. It drops the API key name and id, the client
address, the connection name and id, the request id, the provider and route ids,
the error text, and any field it does not know. A credential label is replaced by
a token that is only comparable inside that snapshot, so `scope_changed` still
works. A snapshot is an export to the classifier.

- Directory 0700, files 0600, whatever the caller's umask. A directory inside a
  git work tree, or one that is not empty and not a store, is refused.
- Retention: snapshots older than 30 days are deleted, then the oldest until the
  store is under 400 MB. A snapshot younger than 48 hours is never deleted, so an
  audit window that is still running keeps its data; if the cap cannot be met
  without it, `prune` says so and exits 2.
- Pruning runs at the end of every `save`. Nothing schedules `save`: run it once a
  day, and before a container recreate, which empties the ring. Snapshots overlap
  (the ring holds about two days) and are self-contained on purpose.
- The admin password comes from `KIRO_GO_ADMIN_PASSWORD`, never from an argument.

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
  between. Its Claude executor can rewrite `cache_control`. For the TTL rules
  the source was read at the deployed tag, v8.0.8: a marker with no `ttl` is
  upgraded to `1h` for OAuth credentials, except on a subagent, probe or title
  helper (see [Classification signals](#classification-signals)), whose TTL is
  stripped unless it asked for `1h`. The other rewrites (inject breakpoints, cap
  at four, repair TTL order) and the `context_management` it adds with
  `keep: "all"`, which clears nothing, are from v7.2.144 and were not checked
  against v8.0.8. A digest that stays equal proves the client side was
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
  first (`/admin/api/forward-events/export?format=json`), or `save` a snapshot to
  the audit store.
- Before analysing a window, check that the oldest exported event is older than
  the window's start; if not, the ring overwrote part of it.
- Roll back by deploying the previous image. Events recorded by the new build
  are plain JSON with extra fields; nothing persisted depends on them.
