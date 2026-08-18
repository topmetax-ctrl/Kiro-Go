# 11 — Performance results

Measured on this machine:

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
APIKEY_BENCH_EVENTS=1000000 go test ./apikey -bench=BenchmarkPortalQueries -benchtime=3s -timeout 30m
```

## Results

| Workload | Result | Bottleneck | Decision |
|---|---|---|---|
| Key lookup 100 | 114 µs/op | HMAC + unique digest index | Keep digest lookup |
| Key lookup 1,000 | 121 µs/op | same; **not O(n)** vs 100 | No scan of `config.ApiKeys` |
| Key lookup 10,000 | ~0.11 ms/op (earlier 1s run) | same | Indexed `UNIQUE secret_digest` |
| Usage write serial | 487 µs/op, 234 allocs | SQLite commit + event insert | Acceptable for this host |
| Usage write parallel | 532 µs/op | single writer serializes | Expected with one connection |
| Portal queries (5k events: latest 50 + cursor + 24h series) | 12.5 ms/op | aggregation + page | Cursor uses `idx_events_key_ts*` |
| SSE publish, 10 subscribers | 512 µs/op | same as Commit | Fan-out not visible vs write |
| SSE publish, 100 subscribers | 509 µs/op | same | Bounded buffer; slow clients drop |
| 1M-event seed | not run in CI | disk + insert time | Manual `APIKEY_BENCH_EVENTS` |
| SQLite `MaxOpenConns` | 1 | writes queue; no busy errors in these benches | **Keep 1** |

Lookup 100 → 1,000 grew ~6%, not 10×. That is the evidence digest lookup is not a linear scan.

Parallel Commit did not outrun serial by much and did not raise `sqlite_busy` in this harness. Raising the pool would need a query that blocks writers — none showed up at this scale.

## Query plans

`TestExplainUsesKeyTsIndex` checks:

- cursor page `(ts, id)` → `idx_events_key_ts` / `idx_events_key_ts_id`
- status filter → `idx_events_key_status_ts` (or covering key/ts)
- model filter → `idx_events_key_model_ts`
- secret lookup → `api_keys.secret_digest` unique index

No new single-column indexes were added in P5. Existing composite indexes are enough for the hot paths.

## SSE

Publish latency with 10 vs 100 draining subscribers is within noise of a single Commit. A slow subscriber is disconnected (`portalSubBuffer=64`) instead of blocking the writer. Replay overflow is capped at 500 and signaled with `sync_required`.

## Decision: keep `SetMaxOpenConns(1)`

Evidence: Commit is the cost center; portal reads in this bench are milliseconds on 5k rows; SSE fan-out is cheaper than the write; reservation tests already serialize on the same connection. Changing the pool without a measured writer-blocked-by-reader incident would trade simplicity for an unproven gain.

Revisit only if production shows portal history queries delaying `Commit` (busy/write counters climbing under mixed load).

## Limitations

- 1M-row portal bench is opt-in; CI uses 5k.
- 500 synthetic SSE clients were not run here; 100 already shows fan-out << write.
- Numbers are one host, one SQLite file, no competing inference traffic.
- Allocations on Commit (~9 KB, ~230 allocs) are dominated by SQL and DTO mapping, not the hub.
