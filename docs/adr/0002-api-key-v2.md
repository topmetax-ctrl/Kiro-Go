# ADR 0002: API key v2 store, quota, and usage portal

Status: Accepted
Date: 2026-08-18

## Context

Client API keys live in `config.json` as plaintext `ApiKeyEntry` values. Every
successful inference rewrites the entire file (`RecordApiKeyUsage` →
`saveLocked`), including the admin password and account refresh tokens. Lookup
is an O(n) `==` scan. Quota is a TOCTOU check at auth time. Metrics events have
no `ApiKeyID` or `RequestID`. There is no public usage portal.

The requirement is a durable key/usage/event subsystem that preserves existing
client and admin contracts.

## Decision

1. **New `apikey` package** owns domain logic and SQLite persistence
   (`data/apikeys.db`). It does not import `proxy` or `config`.
2. **HMAC-SHA256(secret, pepper)** for stored digests. Pepper is a CSPRNG 32-byte
   value persisted as `config.apiKeyPepper` (file mode 0600), overridable by
   `KIRO_APIKEY_PEPPER`. Raw SHA-256 without a server secret is rejected: a
   stolen DB would be rainbow-tableable.
3. **Keep `sk-` + 32-byte hex** generation. Existing secrets stay valid after
   import; clients are not asked to rotate.
4. **Master switch `requireApiKey` is unchanged.** Auth-off still passes through
   even if keys exist. Legacy `config.ApiKey` still authenticates when the
   multi-key list is empty.
5. **Quota semantics:**
   - Token quota uses accounted tokens (`usageSplit`: upstream else estimator).
   - Credit quota uses Kiro `meteringEvent` only (forward path stays 0).
   - Request quota is reserved atomically at inference authorize time.
   - Token/credit enforcement is **soft** (small overrun on the in-flight request).
   - Combined limits are AND; 0 = unlimited.
   - Reset policies: lifetime (default) | daily | weekly | monthly in **UTC**.
6. **Cancel/fail charge known usage.** Request outcome is independent of
   resource consumption. `rejected` never charges tokens/credits (admission
   never ran). `success` / `failed` / `cancelled` persist and increment any
   observed or estimated tokens/credits so a mid-stream disconnect cannot
   bypass quota. Usage is tagged `usage_source` + `usage_estimated`.
7. **Reuse `metrics.Subscribe`** for live portal/admin feeds. Do not add a third
   SSE bus. Persist `request_events` separately so history survives restart.
8. **Portal identity is derived from a session**, never from `?keyId=`. API keys
   never appear in URLs. Shareable portal tokens (`pt-…`) are exchanged for a
   session cookie and redirected off the token URL.
9. **Legacy plaintext remains in `config.json`** (`legacyPlaintextRetention=true`)
   so an old binary can still boot. Live usage is not written back to config.

## Alternatives rejected

- **Keep usage in `config.json`:** write amplification and secret rewrite on the
  hot path are the bug being fixed.
- **Add PostgreSQL/Redis:** single-process deploy; `modernc.org/sqlite` is
  already a module dependency.
- **Raw SHA-256 of the key:** weaker stolen-DB story than HMAC with a pepper.
- **Charge cancelled streams:** originally rejected for compatibility; V1.5
  charges observed/estimated usage because otherwise stream+disconnect bypasses
  token quota. Credits still only come from metering events (often 0 on cancel).
- **A new SSE subsystem:** `metrics.Subscribe` already has heartbeat, backfill,
  and slow-consumer drop semantics.

## Consequences

- `proxy.Handler` holds `*apikey.Service`. Tests that construct `&Handler{}`
  keep the config fallback until they attach a service.
- Admin JSON field names stay stable; new fields are additive.
- `/v1` error envelopes and status codes stay stable (401/429 messages).
- Operators rolling back the binary see pre-upgrade usage counters in
  `config.json`, not post-upgrade SQLite totals.
