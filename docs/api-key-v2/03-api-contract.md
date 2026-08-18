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
| POST | `/admin/api/api-keys/batch` | `{count,name?,…quota}` → `{success,count,keys:[{id,name,key,apiKey}]}` once; `count` 1–100 |
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
| POST | `/portal/api/session` | body `{key,remember?}` | sets cookie, `{ok:true}`. `remember=true` persists 30 days; otherwise a session cookie (server TTL 12h) |
| POST | `/portal/api/session/token` | body `{token}` | same (used by `/usage/p/…` exchange) |
| DELETE | `/portal/api/session` | cookie | clears cookie |
| GET | `/portal/api/me` | cookie | name, masked, status, expiry |
| GET | `/portal/api/summary` | cookie | cards + remaining + next reset |
| GET | `/portal/api/usage` | cookie | chart series (see filter + resolution) |
| GET | `/portal/api/events` | cookie | cursor-paged raw history |
| GET | `/portal/api/events/stream` | cookie | replayable SSE public events |

### Unified filter query

`range`, `from`, `to`, `model`, `endpoint`, `status`, `streaming`,
`error_code` apply to **usage, events, and SSE**. `keyId` / `apiKeyId`
are ignored. The key is always the portal session principal.

| Param | Notes |
|---|---|
| `range` | `LIVE` `1H` `6H` `24H` `7D` `30D` `CUSTOM` (default `LIVE` on the portal; API still defaults empty range to 24H) |
| `from` `to` | unix seconds UTC; required for `CUSTOM`; `from < to`; max 365 days |
| `model` | exact match on `client_model` or `effective_model` |
| `endpoint` | exact (`openai` / `claude` / `responses`) |
| `status` | `success` `failed` `cancelled` `rejected` |
| `streaming` | `true`/`false` (`stream` accepted as alias) |
| `error_code` | public taxonomy (`error` accepted as alias) |
| `metric` | chart allowlist only; never interpolated into SQL |
| `cursor` `limit` | history keyset pagination (`limit` default 50, max 200) |
| `after` / `Last-Event-ID` | SSE replay cursor = `eventId` |

Timestamps in the API are UTC. The frontend may display local time.

### Raw vs aggregate

`request_events` is the raw store (~30 days). `usage_hourly` is the
aggregate store (~365 days). Request history never pretends to have
row-level data past raw retention. Charts pick a resolution:

| Span | Unfiltered | Dimension filter |
|---|---|---|
| ≤ 6h inside raw window | 1m from `request_events` | 1m from `request_events` |
| ≤ 24h inside raw window | 5m from `request_events` | 5m from `request_events` |
| ≤ 7d | 1h from `usage_hourly` | 15m from `request_events` (clipped to raw) |
| longer | 1h from `usage_hourly` | 1h from `request_events` (clipped to raw) |

Responses include `resolution`, `bucketSeconds`, `source`, `truncated`,
`rawAvailableFrom`, `dataRetention`.

Chart metrics allowlist: `requests_attempted`, `requests_quota_consumed`,
`input_tokens`, `output_tokens`, `total_tokens`, `credits`,
`success_rate`, `latency`, `ttfb`. All series are returned; `metric=`
only validates.

History pagination is `ORDER BY ts DESC, id DESC` with an opaque cursor
encoding `(ts, id)`. `event_id` is SQLite AUTOINCREMENT, so it is a
stable identity. UUID `requestId` is not used as a cursor.

### SSE

```
id: 105
event: request
data: {public event}

: ping
```

Reconnect sends `Last-Event-ID`. Handoff is subscribe-first, then
high-water, then replay `(lastId, highWater]`, then live with
`eventId` dedupe. Replay is capped at 500 matching events. If the
matching backlog exceeds the cap the stream stays **200** (a 4xx
would stop `EventSource`) and emits:

```
id: <highWater>
event: sync_required
data: {"reason":"replay_truncated","lastEventId":N,"highWater":M,"replayed":K}
```

The client must refetch summary + history + series, then continue
live. `id: highWater` advances `Last-Event-ID` so the next reconnect
does not re-request the same overflow. A slow client is disconnected
(bounded buffer) and must replay. Session is re-checked every 5s;
logout, disable, token revoke, and TTL close the stream. Heartbeats
are SSE comments, not fake request events. The stream opens with
`retry: 3000`.

`ttfbMs` is nullable. Unknown is `null`, never `0`. `0` means a
measured zero.

Portal JSON errors: `{error,code}` with codes `invalid_api_key`,
`api_key_disabled`, `api_key_expired`, `invalid_portal_token`,
`portal_token_expired`, `portal_session_expired`, `invalid_range`,
`range_too_large`, `invalid_cursor`, `invalid_metric`, `invalid_filter`,
`internal_error`.

`GET /usage` serves `web/usage.html`.  
`GET /usage/p/{token}` exchanges the portal token for a session and 302s to
`/usage` (token must not remain in the address bar).

## Public event DTO

```json
{
  "eventId": 1,
  "requestId": "...",
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

Portal responses omit `apiKeyId` (`PortalView` clears it). Never includes
provider, account, route, credentials, prompt, or raw upstream errors.

`ttfbMs` is time to the first byte written to the client (including a fast
error envelope). Unknown is `null`. A measured zero is `0`. Admissions that
never reached a provider can still show `0` if the 4xx/5xx body was written
in the same millisecond.
