# 00 — Current-state audit

Evidence-only. Source of truth is the tree at `feat/api-key-v2` (branched from
`dadfb59`). Client API keys are distinct from upstream provider keys and from
`Account.KiroApiKey`.

## Module

- Module `kiro-go` (`go 1.24`). Packages: `main`, `config`, `proxy`, `pool`,
  `auth` (Kiro OAuth), `metrics`, `logger`, `search`, `injector`.
- Persistence for app state: `data/config.json` (mode 0600, atomic write + `.bak`).
- `modernc.org/sqlite` is used only by `injector` for the host Kiro CLI DB.
- CI (`.github/workflows/ci.yml`): `go vet ./...`, `go test ./...`, `go test -race ./...`.
  No Makefile.

## Client API key model

`config.ApiKeyEntry` (`config/config.go`):

| Field | Storage | Notes |
|---|---|---|
| `id` | UUID | Stable identity; also memory/responses principal |
| `name` | string | Optional |
| `key` | **plaintext** | Client secret |
| `enabled` | bool | |
| `migrated` | bool | From legacy `config.apiKey` |
| `createdAt` / `lastUsedAt` | unix seconds | |
| `tokenLimit` / `creditLimit` | 0 = unlimited | No request limit, no expiry |
| `tokensUsed` / `creditsUsed` / `requestsCount` | cumulative | Never auto-reset |

Load migration (`config.Load`): if `apiKey != ""` and `apiKeys` empty, one
entry `Name=legacy`, `Migrated=true`, `Enabled=RequireApiKey`. Public deploys
keep the migrated key disabled.

`RequireApiKey` is the **only** master switch. `len(ApiKeys)>0` does **not**
enforce auth. Comment on `Config.RequireApiKey` that claims otherwise is stale.

## Auth

`proxy/auth.go`:

1. `!IsApiKeyRequired()` → `(nil, nil)`.
2. Else if `HasApiKeys()`: Bearer (`"Bearer "` exact prefix) then `X-Api-Key`;
   O(n) `FindApiKeyByValue`; disabled → 401 `"API key disabled"`; over token →
   429 `"token limit exceeded"`; over credit → 429 `"credit limit exceeded"`;
   unknown → 401 `"Invalid or missing API key"`.
3. Else legacy `GetApiKey()`; empty → fail-closed 401; match → `(nil, nil)`
   (no ID, no per-key usage).

Context carries **only** `entry.ID` (`apiKeyIDFromContext`). Secret is never
stored on context.

`/v1/models`, `/health`, `/` are unauthenticated. `/v1/stats` uses
`validateApiKey` and a different error body.

## Request lifecycle (success)

```
ServeHTTP → authenticateFor{Claude,OpenAI} → ResolveRoute
  → forward (upstream Bearer/X-Api-Key; client auth headers stripped)
  → or Kiro pool (ChatExecutor / parseEventStream)
  → usageSplit → recordSuccessForApiKey → config.RecordApiKeyUsage → saveLocked
  → RequestLog ring (500) + metrics.Event (no ApiKeyID)
```

`RecordApiKeyUsage` takes `cfgLock`, increments counters, rewrites **entire**
`config.json` on every success.

## Streaming / cancel

- Kiro: `OnCredits` / `OnComplete` fire only on clean EOF. Cancel → no per-key charge.
- Forward: charge only if `ok && relayErr == nil`. Client cancel → metrics 499,
  no `recordSuccessForApiKey`. Missing usage frame → charge 1 output token.
- Credits: Kiro `meteringEvent.usage` only. Forward credits = 0.

## Telemetry

`metrics.Event` has provider/route/account/IP/status/tokens/`CostUSD`. **No
`ApiKeyID`, no `RequestID`.** Ring 5000, not persisted. Hourly aggregates persist
in `forward_stats.json` every 30s.

SSE (reuse these; do not add a third bus):

- `GET /admin/api/forward-events/stream` — `metrics.Subscribe`, 25s ping, backfill 50
- `GET /admin/api/logs/stream` — `logger.Subscribe`, 25s ping

No WebSocket.

## Admin contract (must keep)

| Method | Path | Notes |
|---|---|---|
| GET | `/admin/api/api-keys` | `{apiKeys:[view]}` masked |
| POST | `/admin/api/api-keys` | `{success,id,key,apiKey}` plaintext once |
| GET/PUT/DELETE | `/admin/api/api-keys/{id}` | DELETE idempotent `{success:true}` |
| POST | `/admin/api/api-keys/{id}/reset-usage` | zeros counters, keeps lastUsedAt |
| GET/POST | `/admin/api/settings` | `requireApiKey`, legacy `apiKey` plaintext |

View fields: `id,name,keyMasked,enabled,migrated,createdAt,lastUsedAt,tokenLimit,creditLimit,tokensUsed,creditsUsed,requestsCount`.

## Frontend

Vanilla IIFE (`web/app.js`). Tabs: accounts, settings, api, forwarding, stats,
console, logs. Client keys live in Settings as cards (`#apiKeysList`). Locales
`apiKeys.*` in en/zh/vi. Admin auth: `X-Admin-Password` + JS cookie
`admin_password` (not HttpOnly). Static files via `http.ServeFile` from `web/`,
not `embed`.

## Risks

1. Plaintext secrets in `config.json`, rewritten on every success.
2. `GET /admin/api/settings` returns legacy `apiKey` in full.
3. O(n) non-constant-time compare.
4. Quota TOCTOU / concurrent overrun.
5. Cancel evades token/credit/request counters.
6. No durable per-key request history.
7. `MaskApiKey` does not mask secrets ≤ 10 chars.
8. Admin password in localStorage / non-HttpOnly cookie (out of V1 scope).

## Compatibility that must survive

Headers, master switch, fail-closed, legacy field, Claude/OpenAI error
envelopes and the four existing messages, admin paths and JSON names,
plaintext-once on create, `0` = unlimited, token 429 before credit 429,
context identity = key ID, do not forward client Authorization, `/v1/models`
unauthenticated, CORS allow-list includes Authorization and X-Api-Key,
`sk-` generation prefix.
