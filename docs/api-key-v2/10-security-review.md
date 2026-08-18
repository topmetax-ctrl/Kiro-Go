# 10 — Security review

## Mitigations shipped

- HMAC-SHA256 + pepper; plaintext shown once on create/rotate
- Admin list/get return masks only
- Portal session HttpOnly, SameSite=Lax, Path=/portal (not sent to `/v1`)
- Secure cookie when TLS; localhost HTTP remains usable
- API keys never in URLs; portal token URL is exchanged then dropped
- `/usage/p/{token}`: `Cache-Control: no-store`, `Referrer-Policy: no-referrer`
- HTTP debug logs redact `/usage/p/{token}`
- Portal UI is first-party only (no third-party scripts on the share URL)
- No raw key in localStorage/sessionStorage
- Horizontal isolation: session → key_id server-side; foreign cursors and
  `Last-Event-ID` values do not leak another key
- Portal token (`pt-`) cannot authenticate `/v1/*`
- Public events omit provider/account/route/raw errors/prompts
- Request history does not store Authorization, prompts, or completions
- Explicit `FinalizeLegacyPlaintext` after a rollback window (never automatic)

## Residual risk (accepted V1)

- `config.json` still holds legacy plaintext keys until an operator finalizes
- Pepper lives in config unless `KIRO_APIKEY_PEPPER` is set
- Admin password still plaintext + JS cookie (pre-existing)
- Admin mutation CSRF is the pre-existing cookie/password model (out of scope)
- Portal logout is SameSite=Lax POST/DELETE; no new CSRF architecture
- `/v1/models` remains unauthenticated (compatibility)
- Soft token/credit quota can overrun by one in-flight request
- SSE revoke propagation ≤ 5s on an already-open stream
- Replay cap 500; overflow emits `sync_required` rather than silent LIVE

## Tests covering isolation

`proxy/portal_isolation_test.go` (including Alice/Bob cursor and Last-Event-ID),
`apikey/store_test.go` (`TestPortalIsolationAndSession`),
`proxy/portal_query_test.go`, `proxy/portal_sse_test.go`.
