# 07 — Rollout plan

## Flags (minimal)

| Key | Default | Meaning |
|---|---|---|
| `apiKeyPepper` | generated | HMAC secret (or env `KIRO_APIKEY_PEPPER`) |
| `legacyPlaintextRetention` | true | keep `apiKeys[].key` in config.json |
| `usageRetentionDays` | 30 | raw event TTL |
| `portalEnabled` | true | serve `/usage` and `/portal/api/*` |

No storage switch flag: SQLite is always opened. Falling back to JSON usage
writes would reintroduce the bug.

## Rollout

1. Deploy binary + `web/` (usage.html/js). Existing `config.json` unchanged.
2. First start creates `apikeys.db` and imports keys. Watch logs for import count.
3. Hit `/v1/*` with an existing key. Confirm 200 and that `config.json` mtime
   does **not** jump on every request.
4. Open admin **API Keys** tab. Create a test key, rotate, reset, portal link.
5. Open portal without admin login; confirm isolation with a second key.

## Rollback

Revert binary. Leave `apikeys.db` in place (harmless to old binary). Old binary
uses `config.json` keys. Do not delete the DB unless wiping the feature.

## Backup

Back up `config.json` **and** `apikeys.db` (and WAL/SHM). Pepper is in config
or the environment — both are required to verify secrets.

## Known product deltas operators should know

- Request quota is new (default unlimited).
- Cancelled requests now consume a request slot if a request limit is set.
- Per-key usage in `config.json` freezes at import time.
- Portal is public (no admin password) but key-scoped.
