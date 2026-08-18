# 12 — Final implementation report

## 1. Architecture

**Before:** plaintext keys + counters in `config.json`; `RecordApiKeyUsage`
rewrote the whole file; O(n) lookup; Settings card UI; no portal.

**After:** `apikey.Service` + SQLite; HMAC lookup; request reservation;
durable events + hourly aggregates; admin API Keys table; public `/usage`
portal scoped by session. `metrics.Subscribe` reused for LIVE.

## 2. Files

- `apikey/` — domain, SQLite, tests
- `config/apikey_v2.go` — pepper + flags
- `proxy/auth.go`, `admin_apikeys.go`, `portal.go`, handler hooks
- `metrics/metrics.go` — `RequestID`, `ApiKeyID`
- `web/` — API Keys tab, `usage.html/js/css`, locales
- `docs/api-key-v2/`, `docs/adr/0002-api-key-v2.md`

## 3. Database

See `02-data-model.md`. Migration: `ImportLegacy` idempotent on startup.

## 4. Security

See `04-security-model.md` and `10-security-review.md`.

## 5. Compatibility

Existing `/v1` headers, master switch, legacy single key, Claude/OpenAI
error messages, admin CRUD JSON names, `sk-` format, `/v1/models` open.

## 6. Testing

| Category | Notes |
|---|---|
| Unit (hash, period, status) | `apikey/*_test.go` |
| Repository / migrate / race | `store_test.go` |
| Portal isolation | `proxy/portal_isolation_test.go` |
| Existing auth tests | `proxy/apikeys_test.go` still on config fallback |
| Lookup bench | `lookup_bench_test.go` |

## 7. Remaining risks

Legacy plaintext retained for rollback; admin auth unchanged; soft token
overrun; cancel/fail charge known usage; `enforcement_mode=strict` is stored
but does not hard-limit tokens/credits.

## 8. V2 later

RPM/TPM, model ACL, PostgreSQL, grace-period rotation, strip plaintext
automatically, `/v1/models` auth (product decision).
