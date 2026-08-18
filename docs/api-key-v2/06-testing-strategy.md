# 06 — Testing strategy

Phase gates: `go vet ./...`, `go test ./...`, `go test -race ./...`.

## Unit (`apikey`)

- HMAC: same secret+pepper → same digest; different pepper → miss
- Mask: `sk-ab12••••••••89cd` shape; short keys still masked (fix ≤10 leak)
- Period roll: daily/weekly/monthly UTC boundaries
- Status: active / disabled / expired / exhausted
- Public event sanitizer strips provider/account/route/raw error

## Repository

- Create / get / list / update / rotate / delete
- Digest lookup O(1) (indexed); bench 100 / 1k / 10k
- Concurrent reserve near request limit (`-race`)
- Transaction rollback on digest collision
- ImportLegacy twice + after crash (re-open)

## API (`proxy`)

- Existing `proxy/apikeys_test.go` envelopes still pass (config fallback **and**
  service-backed Handler)
- Admin create returns `key` once; GET/list never include plaintext
- Rotate keeps id; old secret 401s
- Pagination / filters
- Portal: A cannot read B summary/events/SSE
- Portal token cannot call `/v1/*`
- Revoked / expired token rejected
- Session cookie is HttpOnly and not on `/v1`

## Quota boundaries

0/100, 99/100, 100/100, 101/100 for tokens, credits, requests. Concurrent
requests at 99/100 with limit 100: at most one extra (soft token) or zero extra
(request reservation).

## Streaming

Reuse existing stream integrity tests. New: cancel after reserve increments
`requests_cancelled` and does not add tokens/credits.

## Realtime

Subscribe, receive matching key, ignore other key, unsubscribe does not leak
goroutines (cancel func).

## Migration

Seed `config.json` keys → Open+Import → authenticate with original secret →
stable IDs → limits/usage retained → second Import no duplicates.
