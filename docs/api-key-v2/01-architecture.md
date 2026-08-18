# 01 — Architecture

See also [ADR 0002](../adr/0002-api-key-v2.md).

## Before

HTTP handler mutates `config.ApiKeys` and writes the whole JSON file. Usage,
secrets, and admin password share one lock and one file.

## After

```
HTTP
 ├── /v1/*  → authenticate → Key principal (id, status, quota)
 │                 ├── Quota.Reserve (request slot, if limited)
 │                 └── execute → Commit / Reconcile
 ├── /admin/api/api-keys* → APIKeyService (admin password, unchanged)
 └── /portal/api/* + /usage → PortalService (session cookie)
         │
         ▼
    apikey.Service
         ├── KeyRepository / UsageRepository / EventRepository / PortalRepository
         └── SQLite (data/apikeys.db)
    metrics.Record ── Subscribe ── admin SSE + portal SSE (filtered + sanitized)
```

## Package boundaries

| Package | Allowed to import | Responsibility |
|---|---|---|
| `apikey` | stdlib, `modernc.org/sqlite`, `google/uuid` | Domain + SQLite. No HTTP. |
| `config` | (existing) | Master switch, pepper persistence, legacy plaintext snapshot |
| `proxy` | `apikey`, `config`, `metrics` | Auth adapter, admin/portal HTTP, commit hooks |
| `metrics` | (no `apikey`) | Additive `RequestID` / `ApiKeyID` on `Event` |

`apikey` must not import `proxy` or `config`.

## Handler wiring

`NewHandler` opens `GetConfigDir()/apikeys.db`, loads pepper, imports
`config.ListApiKeys()` (idempotent), assigns `h.keys`. `Shutdown` closes the DB
and stops retention.

`&Handler{}` in unit tests keeps the previous `config.*` auth path so existing
`proxy/apikeys_test.go` cases stay valid. Production always has `h.keys`.

## Request ID

If the client sends a printable `X-Request-Id` of length 1–128, honor it;
otherwise generate a UUID. Attach to context. Never log the API secret.

## Live vs durable events

| Channel | Store | Consumer |
|---|---|---|
| `metrics.Event` | memory ring + `Subscribe` | admin forwarding activity, portal LIVE |
| `request_events` | SQLite | portal/admin history, restart-safe |
| `usage_hourly` | SQLite | charts (server-side aggregate) |
| `api_key_usage` | SQLite | lifetime / period totals |

Portal SSE: `metrics.Subscribe` → drop if `ApiKeyID` ≠ session key → map to
`PublicRequestEvent` (no provider, account, route, raw error, prompt).

## Quota authorize / reserve / commit

```
Authenticate(secret)
  digest → indexed lookup
  reject: missing / disabled / expired
  roll reset window if due (UTC)
  reject: token/credit already exhausted (soft snapshot)
  BEGIN IMMEDIATE
    if request_limit > 0:
      UPDATE usage SET reserved = reserved+1
        WHERE total+reserved < limit
      else request_quota_exceeded
  return principal + reservation

Commit(reservation, outcome, tokens, credits)
  reserved--
  increment classified counters
  on success: add tokens/credits
  insert request_event + upsert usage_hourly
```

On process start: `UPDATE api_key_usage SET requests_reserved = 0`
(in-flight reservations died with the process).

`count_tokens` and `/v1/stats` authenticate **without** reserving a request slot.

## Status (computed)

`disabled` if `!enabled`; else `expired` if `expires_at` in the past; else
`exhausted` if any hard quota is spent; else `active`.
