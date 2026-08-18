# 10 — Security review

## Mitigations shipped

- HMAC-SHA256 + pepper; plaintext shown once on create/rotate
- Admin list/get return masks only
- Portal session HttpOnly, SameSite=Lax, Path=/portal (not sent to `/v1`)
- API keys never in URLs; portal token URL is exchanged then dropped
- No raw key in localStorage/sessionStorage
- Horizontal isolation: session → key_id server-side
- Portal token (`pt-`) cannot authenticate `/v1/*`
- Public events omit provider/account/route/raw errors/prompts
- Request history does not store Authorization, prompts, or completions

## Residual risk (accepted V1)

- `config.json` still holds legacy plaintext keys and the pepper (unless env)
- Admin password still plaintext + JS cookie (pre-existing)
- `/v1/models` remains unauthenticated (compatibility)
- Soft token/credit quota can overrun by one in-flight request
- Cancelled streams do not consume token/credit quota (compatibility)

## Tests covering isolation

`proxy/portal_isolation_test.go`, `apikey/store_test.go` (`TestPortalIsolationAndSession`).
