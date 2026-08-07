# Kiro Prompt-Cache Verification — 2026-07-29

Investigation-only audit. All findings are backed by live-probe evidence captured
with temporary raw-frame instrumentation (since removed) against a dedicated probe
instance on port 9099, isolated via `CONFIG_PATH`. The live `:8080` container was
never touched.

The question: **does prompt caching actually work through Kiro-Go, and should the
proxy pass through real upstream cache token counts instead of the synthetic ones
it currently reports?**

---

## 0. TL;DR

| Claim | Verdict | Confidence |
|---|---|---|
| Client-visible `cache_read_input_tokens` / `cache_creation_input_tokens` are synthetic | **Confirmed** | Very high |
| Any endpoint emits token-level cache counts (`cacheReadInputTokens`) on the wire | **No — never** | Very high |
| Legacy hosts (`q`/`codewhisperer`) emit a `metadataEvent` at all | **No** | Very high |
| Runtime host (`runtime.kiro.dev`) emits `metadataEvent` | **Yes** (but only `{"stopReason":...}`) | Very high |
| Automatic prefix caching is real on `runtime.kiro.dev` | **Yes** | High (6/6 rounds, controlled) |
| Automatic cache requires the `cachePoint` marker | **No — it is automatic** | High |
| Cache keying is content-prefix vs conv-id/session | **Cannot separate** (structural) | — |

**Actionable conclusion:** do NOT build a token-count cache passthrough — upstream
emits no token counts to pass. The only real cache signal is the ~44% credit drop
in `meteringEvent.usage` on a repeated prefix. Keep client-facing cache token
numbers clearly labeled synthetic.

---

## 1. Two backends behave completely differently

Kiro-Go's default endpoints and the current Kiro CLI's endpoint are NOT the same
backend, and they emit different event streams.

| | Legacy (`q.us-east-1.amazonaws.com`, `codewhisperer.us-east-1.amazonaws.com`) | Runtime (`runtime.{region}.kiro.dev`) |
|---|---|---|
| Used by | Kiro-Go default | current Kiro CLI / kirocc |
| Path | `/generateAssistantResponse` | `/` |
| `X-Amz-Target` | `AmazonCodeWhispererStreamingService.GenerateAssistantResponse` | same |
| **Content-Type required** | `application/json` | **`application/x-amz-json-1.0`** |
| Event stream | `assistantResponseEvent`, `contextUsageEvent`, `meteringEvent` | + `initial-response`, **+ `metadataEvent`** |
| `metadataEvent` | **never present** | present, but payload is only `{"stopReason":"END_TURN"}` |
| token usage on wire | none | **none** (no `tokenUsage`, even at 30k prefix + cachePoint) |
| api_key (`ksk_`) auth | accepted | **accepted** (no OIDC required) |

### Reaching the runtime endpoint

POST `https://runtime.{region}.kiro.dev/` with `Content-Type:
application/x-amz-json-1.0`. Plain `application/json` returns a Coral error:

```json
{"Output":{"__type":"com.amazon.coral.service#UnknownOperationException",
  "message":"The requested operation is not recognized by the service."},"Version":"1.0"}
```

The Content-Type is what makes AWS Coral dispatch the operation; the target name
alone is not enough. Header family should be `codewhispererruntime`.

---

## 2. Methodology — closing the "hidden usage" hole

An earlier probe concluded "no upstream cache tokens" by logging *after* the JSON
decode step. That log sat behind a `json.Unmarshal(...) { continue }` guard, so it
could only prove "no JSON-object frame carried usage", not "no frame carried usage".

The fix: log every raw Event Stream frame **before** the JSON guard, capturing
`:event-type`, `:message-type`, `:content-type`, byte length, and payload. This
sees 100% of frames regardless of payload shape or content-type — including
exception frames, array payloads, and unknown event types. With this in place,
"no token usage on the wire" is a real observation, not a parser blind spot.

Result: across the entire session, the only distinct frame types ever seen were
`initial-response`, `assistantResponseEvent`, `metadataEvent`, `contextUsageEvent`,
`meteringEvent`. No frame — decoded or not — ever carried a `tokenUsage`,
`cacheReadInputTokens`, `uncachedInputTokens`, or any token-count key.

---

## 3. Causal cache harness

To decide whether repeating a prefix is genuinely cheaper (vs first-request
warm-up), a controlled harness ran on the runtime endpoint with a **single pinned
account** (cache is per-account; a round-robin pool would lose the hit), `max_tokens=1`
(so output length is fixed and the credit isolates input cost), and **cachePoint
OFF** (testing automatic caching).

Each round fires 4 back-to-back requests over a shared 30k-token prefix `P`:

| Send | Content | conv-id | Isolates |
|---|---|---|---|
| COLD | `nonce_r` + P | new | miss baseline |
| WARM | identical to COLD | same | does a verbatim repeat get cheaper? |
| WNC | same content, salted system prompt | new | content-cache vs conv-id keying |
| CTRL | different `nonce` + P | new | kills the warm-up hypothesis |

`conversationId = SHA1(model + system + firstUserText)`.

### Results (6 rounds, runtime endpoint, cachePoint OFF)

```
        median credit    (max_tokens=1, input-isolated)
COLD    0.309284
WARM    0.172804     <- ~44% cheaper than COLD
WNC     0.319501     ~= COLD
CTRL    0.324877     ~= COLD

paired sign test (n=6 complete rounds):
  WARM < COLD in 6/6 rounds   (deterministic)
  WNC  < COLD in 4/6          (median ~= COLD; noise)
  CTRL < COLD in 2/6          (~= COLD -> NOT warm-up)
```

### Reading

- **WARM cheaper in 6/6** and **CTRL stays at cold price** → the discount is
  content-specific, not a generic "second request is faster" warm-up effect.
- It happens with **cachePoint OFF** → the caching is **automatic**; the
  `cachePoint` marker is not required to activate it.
- The discount is visible ONLY in `meteringEvent.usage` (a credit float), never in
  a token count.

### What could not be separated (structural, not sample size)

Whether the cache is keyed on conv-id/session or on the prefix content. Because
conv-id is derived from `model + system + firstUserText`, you cannot change conv-id
without changing the cached content. The WNC arm salts the system prompt, which
changes both at once, and it lost the discount. Practical upshot is identical
either way: **repeat the request verbatim → cheaper; change anything → cold.**

Open observation (not concluded): COLD drifted cheaper across rounds
(r1 0.498 → r5 0.217) despite a fresh left-anchored nonce each time, hinting the
backend may cache substrings/blocks rather than only a left-anchored prefix. Noisy;
left open.

---

## 4. On the AWS Rust client (schema ≠ behavior)

The official AWS generated client (`aws/amazon-q-developer-cli`) proves the API
*schema* once defined `cachePoint`, `clientCacheConfig`, and
`TokenUsage.cacheRead/WriteInputTokens`. But that repo is deprecated, and the live
runtime backend does not actually emit `tokenUsage`. A schema field existing in a
generated client is not evidence that the live service populates it.

Correction to an earlier claim in this project's history: **"tools-array cachePoint
is the wrong location" was wrong.** Per kirocc, tool-level cachePoint (an entry in
the tools array) and message-level `userInputMessage.cachePoint` are both valid,
distinct cache boundaries. The earlier tools-array experiment was inconclusive
because of the legacy endpoint + small prefix + no controls, not the slot.

---

## 5. Recommendations

1. **Do not implement a token-count cache passthrough.** Upstream emits no token
   counts on any endpoint tried. There is nothing to pass through.
2. **Keep client-facing cache numbers labeled synthetic.** `cache_read_input_tokens`
   / `cache_creation_input_tokens` returned to clients come 100% from
   `proxy/cache_tracker.go`, not upstream.
3. **If surfacing real cache at all, the only signal is the `meteringEvent` credit
   drop** — and it is a credit float, not a token count, so it cannot reconstruct
   an Anthropic-style token breakdown.
4. **Treat any `cachePoint` code as experimental, not load-bearing** — the runtime
   cache is automatic and does not need the marker.
5. Migrating Kiro-Go's default endpoint to `runtime.kiro.dev` is a separate,
   larger decision (different Content-Type, header family, and event shape); this
   audit only established that the runtime endpoint is reachable and behaves
   differently, not that the proxy should switch to it.

---

## 6. Reproduction notes

Temporary instrumentation used during this audit (all env-gated, all since
removed): `[RAW-EVENT]` (raw frame dump before JSON guard), `[RUNTIME-BODY]`
(runtime response body dump), `[ENDPOINT-DIAG]` (host/path/target/auth/status),
`[WIRE-CACHEPOINT]` (cachePoint presence on the wire), `[CACHEPOINT-SET]`,
`[UPSTREAM-CACHE]`. Env flags: `KIRO_RUNTIME_ENDPOINT=1`, `KIRO_RAWFRAME=1`,
`KIRO_CACHEPOINT=1`. Probe ran on `:9099` with an isolated `CONFIG_PATH`; the live
`:8080` container was not modified.
