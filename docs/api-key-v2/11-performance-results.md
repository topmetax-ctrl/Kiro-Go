# 11 — Performance results

Measured on this machine (P5 + P7 re-run):

```
goos: darwin
goarch: amd64
cpu: Intel(R) Core(TM) i5-8500B CPU @ 3.00GHz
MaxOpenConns(1) on apikeys.db
```

Commands:

```bash
go test ./apikey/ -bench=BenchmarkLookup -benchtime=1s
go test ./apikey/ -bench='BenchmarkCommit|BenchmarkPortalQueries|BenchmarkSSEPublish' -benchtime=1s
# Optional large store (manual, not CI):
APIKEY_BENCH_EVENTS=1000000 go test ./apikey -bench=BenchmarkPortalQueries -benchtime=3s -timeout 45m
```

## Results

| Workload | Result | Bottleneck | Decision |
|---|---|---|---|
| Key lookup 100 | 260 µs/op (P7) / 114 µs/op (P5) | HMAC + unique digest index | Keep digest lookup |
| Key lookup 1,000 | 162 µs/op (P7) / 121 µs/op (P5) | same; **not O(n)** vs 100 | No scan of `config.ApiKeys` |
| Key lookup 10,000 | 304 µs/op (P7) / ~110 µs/op (P5) | same (~1.2× vs 100, not 100×) | Indexed `UNIQUE secret_digest` |
| Usage write serial | 891 µs/op (P7, machine also seeding) / 487 µs/op (P5) | SQLite commit + event insert | Acceptable |
| Usage write parallel | 707 µs/op (P7) / 532 µs/op (P5) | single writer serializes | Expected with one connection |
| Portal queries (5k events: latest 50 + cursor + 24h series) | 12.5 ms/op (P5) | aggregation + page | Cursor uses `idx_events_key_ts_id` |
| Portal queries (1M events, **all timestamps in the 24h window**) | 7.21 s/op combined | 24h series scans 1M raw rows | Pathological density; see split below |
| SSE publish, 10 subscribers | 1.01 ms/op (P7) / 512 µs/op (P5) | same as Commit | Fan-out not visible vs write |
| SSE publish, 100 subscribers | 736 µs/op (P7) / 509 µs/op (P5) | same | Bounded buffer; slow clients drop |
| SSE fan-out 10 / 100 / 500 draining subs, 20 publishes | 12 / 12 / 16 ms total; 200 / 2000 / 10000 events received; goroutines 2→2 | channel send | 500 is fine on this host |
| SQLite `MaxOpenConns` | 1 | writes queue; no busy errors | **Keep 1** |

Lookup 100 → 10,000 grew ~1.2×, not 100×. Digest lookup is not a linear scan.

P7 1M **split reads** (1M rows spread over 30 days, same schema; bulk insert for read timing + size only):

| Query | Latency |
|---|---|
| latest 50 | 624 µs |
| cursor next | 601 µs |
| status=failed | 1.07 ms |
| model filter | 499 µs |
| error_code filter | 1.55 ms |
| series 24H (raw, ~1/30 of rows) | 307 ms |
| series CUSTOM 6H | 47 ms |
| series 7D / 30D (hourly store empty in this file) | 384 µs / 112 µs |

| Artifact | Size |
|---|---|
| `apikeys.db` after 1M events | 404 MB |
| WAL after writer close | 0 |

The documented Commit harness (`APIKEY_BENCH_EVENTS=1000000`) seeds every row at "now", so the bundled 24h series in that bench is a worst-case scan of all 1M rows (7.21 s). That is not a realistic single-key day. With a 30-day spread, latest/cursor stay sub-millisecond and 24h series is ~0.3 s.

## Query plans

`TestExplainUsesKeyTsIndex` and the 1M file both report:

- cursor `(ts, id)` → `SEARCH request_events USING COVERING INDEX idx_events_key_ts_id (key_id=?)`
- status / model / error filters → same `idx_events_key_ts_id` (SQLite preferred it over `idx_events_key_status_ts` / `idx_events_key_model_ts` even at 1M rows)
- secret lookup → `SEARCH api_keys USING INDEX sqlite_autoindex_api_keys_2 (secret_digest=?)`

No new indexes were added. All hot paths are index searches, not table scans.

## SSE

Publish latency with 10 vs 100 draining subscribers stays in the Commit noise band. A dedicated P7 fan-out of 500 draining subscribers received every event (10,000/10,000) in 16 ms for 20 publishes; unsubscribe returned to the pre-subscribe goroutine count. A slow subscriber is disconnected (`portalSubBuffer=64`) instead of blocking the writer. Replay overflow is capped at 500 and signaled with `sync_required`.

## Decision: keep `SetMaxOpenConns(1)`

Evidence: Commit is the write cost center; cursor pages are sub-millisecond at 1M rows; SSE fan-out to 500 is cheaper than a single write; the 7 s number is a 24h aggregate over a pathological 1M-in-one-window seed, not a connection-pool failure. No `sqlite_busy` in these harnesses. Changing the pool without a measured writer-blocked-by-reader incident would trade simplicity for an unproven gain.

Revisit only if production shows portal history queries delaying `Commit` (busy/write counters climbing under mixed load).

## Limitations

- CI still uses the 5k `BenchmarkPortalQueries` default.
- 1M Commit-path seed takes ~10 minutes on this host; do not put it in `go test ./...`.
- 7D/30D series latency above used an events-only file (empty `usage_hourly`); production charts for those spans read hourly aggregates.
- Numbers are one host, one SQLite file, no competing inference traffic.
- Allocations on Commit (~9 KB, ~230 allocs) are dominated by SQL and DTO mapping, not the hub.
