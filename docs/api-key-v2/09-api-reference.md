# 09 — API reference

See [03-api-contract.md](03-api-contract.md) for envelopes and field lists.

## Admin (password: `X-Admin-Password` or `admin_password` cookie)

- `GET /admin/api/api-keys?q&status&quota&usage&sort&offset&limit`
- `POST /admin/api/api-keys`
- `POST /admin/api/api-keys/batch` `{count,name?,enabled?,tokenLimit?,creditLimit?,requestLimit?,expiresAt?,resetPolicy?,enforcementMode?}`
- `GET|PUT|DELETE /admin/api/api-keys/{id}`
- `POST /admin/api/api-keys/{id}/reset-usage`
- `POST /admin/api/api-keys/{id}/rotate`
- `POST|DELETE /admin/api/api-keys/{id}/portal-token`

## Portal (cookie `portal_session`, Path=/portal)

- `POST /portal/api/session` `{key,remember?}`
- `POST /portal/api/session/token` `{token}`
- `DELETE /portal/api/session`
- `GET /portal/api/me`
- `GET /portal/api/summary`
- `GET /portal/api/usage?range&from&to&model&endpoint&status&streaming&error_code&metric`
- `GET /portal/api/events?range&from&to&model&endpoint&status&streaming&error_code&cursor&limit`
- `GET /portal/api/events/stream` SSE (`Last-Event-ID` or `after`; named events `request`, `sync_required`, `session`)

## Pages

- `GET /usage` — public portal UI
- `GET /usage/p/{portal-token}` — one-shot exchange + redirect
- `GET /locales/{lang}.json` — shared i18n
