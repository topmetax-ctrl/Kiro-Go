package metrics

import (
	"sort"
	"time"
)

// maxTrackedIPs caps the per-source-IP table. A busy proxy sees few distinct
// callers, but an untrusted deployment could be probed by many IPs; the cap
// keeps memory bounded while still surfacing the heavy hitters.
const maxTrackedIPs = 5000

// ipAgg is one source IP's lifetime counters plus the same minute/hour time
// series a providerAgg keeps, so the callers tab can scope its leaderboard to
// a selected window the way the provider Stats tab does. The bucket maps share
// the provider O(events) bound: a bucket exists only for an (IP, minute/hour)
// that actually saw traffic, so a scan flood of one-hit IPs costs two buckets
// per IP, not the full rolling window.
type ipAgg struct {
	counter
	minutes map[int64]*Bucket
	hours   map[int64]*Bucket
}

func newIPAgg() *ipAgg {
	return &ipAgg{
		minutes: make(map[int64]*Bucket),
		hours:   make(map[int64]*Bucket),
	}
}

// IPStat is the per-caller rollup surfaced by TopIPs: how much traffic one
// source IP has generated over the store's lifetime.
type IPStat struct {
	IP           string  `json:"ip"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	Canceled     int64   `json:"canceled"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	TotalTokens  int64   `json:"totalTokens"`
	CostUSD      float64 `json:"costUsd"`
	AvgLatencyMs int64   `json:"avgLatencyMs"`
	LastUsed     int64   `json:"lastUsed"`
}

// TopIPs returns the callers with the most requests, highest first. A limit of
// zero or less returns every tracked IP.
func TopIPs(limit int) []IPStat {
	s.mu.Lock()
	out := make([]IPStat, 0, len(s.byIP))
	for ip, c := range s.byIP {
		out = append(out, IPStat{
			IP:           ip,
			Requests:     c.requests,
			Success:      c.success,
			Failed:       c.failed,
			Canceled:     c.canceled,
			InputTokens:  c.inputTokens,
			OutputTokens: c.outputTokens,
			TotalTokens:  c.inputTokens + c.outputTokens,
			CostUSD:      c.costUSD,
			AvgLatencyMs: c.avg(),
			LastUsed:     c.lastUsed,
		})
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].LastUsed > out[j].LastUsed
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// TopIPsWindow returns per-IP aggregates restricted to the last `hours`,
// busiest first. It backs the callers tab's range filter, where the all-time
// counters of TopIPs would make the selector look inert.
//
// The windowing rule mirrors ProviderStatsWindow: ranges up to bucketWindowMins
// read the per-minute series (so the 1h view is not padded to a two-hour
// boundary), longer ranges read the hourly rollups. Per-minute buckets are
// memory-only, so the 1h view reads empty for a while after a restart — the
// same caveat the provider Stats tab carries.
//
// LastUsed is derived from the newest bucket inside the window, so it is exact
// only to the bucket unit (a minute or an hour). A leaderboard ranked by
// volume reads that coarse stamp fine; TopIPs keeps the exact one for the
// all-time view.
func TopIPsWindow(hours, limit int) []IPStat {
	if hours <= 0 {
		return TopIPs(limit)
	}
	useMinutes := hours*60 <= bucketWindowMins
	now := time.Now().UnixMilli()

	var cutoff int64
	if useMinutes {
		cutoff = now/60000 - int64(hours*60) + 1
	} else {
		if hours > hourWindowHours {
			hours = hourWindowHours
		}
		cutoff = now/3600000 - int64(hours) + 1
	}

	s.mu.Lock()
	out := make([]IPStat, 0, len(s.byIP))
	for ip, c := range s.byIP {
		src := c.hours
		if useMinutes {
			src = c.minutes
		}
		var agg Bucket
		var lastBucket int64
		for k, b := range src {
			if k < cutoff {
				continue
			}
			agg.Requests += b.Requests
			agg.Success += b.Success
			agg.Failed += b.Failed
			agg.InputTokens += b.InputTokens
			agg.OutputTokens += b.OutputTokens
			agg.CostUSD += b.CostUSD
			agg.SumLatencyMs += b.SumLatencyMs
			if k > lastBucket {
				lastBucket = k
			}
		}
		if agg.Requests == 0 {
			continue
		}
		st := IPStat{
			IP:           ip,
			Requests:     agg.Requests,
			Success:      agg.Success,
			Failed:       agg.Failed,
			InputTokens:  agg.InputTokens,
			OutputTokens: agg.OutputTokens,
			TotalTokens:  agg.InputTokens + agg.OutputTokens,
			CostUSD:      agg.CostUSD,
		}
		if useMinutes {
			st.LastUsed = lastBucket * 60000
		} else {
			st.LastUsed = lastBucket * 3600000
		}
		st.AvgLatencyMs = agg.SumLatencyMs / agg.Requests
		out = append(out, st)
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].LastUsed > out[j].LastUsed
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// TopIPsRange returns per-IP aggregates over the arbitrary [fromMs, toMs]
// window (Unix ms; toMs <= 0 means now), busiest first. It backs the callers
// tab's custom range with the same granularity and edge-rounding rule as
// ProviderStatsRange: minute buckets while the window is fully covered by
// memory, hourly rollups beyond that with edge buckets counted in full.
// LastUsed derives from the newest bucket inside the window, exact only to the
// bucket unit; the all-time view keeps the exact stamp.
func TopIPsRange(fromMs, toMs int64, limit int) []IPStat {
	useMinutes, lo, hi, _ := rangeBucketBounds(fromMs, toMs)

	s.mu.Lock()
	out := make([]IPStat, 0, len(s.byIP))
	for ip, c := range s.byIP {
		src := c.hours
		if useMinutes {
			src = c.minutes
		}
		var agg Bucket
		var lastBucket int64
		for k, b := range src {
			if k < lo || k > hi {
				continue
			}
			agg.Requests += b.Requests
			agg.Success += b.Success
			agg.Failed += b.Failed
			agg.InputTokens += b.InputTokens
			agg.OutputTokens += b.OutputTokens
			agg.CostUSD += b.CostUSD
			agg.SumLatencyMs += b.SumLatencyMs
			if k > lastBucket {
				lastBucket = k
			}
		}
		if agg.Requests == 0 {
			continue
		}
		st := IPStat{
			IP:           ip,
			Requests:     agg.Requests,
			Success:      agg.Success,
			Failed:       agg.Failed,
			InputTokens:  agg.InputTokens,
			OutputTokens: agg.OutputTokens,
			TotalTokens:  agg.InputTokens + agg.OutputTokens,
			CostUSD:      agg.CostUSD,
		}
		if useMinutes {
			st.LastUsed = lastBucket * 60000
		} else {
			st.LastUsed = lastBucket * 3600000
		}
		st.AvgLatencyMs = agg.SumLatencyMs / agg.Requests
		out = append(out, st)
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].LastUsed > out[j].LastUsed
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
