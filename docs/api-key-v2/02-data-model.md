# 02 — Data model

SQLite file: `{configDir}/apikeys.db`.

## Pragmas

Chosen from SQLite documentation for a single-writer process:

- `journal_mode=WAL` — readers do not block the writer
- `busy_timeout=5000` — wait 5s on lock instead of immediate SQLITE_BUSY
- `foreign_keys=ON`
- `synchronous=NORMAL` — safe with WAL on local disk
- `SetMaxOpenConns(1)` — serialize writes; revisit after the 10k-key bench

## Tables

### schema_migrations

`version INTEGER PRIMARY KEY` — applied in order. V1 = `1`.

### api_keys

| Column | Type | Notes |
|---|---|---|
| id | TEXT PK | Stable UUID; unchanged on rotate |
| name | TEXT | |
| secret_digest | TEXT UNIQUE NOT NULL | hex HMAC-SHA256 |
| key_prefix | TEXT | first 6 chars for mask |
| key_last4 | TEXT | last 4 chars |
| enabled | INTEGER | 0/1 |
| migrated | INTEGER | from legacy config |
| created_at | INTEGER | unix seconds |
| updated_at | INTEGER | |
| last_used_at | INTEGER | nullable |
| expires_at | INTEGER | nullable; 0/NULL = none |
| metadata | TEXT | JSON object, default `{}` |

### api_key_quotas

| Column | Type | Notes |
|---|---|---|
| key_id | TEXT PK FK | |
| token_limit | INTEGER | 0 = unlimited |
| credit_limit | REAL | 0 = unlimited |
| request_limit | INTEGER | 0 = unlimited |
| reset_policy | TEXT | `lifetime`/`daily`/`weekly`/`monthly` |
| period_start | INTEGER | unix seconds, UTC |
| period_end | INTEGER | nullable for lifetime |
| enforcement_mode | TEXT | `soft` (default) / `strict` (reserved) |

V1 implements soft token/credit + strict request reservation regardless of
`enforcement_mode` except that the column is stored for V2.

### api_key_usage

| Column | Type | Notes |
|---|---|---|
| key_id | TEXT | |
| period | TEXT | `lifetime` or `YYYY-MM-DD` / `YYYY-Www` / `YYYY-MM` |
| requests_total | INTEGER | quota consumed = success+failed+cancelled |
| requests_success/failed/cancelled/rejected | INTEGER | |
| requests_reserved | INTEGER | in-flight; reset to 0 on open |
| input_tokens / output_tokens / total_tokens | INTEGER | |
| credits | REAL | |
| last_used_at | INTEGER | |
| PRIMARY KEY (key_id, period) | | |

Lifetime row is always maintained. Period row is maintained when reset ≠ lifetime.

### request_events

Metadata only. No prompt, messages, tools, completion, Authorization, or secret.

Columns: `id INTEGER PK` (exposed as `eventId`), `request_id`, `key_id`, `ts`, `endpoint`,
`client_model`, `effective_model`, `status_code`, `status`, `input_tokens`,
`output_tokens`, `total_tokens`, `credits`, `latency_ms`, `ttfb_ms`, `stream`,
`cancelled`, `error_code`, `sanitized_error`, `usage_source`, `usage_estimated`.

`requests_attempted` is derived: success+failed+cancelled+rejected.
`requests_quota_consumed` = `requests_total`.

Indexes:

- `request_events(key_id, ts DESC)`
- `request_events(key_id, status, ts DESC)`
- `request_events(key_id, effective_model, ts DESC)`

### usage_hourly

`(key_id, hour_utc)` unique. Counters matching the event metrics for charts.

### portal_tokens

One current token per key (regenerate replaces). `token_digest` UNIQUE,
`prefix`, `created_at`, `expires_at` nullable, `revoked_at` nullable.

### portal_sessions

`id_digest` PK, `key_id`, `expires_at`, `created_at`. TTL default 12h.

## Retention

| Data | Default | Config |
|---|---|---|
| `request_events` | 30 days | `usageRetentionDays` |
| `usage_hourly` | 365 days | fixed V1 |
| lifetime usage | permanent | |
| keys / quotas | until delete | |

Cleanup: hourly, `DELETE … WHERE ts < ? LIMIT 500` loop.

## In-memory / config leftovers

`config.ApiKeys[].Key` remains for rollback. Usage fields in that slice become
a snapshot at import time and are no longer updated.
