# Token Optimization

Optional settings (Settings tab > Token Optimization) that shrink what Kiro-Go
sends upstream or steer how the model answers. Everything is **off by default**
and an install that never touches it behaves exactly as before: the config file
is unchanged and requests are forwarded byte for byte. The design and the
verified facts behind it are in [ADR 0006](adr/0006-token-optimization-integration-layer.md).

## What each integration really is

| Integration | What it does here | Runs where | Needs |
|---|---|---|---|
| **Headroom** | Replaces large tool results (command output, logs, JSON) with shorter text, via Headroom's `POST /v1/compress`. | Gateway, per request | A running `headroom proxy` |
| **RTK** | Nothing, inside Kiro-Go. Its hooks shorten shell output on the machine where the agent runs; its optional `rtk-proxy` is a hop you point a provider at. Selecting RTK changes no request. | Agent machine / your own daemon | `rtk` on the agent machine |
| **Caveman** | Appends the project's official response-style text to the system prompt. | Gateway | Nothing |
| **Ponytail** | Appends the project's official coding-behavior text to the system prompt. | Gateway | Nothing |

Caveman and Ponytail apply only the projects' prompt text. Their hooks, tools
and slash commands are not part of it, and Kiro-Go does not claim otherwise.

**Auto** is shown disabled: it would need two engines that can compress on the
gateway, and only Headroom can.

### Supported request formats

Claude Messages (`/v1/messages`), OpenAI Chat (`/v1/chat/completions`) and
OpenAI Responses (`/v1/responses`), on both paths: relayed to an external
provider, and translated to Kiro. Streaming is untouched: only the request is
changed, before anything is sent.

### What is never compressed

Tool results from file-reading and editing tools (`Read`, `Edit`, `Write`,
`read_file`, `apply_patch`, ... the full list is shown in the panel). The model
needs those byte for byte, and a later edit would fail to match otherwise.
Also never sent: error results (`is_error`), image blocks, and results smaller
than the configured minimum.

## Engine mode

| Mode | Behavior |
|---|---|
| **Off** | Nothing changes, nothing is measured. Settings are saved but inactive. |
| **Observe** | In the background, on a bounded sample of requests, asks Headroom what it *would* save, and discards the answer. Requests go out exactly as received; no prompt is added. Prompt styles cannot be simulated and show as *not observable*. |
| **On** | Applies the selected integrations. A failure never blocks a request. |

## Setup

### Headroom

1. Install and run it where Kiro-Go can reach it. From its README:
   `pip install "headroom-ai[proxy]"` then `headroom proxy --port 8787`, or the
   image `ghcr.io/headroomlabs-ai/headroom`. Tested against **v0.41.0**.
2. In Settings choose *Input compression: Headroom*, enter the service URL
   (`http://127.0.0.1:8787`), press **Test connection**, then set *Engine mode*
   and **Save**.

Headroom's own access rules matter, and they are the usual cause of a failed test:

- `/v1/compress` is **loopback-only** by default: it checks the caller's address
  *and* the `Host` header. On the same host, `http://127.0.0.1:8787` works.
- From a **container** (Kiro-Go in Docker, Headroom on the host) use
  `http://host.docker.internal:8787`, start Headroom with
  `HEADROOM_COMPRESS_ALLOW_REMOTE=1` and `HEADROOM_PROXY_TOKEN=<token>`, and put
  the token in *Proxy token*. Headroom refuses a non-loopback bind with no token.
- Headroom's telemetry is opt-in, but its update check runs unless disabled.
  `HEADROOM_OFFLINE` turns off everything Headroom decides to fetch on its own.

Run Headroom with its default (*coding* / cache) profile. Cache-aware mode relies
on Headroom's own protection of file reads, which Kiro-Go cannot enforce.

| Setting | Meaning |
|---|---|
| Mode: *Cache-aware* (default) | Compress only the newest tool result; earlier messages stay byte-identical so the provider's prompt cache keeps working. Sends `protect_recent: 0` and `frozen_message_count`. |
| Mode: *Token-saver* | Let Headroom also rewrite older results. Saves more; can lower prompt-cache hits. |
| Timeout | Per call (200 to 30000 ms, default 3000). On timeout the request goes out unchanged. |
| Minimum size | Results smaller than this are never sent (default 4096 bytes). |

### RTK

RTK cannot compress requests inside Kiro-Go.

- **Agent side (the supported mode):** on the machine running your coding agent,
  `brew install rtk`, `rtk init -g`, restart the agent, run `rtk gain` to confirm.
  Kiro-Go can neither apply nor check this.
- **Optional rtk-proxy:** a transparent daemon for one fixed upstream, Anthropic
  format only. Start it with `RTK_PROXY_ANTHROPIC_UPSTREAM` set to your provider
  and a free port in `RTK_PROXY_BIND` (its default, 8080, is Kiro-Go's), then set
  that provider's *Base URL* to the daemon. Enter the daemon's URL in the RTK
  panel to see whether it is reachable and which providers route through it.
- Licensing: `rtk-proxy` and the RTK engine are BUSL-1.1 (free for production
  use; not for offering as a service). Kiro-Go neither bundles nor runs them.

### Caveman and Ponytail

Pick a style (*Caveman*, *Ultracave*) and/or a level (*Lite*, *Full*, *Ultra*).
Nothing to install. The block is appended after your own system text and the
prompt filters; Ponytail first, then Caveman. If the client already carries the
same text (detected by the projects' own marker lines) it is not added twice.

## Verifying it works

1. **Test connection** in the Headroom panel checks liveness (and reads the
   version) *and* makes a real one-line compress call; liveness alone proves
   neither the token nor the loopback rules.
2. The *Runtime status* block shows what is true now: connected or not, active or
   not, circuit state, last check, version vs. the tested one, and counters.
3. Inspect what a provider receives: point a model route at a recording server
   and send a request with a large tool result.

## Failure behavior

| Situation | Result |
|---|---|
| Mode off, engine none/RTK | Original bytes. The engine is never called. |
| Engine unconfigured, unreachable, slow, 4xx/5xx, malformed reply | Original bytes; counted with a reason. |
| Reply whose messages do not match what was sent (count, roles, tool ids) | Whole reply discarded; counted as a failure. |
| Replacement not smaller, empty, or invalid text | That result is left as sent. |
| 3 consecutive failures | Circuit opens for 30 s (no per-request timeout), then one trial call. |
| Client hangs up | Not counted against the engine. |
| Panic inside the layer | Contained; original bytes. |

A streamed response is never retried: nothing here happens after the first
response byte.

## Prompt cache and conversation identity

- Earlier messages are replayed from the memo, so they are the same bytes every
  turn. The memo is bounded (4096 entries / 64 MiB), in memory, scoped per API
  key, mode and content. A restart costs one recompression, never correctness.
- The Kiro conversation id comes from the prompt *before* the behavior block, so
  turning a setting on or off does not start a new conversation.
- Toggling a setting changes the prompt once, so the provider's cache is cold for
  that one turn.

## Security

- The service URL must be absolute `http(s)`, with no credentials, query or
  fragment. Connections to link-local, multicast, unspecified and cloud-metadata
  addresses are refused **at connect time** (so DNS rebinding cannot swap one in);
  loopback and private ranges are allowed, since the engine is meant to be local.
- No redirects are followed, and the outbound Kiro proxy setting is not used.
- Replies larger than 8 MiB, and requests larger than that, are not exchanged.
- The Headroom token is sent only as `x-headroom-proxy-token`, never returned by
  the API (even short tokens are fully masked), never logged, and "Test
  connection" never sends the saved token to an address you only typed.
- Errors and logs carry the failure kind, never request or response content.

## Observability

`GET /admin/api/token-optimization` returns the saved settings, the runtime state
and counters; `?probe=1` refreshes stale health first.

- Counters are keyed `engine x outcome x reason` (a small closed set).
- Byte figures are measured by Kiro-Go on the text it replaced. Token figures are
  estimates, from a local heuristic and from Headroom's own tokenizer, shown
  separately. **Neither is provider usage**, which is unchanged in the usual stats.
- *Claimed savings that changed nothing* counts engine replies that reported
  savings while nothing usable came back.
- Counters are in memory and reset on restart.

## Admin API

| Route | Purpose |
|---|---|
| `GET /admin/api/token-optimization[?probe=1]` | `{settings, runtime, limits}` |
| `POST /admin/api/token-optimization` | Partial update. Validation failures return `400 {error, field}`. The token: omit or send the mask to keep it; `headroom.clearApiKey:true` removes it. |
| `POST /admin/api/token-optimization/test` | `{target:"headroom"\|"rtk", ...}`; unsaved values may be tested. |

Updates run under one lock, so concurrent saves of different fields all land.

## Updating a vendored asset

`tokenopt/assets/manifest.json` pins each file (repo, tag, commit, path, sha256).
To move to a new release: copy the new upstream file over the vendored one,
update the manifest entry, and for Ponytail regenerate
`tokenopt/testdata/ponytail-official.json` (the command is in the comment above
`TestPonytailMatchesOfficialBuilder`; it runs the project's own script under
Node's `--permission` mode). `go test ./tokenopt` fails if a file no longer
matches its hash or the port drifts from the project's builder. Licenses and
notices ship with the assets; see [third-party notices](third-party-notices.md).

## Tests

`go test ./config ./tokenopt ./proxy` covers settings persistence and
migration, concurrent updates, prompt composition against the official builder,
envelope preservation (unknown fields, numeric literals, tool ids, ordering),
the structural guard, memo and cache stability, breaker and timeout behavior,
SSRF controls, observe mode, the Claude/OpenAI/Responses relay paths, the
Kiro-native translator, streaming integrity and the admin API. No unit test needs
the network or a third-party service: Headroom is a local stub that follows the
contract read from its source. Checking a real deployment is what **Test
connection** is for.

## Limitations

- Verified against Headroom v0.41.0's source and a stub, not a live Headroom run.
- RTK is guidance and a health check, by design.
- `/v1/messages/count_tokens` reports the client's original request.
- Observe samples at most two requests at a time and drops the rest.
