# Provider Error Boundary

Status: Accepted
Date: 2026-08-18

## Context

Custom/upstream providers return detailed failures (invalid API key, account
suspension, quota, request IDs, raw JSON/HTML). Those strings were reaching
API clients via `io.Copy` of error bodies (forward path) and `lastErr.Error()`
(Kiro pool), including `KiroUpstreamError` which embeds the raw body. Clients
could confuse an upstream 401 with a Kiro-Go API-key failure.

Admin still needs high-fidelity diagnostics, correlated by `requestId`.

## Decision

1. New `providererr` package owns structurally separate types:
   - `InternalError` — parsed once, never serialized to public APIs
   - `PublicError` — small `{code, message, requestId, httpStatus}`
2. Public taxonomy reuses `apikey` codes, adding `provider_unavailable` and
   `provider_rejected`. Local gateway errors (invalid key, quota, validation)
   are unchanged.
3. Protocol renderers (`sendPublicClaudeError`, `sendPublicOpenAIError`,
   Responses `response.failed`, SSE filters) consume only `PublicError`.
4. Admin diagnostics persist in `provider_error_details` (same SQLite as API
   keys, same retention). Portal `request_events` never joins this table.
5. Credentials are redacted before persist/log/admin render. Diagnostic
   messages (e.g. account suspended) remain for admin.
6. Upstream 401/403 map to HTTP 502 `provider_error`, not client 401.
   Provider 429 is `provider_rate_limited`; API-key quota 429 keeps
   `token_quota_exceeded` / `credit_quota_exceeded` / `request_quota_exceeded`.
7. Retry/failover still uses internal classification (`retryableUpstreamStatus`,
   `KiroUpstreamError.Category`) before sanitization.

## Alternatives rejected

- A single struct with `json:"-"` on raw fields — other serializers can leak.
- Relaying upstream error JSON “because OpenAI clients expect it” — that is
  the leak. Envelope shape is preserved; content is generic.
- Storing raw bodies on `request_events` — portal already selects that table.

## Consequences

- Forward non-2xx no longer copies the upstream body to the client.
- Kiro exhaustion HTTP status for 5xx upstreams is 502, not 500.
- Clients should quote `request_id` / `X-Request-Id` for support.
