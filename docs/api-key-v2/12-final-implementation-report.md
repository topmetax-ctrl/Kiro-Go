# 12 — Final implementation report

## 1. Architecture

**Before:** plaintext keys + counters in `config.json`; `RecordApiKeyUsage`
rewrote the whole file; O(n) lookup; Settings card UI; no portal.

**After:** `apikey.Service` + SQLite; HMAC lookup; request reservation;
durable events + hourly aggregates; admin API Keys table; public `/usage`
portal scoped by session. Portal SSE uses a key-scoped `portalHub`
(subscribe-first replay/live handoff). `metrics.Subscribe` remains admin-only.

## 2. Files

- `apikey/` — domain, SQLite, hub, query planner, observability, tests
- `config/apikey_v2.go` — pepper + flags + plaintext finalization
- `proxy/auth.go`, `admin_apikeys.go`, `portal.go`, lease/commit hooks
- `web/usage.html|js|css` + `web/locales/{en,zh,vi}.json`
- `docs/api-key-v2/`, `docs/adr/0002-api-key-v2.md`

## 3. Database

See `02-data-model.md`. Migration: `ImportLegacy` idempotent on startup.
`MaxOpenConns(1)` kept after P5 measurement.

## 4. Security

See `04-security-model.md` and `10-security-review.md`.

## 5. Compatibility

Existing `/v1` headers, master switch, legacy single key, Claude/OpenAI
error messages, admin CRUD JSON names, `sk-` format, `/v1/models` open.

## 6. Testing

| Category | Notes |
|---|---|
| Unit (hash, period, status) | `apikey/*_test.go` |
| Repository / migrate / restart | `store_test.go` |
| Portal isolation + token exchange | `proxy/portal_isolation_test.go` |
| Query / SSE / frontend contract lint | `portal_query_test.go`, `portal_sse_test.go`, `portal_frontend_test.go` |
| Observability invariants | `observability_test.go` |
| Lookup / write / portal benches | `lookup_bench_test.go`, `workload_bench_test.go` |

## 7. Remaining risks

Legacy plaintext retained until operator finalization; admin auth unchanged;
soft token overrun; `enforcement_mode=strict` is stored but does not
hard-limit tokens/credits; replay cap 500 with `sync_required`; SSE revoke
≤ 5s; single SQLite connection.

## 8. V2 later

RPM/TPM, model ACL, PostgreSQL, grace-period rotation, `/v1/models` auth
(product decision), optional multi-connection SQLite after mixed-load evidence.
