package metrics

import "sort"

// maxTrackedIPs caps the per-source-IP table. A busy proxy sees few distinct
// callers, but an untrusted deployment could be probed by many IPs; the cap
// keeps memory bounded while still surfacing the heavy hitters.
const maxTrackedIPs = 5000

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
