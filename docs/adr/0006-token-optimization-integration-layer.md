# ADR 0006: Token Optimization integration layer

Status: Accepted
Date: 2026-10-21

## Context

Operators want to spend fewer tokens through Kiro-Go. Four open-source projects
are commonly named for this: Headroom, RTK, Caveman and Ponytail. They are not
the same kind of thing, and a popular gateway (9Router) blurs the differences.
Before designing anything, each was read at its latest tag, from source, not
from its README.

| Project | Pinned at | What it actually is | Gateway-usable? |
|---|---|---|---|
| Headroom (Apache-2.0) | v0.41.0 `76133cff` | Python service. `POST /v1/compress` takes `{model, messages, config}` and returns compressed `messages` plus token counts. The default pipeline is marker-free: the reply needs no retrieval round-trip. `config.mode:"ccr"` does need one (`headroom_retrieve` tool, loopback `/v1/retrieve`). Cache-aware operation is `config.frozen_message_count` / `session_id`. | Yes: stateless, per request. |
| RTK (Apache-2.0 + BUSL-1.1) | v1.0.1 `4ef8730c` | Three parts. (1) `rtk` CLI plus agent hooks that rewrite *shell commands* so output is shorter, on the agent's machine. (2) A BUSL-1.1 engine. (3) `rtk-proxy`, a BUSL-1.1 daemon: a transparent hop to ONE upstream fixed at boot per dialect, compressing only the Anthropic dialect, with no standalone compress endpoint, bound to `127.0.0.1:8080` by default. | No. Hooks are client-side; the proxy is a hop, not an API. |
| Caveman (Apache-2.0, trademarked name) | v4.0.1 `d262ab38` | A skill/plugin ecosystem. Its "say less" part is plain prompt text; the installer writes an always-on rule (`src/rules/caveman-activate.md`) derived from `skills/caveman/SKILL.md`. | The prompt text only. |
| Ponytail (MIT) | v5.3.2 `16352ac2` | A skill. One `SKILL.md`; its own hook builds the per-level instruction by dropping frontmatter and other levels' rows. | The prompt text only. |
| 9Router (MIT; reference only) | `870dccdc` | Its "RTK" is a JavaScript port of RTK's filters; its Caveman and Ponytail prompts are "adapted from" the originals with different mode names. Only its Headroom adapter calls the real service. | n/a |

## Decision

Add a small integration layer, package `tokenopt`, behind a Settings group
(Engine mode, Input compression, Output & coding behavior). It owns stable
internal contracts and treats every external engine as an optional, replaceable
adapter. Specifically:

1. **Headroom is the only gateway-level engine.** It is called over HTTP at
   `POST /v1/compress`, in marker-free mode, never `ccr` and never with a session
   id (a stateless gateway cannot serve a retrieval loop or hold per-session
   replay state). Its modes are Headroom's own: *cache* (newest result only,
   earlier messages frozen) and *token*.
2. **RTK is never emulated.** Selecting it changes no request. The panel says
   what RTK really is, shows the verified install steps, health-checks an
   optional `rtk-proxy` URL, and lists the providers whose base URL points at it.
   9Router's approach (a JS reimplementation labelled RTK) is rejected.
3. **Caveman and Ponytail inject the projects' own text**, vendored byte-for-byte
   under `tokenopt/assets/` with tag, commit, sha256 and license, verified at
   startup. Ponytail's level selection is a port of its own builder, pinned by a
   golden test generated from the project's JavaScript. The Classical-Chinese
   `megacave` variant is not offered.
4. **No `Auto` input mode.** It would need a second gateway-level engine to fall
   back to, and there is none. The UI shows it disabled, with the reason.
5. **Kiro-Go owns the request envelope.** An engine is asked only for replacement
   text for tool-result strings. Message order, roles, tool ids, block structure,
   `cache_control` and every unmodelled field come from the client's own bytes. A
   reply is accepted only if its message skeleton matches what was sent; otherwise
   it is discarded.
6. **Fail open, everywhere.** Disabled, unconfigured, unreachable, slow, rejected,
   malformed, structurally wrong, or panicking: the client's original bytes go
   upstream. A circuit breaker stops paying a timeout per request.
7. **Byte stability.** The client re-sends its whole history uncompressed every
   turn. A bounded in-memory memo (per caller, per mode, per content) replays the
   first answer, so earlier messages stay identical turn to turn and the engine is
   only asked about new results. In cache mode the already-answered prefix is also
   sent as `frozen_message_count`.
8. **Where text goes.** Relayed requests get the block appended to the system
   field of the raw body (client text and its cache breakpoints stay first).
   Kiro-native requests get it appended *after* `applyPromptFilters`, because
   `FilterClaudeCode` replaces the whole prompt. The Kiro conversation id is still
   derived from the prompt before the block, so toggling a setting never forks a
   conversation. Text the client already carries (the projects' own sentinels) is
   not added twice.
9. **Config is backward compatible.** The section is omitted while it holds
   defaults; the stored form is canonical (defaults zeroed). Existing installs are
   unchanged.

## Consequences

- Nothing new is required to run Kiro-Go. No downloads, no binaries executed, no
  GitHub access at inference time.
- Savings figures are labelled by provenance (bytes measured by Kiro-Go; token
  counts estimated locally or reported by Headroom). They are not provider usage.
- Observe mode samples in the background, bounded; it does send the conversation
  to the configured engine, which is the service the operator pointed it at.
- Re-pinning an asset means copying the file, updating `manifest.json`, and
  regenerating the Ponytail golden (see the guide).
- Known gaps: the adapter was verified against a stub that follows Headroom's
  source-verified contract, not against a running Headroom; the admin "Test
  connection" does that check end to end on a real deployment. The
  `/v1/messages/count_tokens` endpoint still reports the client's original
  request. Token-opt counters are in memory and reset on restart.

## Alternatives rejected

- **Port RTK/Headroom/Caveman algorithms to Go.** Forbidden by the task and
  wrong: it would present a reimplementation as the official engine.
- **Run `rtk` as a subprocess to compress request text.** It has no such
  command; its hook protocol rewrites shell commands read from agent JSON.
- **One `rtk-proxy` hop built into every forwarded request.** The daemon has one
  fixed upstream, so it needs per-provider wiring; the existing provider Base URL
  already expresses that, with no gateway code.
- **Typed round-trip (`ClaudeRequest` -> JSON) for relayed bodies.** It drops
  fields the proxy does not model (`stop_sequences`, `top_k`,
  `context_management`, ...).
