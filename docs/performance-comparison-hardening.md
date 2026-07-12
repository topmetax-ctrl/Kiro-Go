# Performance Comparison — Hardening Branch

Branch: `feat/hardening-profile-picker-lru` vs base `9bdc617`.
Machine: darwin/amd64, Intel i5-8500B @ 3.00GHz, Go 1.26.5.
Method: `go test -bench -benchmem -benchtime=200ms -count=5`. Raw data in
`docs/benchmarks-before-hardening.txt` and `docs/benchmarks-after-hardening.txt`.
Numbers below are representative runs (medians across the count=5 set); absolute
ns/op varies with machine load, so read the *ratios* and *scaling shape*, not the
exact figures.

## Prompt-cache tracker (the headline change)

The tracker changed from a per-account nested map with an O(total-entries)
`pruneExpiredLocked` scan on every call, to a bounded global LRU (map +
container/list) with lazy per-key expiry. This removes the full-map scan from the
hot path.

| Benchmark | Before | After | Change |
|---|---|---|---|
| `Compute` — 1 account, 4 entries | ~330 ns/op | ~216 ns/op | ~1.5× faster |
| `Compute` — 64 accounts, 16 entries | ~17,200 ns/op | ~290 ns/op | **~59× faster** |
| `Update` — 64 accounts, 16 entries | ~18,200 ns/op | ~404 ns/op | **~45× faster** |
| `ComputeMiss` — 64 accounts | ~17,700 ns/op | ~272 ns/op | **~65× faster** |
| allocs/op (hot path) | 0 | 0 | unchanged |

The critical result is the **scaling shape**: before, `Compute` cost grew roughly
linearly with the total number of cached entries across all accounts (330 ns at 1
account → 17 µs at 64), because every call scanned the entire structure to prune
expired entries. After, `Compute` is effectively flat (216 ns → 290 ns) because
lookups are O(1) and expiry is handled lazily on access. Memory is now bounded by a
single global capacity (default 50k entries) regardless of account count; before it
grew unbounded.

No allocation regression: the hot path remains 0 allocs/op.

## Account selection (pool)

Getters now return an immutable `config.Account` snapshot (a value copy) instead of
a pointer into the live backing slice, to eliminate a torn-read race under
concurrent refresh.

| Benchmark | Before | After | Change |
|---|---|---|---|
| `GetNextExcluding` — 64 accounts | ~175–575 ns/op, 0 allocs | ~291 ns/op, 1 alloc (704 B) | +1 alloc/op |
| `GetByID` | ~230 ns/op | ~228 ns/op | flat |

The snapshot adds exactly one ~704-byte allocation per selection (the
`config.Account` copy). This is negligible relative to the cost of an actual
upstream request, and it buys race-freedom by construction. `ns/op` is in the same
band as before (the before-numbers themselves varied 175–640 ns across runs due to
machine load). This is an accepted, deliberate trade documented in the audit: a
data-race fix worth one small allocation off the request hot path.

## Web-search router

`Route` was benchmarked as a control (unchanged by this work); it stays in the
~2–10 µs band dominated by the fake provider, with no regression.

## Verdict

- The prompt-cache rework is a large, unambiguous win: the O(n) prune is gone and
  the hot path is flat and bounded.
- The account-snapshot change adds one small allocation per selection — a
  conscious correctness-for-allocation trade, not a regression to hide.
- No benchmark showed an unexplained regression. Nothing was removed to mask a
  regression.
