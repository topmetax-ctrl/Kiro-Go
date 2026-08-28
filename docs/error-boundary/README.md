# Provider error trust boundary

## Current-state leak audit (before this change)

| Path | Raw error appeared | Client | Admin | Portal |
|---|---|---|---|---|
| Custom forward non-2xx | `writeUpstreamResponseBytes` copied body + status | **leak** | metrics `errorMsg` | `errorCode` only |
| Forward stream | verbatim SSE, including `data: {"error":...}` | **leak** | metrics | errorCode |
| Kiro pool exhaustion | `lastErr.Error()` → `KiroUpstreamError` body + endpoint | **leak** | RequestLog | errorCode via ClassifyPublicError |
| Kiro/OpenAI/Responses mid-stream | SSE/chunk with `err.Error()` | **leak** | RequestLog | errorCode |
| Web search default | `"Web search failed: "+err.Error()` | **leak** | RequestLog | — |
| Transport failure | already generic `"upstream request failed"` | OK | log with dial text | — |
| Local API-key auth/quota/validation | stable messages | OK (intentional) | — | taxonomy codes |

## Trust boundaries

```
Provider HTTP / Go error
        │
        ▼
  providererr.FromHTTP / FromNetwork     ← parse once
        │
        ▼
   InternalError (never public JSON)
        │
   ┌────┴─────────────────────┐
   ▼                          ▼
Admin store               PublicError
provider_error_details    code + generic message + requestId
(admin password)          │
                          ├─ OpenAI renderer
                          ├─ Anthropic renderer
                          └─ Responses renderer
```

## Classification table

| Internal class | Upstream | Public HTTP | Public code | Message | Internal retry |
|---|---|---|---|---|---|
| rate_limited | 429 | 429 | `provider_rate_limited` | The upstream service is temporarily rate limited. | yes |
| timeout | 408/504, deadline | 504 | `provider_timeout` | The upstream service timed out. | yes |
| unavailable | DNS/TLS/refused, 503 | 502/503 | `provider_unavailable` | The upstream service is temporarily unavailable. | yes |
| rejected | 400/404/409/422 | 400/404/409/422 | `provider_rejected` | The upstream service rejected the request. | no |
| error (incl. upstream 401/403) | 401/403/500/502 | **502** | `provider_error` | An upstream service error occurred. | 5xx yes; 401/403 no |
| canceled | context cancel | 499 | `client_cancelled` | (no body if client gone) | no |

API-key quota remains 429 + `token_quota_exceeded` / `credit_quota_exceeded` / `request_quota_exceeded` with the existing messages.

## Streaming

- **Before headers:** same JSON error as non-stream.
- **After stream start:** HTTP status stays 200. Claude gets `event: error` with the public message. OpenAI chat gets a `data: {"error":...}` chunk (no `[DONE]`). Responses gets `response.failed`. Forward SSE frames that look like errors are rewritten; content frames pass through.

## Admin diagnostics

- `GET /admin/api/provider-errors?requestId=` (admin password / cookie).
- Forwarding event detail panel loads the same payload.
- Storage: `provider_error_details`, 16 KiB cap, same retention as `request_events`.
- Secrets redacted: `Authorization`, Bearer, `sk-`/`pt-`, `api_key` / `access_token` / `cookie` assignments.

## Admin probes (Test buttons)

The panel's three Test buttons — a provider's model browser, a key in the pool
manager, a route target — all `POST /admin/api/upstream-test` and all run the
SAME classification the forward path runs, via `providererr.Diagnose*`.

`Diagnose*` is the unmetered twin of `From*`: identical parse / classify /
redact, but it does not touch the classification counters. A probe is an
operator's question, not upstream traffic, so twelve clicks on Test must not
read back as twelve upstream failures on the dashboards. `FromHTTP` /
`FromNetwork` remain the metered entry points and are now thin wrappers over the
same core, so the two paths cannot drift apart.

What the endpoint reports on a failure:

| Field | Meaning |
|---|---|
| `category` | internal class (`rejected`, `rate_limited`, `unavailable`, `timeout`, `error`) |
| `retryable` | whether the forward path would try the next target |
| `status` | upstream HTTP status; absent when nothing answered |
| `kind` | transport failure (`dns`, `tls`, `refused`, `eof`, `timeout`); absent when the upstream answered |
| `code` / `message` | the upstream's own error code and message |
| `error` | the redacted, capped upstream body; absent when it sent none |
| `upstreamRequestId` / `retryAfter` | from the upstream's response headers |

Successful probes report none of these — the panel decides what to render from
which fields exist, so a category on a healthy probe would open an empty "why"
panel on a green row.

The body reaches the panel only through `Redact` + the 16 KiB cap. This matters
because some gateways echo the refused request back inside their 400, which
would otherwise hand the panel the `Authorization` header we just sent (test:
`TestUpstreamTestRedactsEchoedCredentials`). Proxy URL userinfo
(`http://user:pass@host`) is redacted too, keeping the host: a dial failure
through a configured proxy carries that credential in its error text.

Client-facing behaviour is unchanged by this: `/admin/api/upstream-test` is
admin-only, and no `PublicError` renderer reads these fields.

## Known limitations

- Compressed non-JSON error bodies on a **200** stream that never form SSE frames larger than 64 KiB could still pass through; those are treated as truncated/replaced when the buffer exceeds the parse cap.
- Kiro `Error()` still includes the raw body for legacy failover substring matching; it is not sent to clients.
