# 08 — Operations

## Files

| Path | Role |
|---|---|
| `{configDir}/config.json` | Admin password, `requireApiKey`, pepper, legacy plaintext snapshot |
| `{configDir}/apikeys.db` | Keys, quotas, usage, events, portal tokens/sessions |
| `{configDir}/apikeys.db-wal` / `-shm` | SQLite WAL (back up with the db) |

## Environment

| Variable | Effect |
|---|---|
| `KIRO_APIKEY_PEPPER` | HMAC pepper override (not written to config) |
| `CONFIG_PATH` | Config file path (db lives in the same directory) |

## Useful log lines

- `[ApiKey] store ready; imported N, already present M, skipped K`
- `[ApiKey] failed to commit usage for key <id>`
- `[ApiKey] retention removed N events…`

Never includes the secret.

## Database / WAL growth

`apikeys.db` stays small while recent writes sit in `apikeys.db-wal`.
WAL is checkpointed by SQLite when the writer connection closes (process
shutdown) or when the WAL grows large. Expected V1 size is dominated by
`request_events` (~30 days) plus `usage_hourly` (~365 days).

Operators should back up the `.db` **and** `-wal`/`-shm` together. Do not
run `VACUUM` on a live process. If the WAL is huge after a crash, restart
the process and let SQLite checkpoint; only then consider an offline
`VACUUM` if disk is the constraint.

Retention cleanup runs hourly (`DELETE … LIMIT 500` loops) and is logged
when it removes rows.

## Backup / restore

Copy `config.json` **and** `apikeys.db` (+ WAL/SHM) together. The pepper in
config (or the env var) must match the digests in the database.

## Disable the portal

Set `"portalEnabled": false` in `config.json` and restart. `/usage` and
`/portal/api/*` return 404.

## Portal session revoke

Portal SSE re-checks the session every 5 seconds. Revoke/disable/logout
make new HTTP calls fail immediately; an already-open stream closes on
the next re-auth tick (propagation ≤ 5s). That bound is intentional:
per-event session lookups would serialize the hot path on the single
SQLite connection.

## Rollback (while plaintext is retained)

Stop the RC binary and start the previous binary against the same
`config.json`. Leave `apikeys.db` in place (the old binary ignores it).
Only secrets that were still in `apiKeys[].key` at import time work.
Keys created or rotated after the upgrade live only in SQLite — they will
not authenticate on the old binary (rotated keys fall back to the
pre-rotate snapshot). Usage recorded after the upgrade stays in SQLite.

## Reverse proxy / TLS cookies

`portal_session` sets `Secure` when the process itself has TLS
(`IsTLSEnabled` or `r.TLS`). `TrustProxy` is used for client IP only, not
for `X-Forwarded-Proto`. If TLS terminates at a reverse proxy and the
app speaks HTTP, the cookie will not be marked Secure. Prefer terminating
TLS on the app, or accept this residual risk and block cleartext at the
edge.

SSE: send `X-Accel-Buffering: no` (already set) and disable response
buffering on the proxy (`proxy_buffering off` / equivalent) so heartbeats
and `retry: 3000` reach the browser.

## Strip legacy plaintext (optional, after confidence)

Default upgrade keeps `apiKeys[].key` so an old binary can boot. After the
rollback window, call `config.FinalizeLegacyPlaintext` (or an operator
tool that wraps it). That verify-then-scrub path is irreversible for old
binaries. Do not flip the flag by hand without verify — a mistyped edit
can leave SQLite keys that no longer match leftover plaintext. Do not
finalize production as part of an automated test.
