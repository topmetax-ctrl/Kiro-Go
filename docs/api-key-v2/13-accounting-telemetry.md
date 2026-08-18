# 13 — Accounting and telemetry contract

Request **outcome** is independent of **resource consumption**.

## Counters

| Field | Meaning |
|---|---|
| `requests_total` | Quota consumed = success + failed + cancelled |
| `requests_rejected` | Admissions that never executed (no-account 503, 4xx) |
| `requests_attempted` | Derived: success + failed + cancelled + rejected |
| `requests_quota_consumed` | Alias of `requests_total` |
| `requests_reserved` | In-flight; always ≥ 0; 0 when idle |

## Usage policy

| Case | Outcome | Request quota | Tokens / credits | Source |
|---|---|---|---|---|
| success non-stream / stream | success | consumed | accounted | `upstream` else `estimator` |
| validation / malformed JSON | rejected | no | 0 | `none` |
| no available accounts | rejected | no | 0 | `none` |
| provider error before first byte | failed | consumed | estimated input if the provider call started | `estimator` |
| provider error / truncate after partial output | failed | consumed | observed output + estimated/upstream input | `stream_observed` / `upstream` |
| timeout / cancel before provider | cancelled | consumed | 0 | `none` |
| timeout / cancel after provider, no output | cancelled | consumed | estimated input | `estimator` |
| mid-stream client disconnect | cancelled | consumed | estimated input + observed output | `stream_observed` |
| late usage chunk on clean EOF | success | consumed | upstream | `upstream` |
| missing final usage on clean EOF | success | consumed | estimator | `estimator` |

Credits are never estimated. They are recorded only from a Kiro `meteringEvent`.

`rejected` ignores any token fields on Commit.

## Strict mode (V1)

- Request limit: always exact reservation when set.
- Token / credit limits: always **soft** (checked at next Authenticate).
- `enforcement_mode=strict` is stored for V2 and must not be advertised as
  hard token/credit enforcement.

## Observability

`apikey.UnsettledReservations()` counts handler exits that reserved but never
`Note()`d an outcome. Production expectation: 0. Panic recovery Notes
`internal_error` so that path does not increment the counter.
