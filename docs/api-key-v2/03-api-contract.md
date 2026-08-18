# 03 — API contract

## /v1 compatibility (unchanged envelopes)

Claude: `{type:"error",error:{type,message}}`  
OpenAI: `{error:{type,message}}`

| Condition | Status | type | message |
|---|---|---|---|
| missing / unknown | 401 | authentication_error | Invalid or missing API key |
| disabled | 401 | authentication_error | API key disabled |
| expired | 401 | authentication_error | API key expired |
| token quota | 429 | rate_limit_error | token limit exceeded |
| credit quota | 429 | rate_limit_error | credit limit exceeded |
| request quota | 429 | rate_limit_error | request limit exceeded |
| no keys configured | 401 | authentication_error | API key authentication is required but no keys are configured |

`/v1/stats` still returns `{"error":"Invalid or missing API key"}` on any auth
failure.

## Admin — existing

Unchanged paths and existing JSON field names. Additive fields on `apiKey` view:

`requestLimit`, `expiresAt`, `resetPolicy`, `enforcementMode`, `status`,
`tokensRemaining`, `creditsRemaining`, `requestsRemaining`, `hasPortalToken`,
`requestsSuccess`, `requestsFailed`, `requestsCancelled`, `requestsRejected`,
`requestsAttempted`, `requestsQuotaConsumed`,
`tokenEnforcement` (always `"soft"`), `creditEnforcement` (always `"soft"`),
`requestEnforcement` (`"reserved"` or `"none"`).

`enforcementMode=strict` is stored but **does not** hard-limit tokens or credits
in V1. Request limits are always reserved exactly when set.

`requestsCount` remains = `requests_total` (= quota consumed) for old UI.

### New admin routes

| Method | Path | Body / result |
|---|---|---|
| POST | `/admin/api/api-keys/{id}/rotate` | `{success,id,key,apiKey}` |
| POST | `/admin/api/api-keys/{id}/portal-token` | `{success,token,url,expiresAt}` once |
| DELETE | `/admin/api/api-keys/{id}/portal-token` | `{success:true}` |

List query: `q`, `status` (`active|disabled|expired|exhausted`), `quota`
(`unlimited|credits|tokens|requests`), `usage` (`high|never|recent`),
`sort` (`created_desc` default, `name`, `last_used`, `usage`), `offset`, `limit`
(default 50, max 200). Empty query still returns `{apiKeys,total}`.

Create/update accept optional `requestLimit`, `expiresAt` (unix seconds),
`resetPolicy`, `enforcementMode`.

## Portal

Identity comes from cookie `portal_session` (HttpOnly, SameSite=Lax, Path=/portal,
Secure when TLS). **No `keyId` query parameter is honored.**

| Method | Path | Auth | Result |
|---|---|---|---|
| POST | `/portal/api/session` | body `{key}` | sets cookie, `{ok:true}` |
| POST | `/portal/api/session/token` | body `{token}` | same (used by `/usage/p/…` exchange) |
| DELETE | `/portal/api/session` | cookie | clears cookie |
| GET | `/portal/api/me` | cookie | name, masked, status, expiry |
| GET | `/portal/api/summary` | cookie | cards + remaining + next reset |
| GET | `/portal/api/usage?range=` | cookie | hourly series |
| GET | `/portal/api/events` | cookie | paged history |
| GET | `/portal/api/events/stream` | cookie | SSE public events |

Portal JSON errors: `{error,code}` with codes `invalid_api_key`,
`api_key_disabled`, `api_key_expired`, `invalid_portal_token`,
`portal_token_expired`, `portal_session_expired`, `internal_error`.

`GET /usage` serves `web/usage.html`.  
`GET /usage/p/{token}` exchanges the portal token for a session and 302s to
`/usage` (token must not remain in the address bar).

## Public event DTO

```json
{
  "eventId": 1,
  "requestId": "...",
  "apiKeyId": "...",
  "timestamp": "2026-08-18T05:00:00Z",
  "endpoint": "openai",
  "clientModel": "claude-sonnet-4.5",
  "effectiveModel": "claude-sonnet-4.5",
  "model": "claude-sonnet-4.5",
  "statusCode": 200,
  "status": "success",
  "inputTokens": 100,
  "outputTokens": 20,
  "totalTokens": 120,
  "credits": 0.12,
  "latencyMs": 800,
  "ttfbMs": 120,
  "streaming": true,
  "errorCode": "",
  "usageSource": "upstream",
  "usageEstimated": false
}
```

Public `errorCode` taxonomy: `validation_error`, `authentication_failed`,
`api_key_disabled`, `api_key_expired`, `request_quota_exceeded`,
`token_quota_exceeded`, `credit_quota_exceeded`, `no_available_accounts`,
`provider_rate_limited`, `provider_error`, `provider_timeout`,
`client_cancelled`, `server_timeout`, `internal_error`,
`unsettled_reservation`.

Never includes provider, account, route, credentials, prompt, or raw upstream
errors.
