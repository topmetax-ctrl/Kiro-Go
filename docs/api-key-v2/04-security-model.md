# 04 — Security model

## Threats in scope (V1)

| Threat | Mitigation |
|---|---|
| Stolen `apikeys.db` | HMAC-SHA256 with server pepper; no plaintext secrets |
| Stolen `config.json` | Pepper + legacy plaintext still present until operators strip; file 0600 |
| Key in URL / Referer / history | Forbidden for API keys. Portal token URL is one-shot exchanged |
| Browser storage of raw key | Portal uses HttpOnly session cookie only |
| Horizontal access (key A reads B) | Session → key_id server-side; tests required |
| Portal token used as API key | Different prefix `pt-`; not in `api_keys.secret_digest` |
| Log leakage | Never log secret, Authorization, or pepper. Log keyID + requestID |
| Admin list scrape | Masked prefix/last4 only; plaintext only on create/rotate |
| SSE leak of internals | Public DTO filter; admin stream stays admin-password gated |

## Pepper

- 32 CSPRNG bytes, hex-encoded in `config.apiKeyPepper`.
- `KIRO_APIKEY_PEPPER` overrides in memory and is **not** written back.
- Losing the pepper makes every digest unverifiable — treat as a secret.
  Restore from backup or re-import plaintext leftovers and re-hash.

## Why HMAC instead of raw SHA-256

A stolen database of unsalted SHA-256(API keys) is brute-forceable (keys have
a known `sk-` + hex shape). HMAC with a pepper not stored in the same file as
the digest (when operators use the env override) requires both artifacts.

## Portal session cookie

| Attribute | Value | Why |
|---|---|---|
| Name | `portal_session` | distinct from `admin_password` |
| HttpOnly | yes | JS cannot read it |
| SameSite | Lax | allow top-level GET from share link; block CSRF POST from other sites |
| Secure | if TLS (`IsTLSEnabled` or `r.TLS`) | production HTTPS on the process; localhost HTTP stays usable. TLS-terminating reverse proxies do not flip this flag (`TrustProxy` is IP-only; see `08-operations.md`) |
| Path | `/portal` | not sent to `/v1/*` |
| Max-Age | 12h | short-lived |

`GET /usage/p/{token}` sets `Cache-Control: no-store` and
`Referrer-Policy: no-referrer`, then 302s to `/usage`. HTTP debug logs
redact the token path. The portal page loads only first-party `/usage/*`
and `/locales/*` (no third-party scripts). Pepper precedence:
`KIRO_APIKEY_PEPPER` (memory only) then persisted `apiKeyPepper`. Pepper
is generated once and saved; it is not regenerated on restart.

Session value is an opaque random ID; only its digest is stored.

## Isolation rules (P0)

A portal session for key A must receive 404/401 (not 403 with existence leak
where avoidable) when asking for key B's data. There is no key selector. SSE
subscribers are filtered by the session's `key_id` on the server.

## Privacy

`request_events` stores operational metadata only. If a future change wants
prompt logging, it needs an explicit ADR. Existing Responses store and optional
memory capture are out of this subsystem's write path.

## Out of V1 scope (known debt)

Admin password still plaintext in config + localStorage; CSRF tokens; login
rate limit; `/v1/models` unauthenticated. Documented, not silently "fixed" in
this change.
