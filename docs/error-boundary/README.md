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

## Known limitations

- Compressed non-JSON error bodies on a **200** stream that never form SSE frames larger than 64 KiB could still pass through; those are treated as truncated/replaced when the buffer exceeds the parse cap.
- Kiro `Error()` still includes the raw body for legacy failover substring matching; it is not sent to clients.
