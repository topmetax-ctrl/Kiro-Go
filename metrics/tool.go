package metrics

// Tool observability — generic server/tool execution metrics.
//
// This package intentionally does NOT turn tool executions into fake requests.
// A Claude request with 3 web_search tool uses remains:
//   Claude requests = 1, web_search uses = 3
// See metrics.go: Event invariant (1 Event = 1 request/attempt outcome).
//
// ToolUsage is a parallel signal with its own ring, counters and hourly rollups,
// sharing the same design properties as the request metrics:
//   - stdlib-only, no import of config/proxy
//   - bounded (ring + 120-min window + 30-day hourly)
//   - non-blocking broadcast (slow SSE subscriber drops)
//   - additive/backward-compatible persistence
//
// Cardinality discipline (Prometheus/OpenTelemetry guidance):
//   - Aggregation keys are bounded enums only: ToolKind, Origin, Backend.
//   - Raw query, URL, tool_call_id, user ID, request ID NEVER become map keys.
//   - RequestID is kept on the ring event for log/trace correlation only.
//
// OTel GenAI alignment: this is the "execute_tool" operation dimension,
// distinct from the inference request. An exporter can later map ToolUsage
// to OTel spans without redesigning the domain.

import (
	"sync"
	"time"
)

// ToolKind is the kind of server/tool execution. Bounded set; adding a new
// kind (web_fetch, retrieval, mcp, ...) only adds one constant here — the
// aggregation maps stay small.
type ToolKind string

const (
	ToolKindWebSearch ToolKind = "web_search"
)

// ToolOrigin names who owned the execution. Low cardinality.
type ToolOrigin string

const (
	// ToolOriginUpstreamNative: Kiro-Go did not execute the tool; the upstream
	// provider did and Kiro-Go only observed usage.server_tool_use.
	ToolOriginUpstreamNative ToolOrigin = "upstream_native"
	// ToolOriginKiroOrchestrator: search.Orchestrator (SearXNG/Tavily via ProviderRouter).
	ToolOriginKiroOrchestrator ToolOrigin = "kiro_orchestrator"
	// ToolOriginKiroMCP: legacy Kiro MCP path (pure/mixed). Kept distinct for migration visibility.
	ToolOriginKiroMCP ToolOrigin = "kiro_mcp"
)

// ToolUsage is one logical tool observation. For local (Kiro-managed) tools,
// Uses and Executions may differ because of dedup/cache. For opaque upstream-
// native tools only Uses is known; Executions/CacheHits/Backend remain zero/empty.
type ToolUsage struct {
	TimeMs     int64      `json:"time"`
	RequestID  string     `json:"requestId,omitempty"` // correlation only, not a metric label
	ToolKind   ToolKind   `json:"toolKind"`
	Origin     ToolOrigin `json:"origin"`
	ProviderID string     `json:"providerId,omitempty"`
	Backend    string     `json:"backend,omitempty"` // actual backend: searxng/tavily etc. (only when observed)

	Uses       int64 `json:"uses"`       // logical tool uses the model requested
	Executions int64 `json:"executions"` // actual backend executions (0 when not observable)
	CacheHits  int64 `json:"cacheHits"`
	Failures   int64 `json:"failures"`
	LatencyMs  int64 `json:"latencyMs"`
	Credits    int64 `json:"credits"` // Tavily credits etc.
}

// toolCounter aggregates tool usage for one (kind, origin, backend) slice.
type toolCounter struct {
	uses       int64
	executions int64
	cacheHits  int64
	failures   int64
	totalLatencyMs int64
	credits    int64
	requests   int64 // how many logical requests contributed (for RequestsUsingTool)
	lastUsed   int64
}

func (c *toolCounter) add(u ToolUsage) {
	c.uses += u.Uses
	c.executions += u.Executions
	c.cacheHits += u.CacheHits
	c.failures += u.Failures
	c.totalLatencyMs += u.LatencyMs
	c.credits += u.Credits
	if u.Uses > 0 || u.Failures > 0 {
		c.requests++
	}
	if u.TimeMs > c.lastUsed {
		c.lastUsed = u.TimeMs
	}
}

func (c *toolCounter) sub(o toolCounter) {
	c.uses -= o.uses
	c.executions -= o.executions
	c.cacheHits -= o.cacheHits
	c.failures -= o.failures
	c.totalLatencyMs -= o.totalLatencyMs
	c.credits -= o.credits
	c.requests -= o.requests
	c.clampNonNegative()
}

func (c *toolCounter) clampNonNegative() {
	if c.uses < 0 { c.uses = 0 }
	if c.executions < 0 { c.executions = 0 }
	if c.cacheHits < 0 { c.cacheHits = 0 }
	if c.failures < 0 { c.failures = 0 }
	if c.totalLatencyMs < 0 { c.totalLatencyMs = 0 }
	if c.credits < 0 { c.credits = 0 }
	if c.requests < 0 { c.requests = 0 }
}

func (c *toolCounter) avgLatency() int64 {
	if c.uses == 0 && c.requests == 0 {
		return 0
	}
	den := c.uses
	if den == 0 {
		den = c.requests
	}
	if den == 0 {
		return 0
	}
	return c.totalLatencyMs / den
}

// toolBucket is one hour bucket for tool stats (minute buckets are memory-only for requests; tools use hourly only to keep impl small).
type toolBucket struct {
	Hour       int64 `json:"hour"`
	Uses       int64 `json:"uses"`
	Executions int64 `json:"executions"`
	CacheHits  int64 `json:"cacheHits"`
	Failures   int64 `json:"failures"`
	Credits    int64 `json:"credits"`
	SumLatencyMs int64 `json:"-"`
	AvgLatencyMs int64 `json:"avgLatencyMs"`
	Requests   int64 `json:"requests"`
}

func (b *toolBucket) add(u ToolUsage) {
	b.Uses += u.Uses
	b.Executions += u.Executions
	b.CacheHits += u.CacheHits
	b.Failures += u.Failures
	b.Credits += u.Credits
	if u.Uses > 0 || u.Failures > 0 {
		b.Requests++
	}
	b.SumLatencyMs += u.LatencyMs
}

func (b toolBucket) derive() toolBucket {
	den := b.Uses
	if den == 0 {
		den = b.Requests
	}
	if den > 0 {
		b.AvgLatencyMs = b.SumLatencyMs / den
	}
	return b
}

const (
	toolRingCapacity = 5000
	toolSubscriberBuffer = 256
)

// toolProviderAgg is one origin/backend slice inside a ToolKind.
type toolAgg struct {
	toolCounter
	byOrigin map[ToolOrigin]*toolCounter
	byBackend map[string]*toolCounter
	hours    map[int64]*toolBucket
}

func newToolAgg() *toolAgg {
	return &toolAgg{
		byOrigin: make(map[ToolOrigin]*toolCounter),
		byBackend: make(map[string]*toolCounter),
		hours:    make(map[int64]*toolBucket),
	}
}

type toolStore struct {
	mu          sync.Mutex
	ring        []ToolUsage
	next        int
	full        bool
	overall     map[ToolKind]*toolAgg
	subscribers map[chan ToolUsage]struct{}
}

var ts = newToolStore()

func newToolStore() *toolStore {
	return &toolStore{
		ring:        make([]ToolUsage, toolRingCapacity),
		overall:     make(map[ToolKind]*toolAgg),
		subscribers: make(map[chan ToolUsage]struct{}),
	}
}

func (s *toolStore) aggLocked(kind ToolKind) *toolAgg {
	a := s.overall[kind]
	if a == nil {
		a = newToolAgg()
		s.overall[kind] = a
	}
	return a
}

// RecordToolUsage ingests one tool observation. Non-blocking, never panics.
func RecordToolUsage(u ToolUsage) {
	if u.TimeMs == 0 {
		u.TimeMs = time.Now().UnixMilli()
	}
	if u.ToolKind == "" {
		return
	}
	ts.mu.Lock()

	ts.ring[ts.next] = u
	ts.next = (ts.next + 1) % toolRingCapacity
	if ts.next == 0 {
		ts.full = true
	}

	a := ts.aggLocked(u.ToolKind)
	a.add(u)

	if u.Origin != "" {
		oc := a.byOrigin[u.Origin]
		if oc == nil {
			oc = &toolCounter{}
			a.byOrigin[u.Origin] = oc
		}
		oc.add(u)
	}
	if u.Backend != "" {
		bc := a.byBackend[u.Backend]
		if bc == nil {
			bc = &toolCounter{}
			a.byBackend[u.Backend] = bc
		}
		bc.add(u)
	}

	hour := u.TimeMs / 3600000
	b := a.hours[hour]
	if b == nil {
		b = &toolBucket{Hour: hour}
		a.hours[hour] = b
	}
	b.add(u)
	pruneToolBuckets(a.hours, hour-hourWindowHours)

	for ch := range ts.subscribers {
		select {
		case ch <- u:
		default:
		}
	}

	ts.mu.Unlock()
}

func pruneToolBuckets(m map[int64]*toolBucket, cutoff int64) {
	for k := range m {
		if k < cutoff {
			delete(m, k)
		}
	}
}

// ToolStat is the aggregate for one ToolKind.
type ToolStat struct {
	ToolKind     ToolKind `json:"toolKind"`
	Uses         int64    `json:"uses"`
	Executions   int64    `json:"executions"`
	CacheHits    int64    `json:"cacheHits"`
	Failures     int64    `json:"failures"`
	Credits      int64    `json:"credits"`
	Requests     int64    `json:"requests"` // logical requests that used the tool
	AvgLatencyMs int64    `json:"avgLatencyMs"`
	LastUsed     int64    `json:"lastUsed"`
	ByOrigin     map[ToolOrigin]int64 `json:"byOrigin,omitempty"`
	ByBackend    map[string]int64     `json:"byBackend,omitempty"`
}

// ToolStats returns one aggregate per ToolKind, busiest first.
func ToolStats() []ToolStat {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]ToolStat, 0, len(ts.overall))
	for kind, a := range ts.overall {
		st := ToolStat{
			ToolKind:     kind,
			Uses:         a.uses,
			Executions:   a.executions,
			CacheHits:    a.cacheHits,
			Failures:     a.failures,
			Credits:      a.credits,
			Requests:     a.requests,
			AvgLatencyMs: a.avgLatency(),
			LastUsed:     a.lastUsed,
		}
		if len(a.byOrigin) > 0 {
			st.ByOrigin = make(map[ToolOrigin]int64, len(a.byOrigin))
			for o, c := range a.byOrigin {
				st.ByOrigin[o] = c.uses
			}
		}
		if len(a.byBackend) > 0 {
			st.ByBackend = make(map[string]int64, len(a.byBackend))
			for b, c := range a.byBackend {
				st.ByBackend[b] = c.executions
			}
		}
		out = append(out, st)
	}
	// busiest first
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j].Uses > out[j-1].Uses {
			out[j], out[j-1] = out[j-1], out[j]
			j--
		}
	}
	return out
}

// ToolStatsFor returns the aggregate for one kind, or zero when none.
func ToolStatsFor(kind ToolKind) ToolStat {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	a := ts.overall[kind]
	if a == nil {
		return ToolStat{ToolKind: kind}
	}
	st := ToolStat{
		ToolKind:     kind,
		Uses:         a.uses,
		Executions:   a.executions,
		CacheHits:    a.cacheHits,
		Failures:     a.failures,
		Credits:      a.credits,
		Requests:     a.requests,
		AvgLatencyMs: a.avgLatency(),
		LastUsed:     a.lastUsed,
	}
	if len(a.byOrigin) > 0 {
		st.ByOrigin = make(map[ToolOrigin]int64, len(a.byOrigin))
		for o, c := range a.byOrigin {
			st.ByOrigin[o] = c.uses
		}
	}
	if len(a.byBackend) > 0 {
		st.ByBackend = make(map[string]int64, len(a.byBackend))
		for b, c := range a.byBackend {
			st.ByBackend[b] = c.executions
		}
	}
	return st
}

// ToolHistory returns hourly buckets for one kind, oldest first.
func ToolHistory(kind ToolKind, hours int) []toolBucket {
	if hours <= 0 || hours > hourWindowHours {
		hours = 24
	}
	nowHour := time.Now().UnixMilli() / 3600000
	ts.mu.Lock()
	defer ts.mu.Unlock()
	a := ts.overall[kind]
	if a == nil {
		out := make([]toolBucket, hours)
		for i := range out {
			out[i].Hour = nowHour - int64(hours-1-i)
		}
		return out
	}
	out := make([]toolBucket, 0, hours)
	for i := int64(hours - 1); i >= 0; i-- {
		k := nowHour - i
		if b := a.hours[k]; b != nil {
			out = append(out, b.derive())
		} else {
			out = append(out, toolBucket{Hour: k})
		}
	}
	return out
}

// ToolEvents returns ring events newest-first, filtered by kind when non-empty.
func ToolEvents(kind ToolKind, limit int) ([]ToolUsage, int) {
	ts.mu.Lock()
	all := ts.snapshotLocked()
	ts.mu.Unlock()
	matched := make([]ToolUsage, 0, len(all))
	for _, u := range all {
		if kind != "" && u.ToolKind != kind {
			continue
		}
		matched = append(matched, u)
	}
	total := len(matched)
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, total
}

func (s *toolStore) snapshotLocked() []ToolUsage {
	var out []ToolUsage
	if !s.full {
		out = make([]ToolUsage, s.next)
		copy(out, s.ring[:s.next])
	} else {
		out = make([]ToolUsage, 0, toolRingCapacity)
		out = append(out, s.ring[s.next:]...)
		out = append(out, s.ring[:s.next]...)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// SubscribeTool registers a live tool-usage subscriber.
func SubscribeTool() (<-chan ToolUsage, func()) {
	ch := make(chan ToolUsage, toolSubscriberBuffer)
	ts.mu.Lock()
	ts.subscribers[ch] = struct{}{}
	ts.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			ts.mu.Lock()
			delete(ts.subscribers, ch)
			ts.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// ResetToolStats clears all tool aggregates and ring. Subscribers stay connected.
func ResetToolStats() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.ring = make([]ToolUsage, toolRingCapacity)
	ts.next = 0
	ts.full = false
	ts.overall = make(map[ToolKind]*toolAgg)
}

// ResetToolKind clears one kind.
func ResetToolKind(kind ToolKind) bool {
	if kind == "" {
		return false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if _, ok := ts.overall[kind]; !ok {
		return false
	}
	delete(ts.overall, kind)
	kept := ts.snapshotLocked()
	ts.ring = make([]ToolUsage, toolRingCapacity)
	ts.next = 0
	ts.full = false
	for i := len(kept) - 1; i >= 0; i-- {
		u := kept[i]
		if u.ToolKind == kind {
			continue
		}
		ts.ring[ts.next] = u
		ts.next = (ts.next + 1) % toolRingCapacity
		if ts.next == 0 {
			ts.full = true
		}
	}
	return true
}
