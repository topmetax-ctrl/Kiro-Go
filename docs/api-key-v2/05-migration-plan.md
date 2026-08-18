# 05 — Migration plan

## Goals

Existing client secrets, IDs, names, enabled flags, createdAt, token/credit
limits, and usage counters survive. Clients do not regenerate keys. The job is
idempotent, crash-safe, and does not delete `config.json` on failure.

## Steps (startup)

1. `config.Init` — existing Load already folds legacy `apiKey` into `apiKeys`.
2. `GetOrCreateAPIKeyPepper`.
3. `apikey.Open(apikeys.db, pepper)` — apply SQL migrations; zero
   `requests_reserved`.
4. `ImportLegacy(config.ListApiKeys())`:
   - For each entry with non-empty `key`: `INSERT OR IGNORE` by `id`.
   - Digest = HMAC(key, pepper). Copy name/enabled/migrated/timestamps/limits/usage
     onto the lifetime usage row (`requests_total = requestsCount`,
     `requests_success = requestsCount` because historical counts were success-only).
   - If the row already exists (same id), **do not** overwrite live usage.
   - Record `legacy_import` in `schema_migrations` equivalent table
     `import_state(id PRIMARY KEY, done INTEGER)` key `config_api_keys`.
5. On import error: log, **do not** truncate config, **do not** serve with a
   half-empty store if any source key failed to hash. Fail startup.

Re-running Open + Import 10 times must yield the same row count.

## Dual source during V1

| Concern | Source of truth after upgrade |
|---|---|
| Auth lookup | SQLite digest |
| Usage / events | SQLite |
| Admin CRUD | SQLite |
| Rollback binary | `config.ApiKeys` plaintext snapshot |

`legacyPlaintextRetention` defaults true: config keys are not stripped.

### Explicit plaintext finalization

After a verification/rollback window, an operator may call
`config.FinalizeLegacyPlaintext(verify)`. It:

1. verifies every remaining `apiKeys[].key` still authenticates in SQLite;
2. refuses to scrub if any verify fails;
3. clears plaintext `key` fields;
4. sets `legacyPlaintextRetention=false`;
5. saves atomically (`Save` already writes `.bak`).

Startup never flips the default. SQLite key rows are never deleted. Older
binaries cannot authenticate those keys after finalization.

## Rollback

1. Stop new binary.
2. Start previous binary. It reads `config.json` keys (still valid).
3. Usage after the upgrade is in SQLite and **will not** appear in the old UI.
   Pre-upgrade counters in config are the snapshot at first import.

## Failure modes

| Failure | Behavior |
|---|---|
| DB create/open error | Fatal at `NewHandler` / main — do not silently fall back to JSON usage writes |
| Pepper missing and cannot persist | Fatal |
| Import of one corrupt entry | Fatal with key **id** (never secret) in the log |
| Crash mid-import | Next start `INSERT OR IGNORE` + existing rows left intact |

## Observability

Log: `api-key store opened`, `legacy import: N keys (M already present)`,
`legacy import complete`. Metric-style counters live in process logs for V1
(no Prometheus).
