# 11 — Performance results

Lookup is an indexed HMAC digest match (`UNIQUE secret_digest`), not an O(n)
scan of `config.ApiKeys`.

Run:

```bash
go test ./apikey/ -bench=BenchmarkLookup -benchtime=3s
```

Measured on this machine (`go test ./apikey/ -bench=BenchmarkLookup1000 -benchtime=1s`):

```
BenchmarkLookup1000-6    10926    112061 ns/op
```

~0.11 ms per digest lookup at 1,000 keys (indexed UNIQUE). A linear `==`
scan of `config.ApiKeys` would grow with N; this path does not.

`SetMaxOpenConns(1)` serializes writes; reservation races are tested in
`TestConcurrentRequestLimit`. If inference QPS needs more readers later,
raise the pool and keep reservation `UPDATE … WHERE` atomic.

Usage no longer rewrites `config.json` on the hot path.
