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

## Strip legacy plaintext (optional, after confidence)

Default upgrade keeps `apiKeys[].key` so an old binary can boot. After the
rollback window, call `config.FinalizeLegacyPlaintext` (or an operator
tool that wraps it). That verify-then-scrub path is irreversible for old
binaries. Do not flip the flag by hand without verify — a mistyped edit
can leave SQLite keys that no longer match leftover plaintext.
