// Package metrics tracks outcomes of the upstream-forwarding path (requests
// forwarded to external OpenAI/Anthropic-compatible providers). It aggregates
// per-provider and per-route counters, retains a bounded ring buffer of recent
// events for the dashboard log, keeps rolling per-minute time-series buckets,
// and fans live events out to SSE subscribers.
//
// The package depends only on the standard library and never imports config or
// proxy, so callers pass all identity (provider/route ids and names) into
// Record. Record is non-blocking and never panics into the caller: a slow SSE
// consumer drops events rather than stalling the proxy response path, mirroring
// the logger broadcast hub.
package metrics

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	eventRingCapacity = 1000 // recent forward events retained for the log view
	bucketWindowMins  = 120  // per-minute time-series buckets kept (rolling)
	subscriberBuffer  = 256  // per-subscriber channel depth before dropping
)

// Event is a single forwarded-request outcome. It is retained in the ring
// buffer and broadcast to live subscribers. TimeMs is Unix milliseconds.
type Event struct {
	TimeMs       int64  `json:"time"`
	ClientModel  string `json:"clientModel"`
	TargetModel  string `json:"targetModel,omitempty"`
	RouteID      string `json:"routeId,omitempty"`
	ProviderID   string `json:"providerId,omitempty"`
	ProviderName string `json:"providerName,omitempty"`
	Status       int    `json:"status"`
	LatencyMs    int64  `json:"latencyMs"`
	Stream       bool   `json:"stream"`
	Ok           bool   `json:"ok"`
	ErrorMsg     string `json:"errorMsg,omitempty"`
}

// counter aggregates outcomes for the overall stream or one provider/route.
type counter struct {
	requests       int64
	success        int64
	failed         int64
	totalLatencyMs int64
	lastUsed       int64
}

func (c *counter) add(ev Event) {
	c.requests++
	if ev.Ok {
		c.success++
	} else {
		c.failed++
	}
	c.totalLatencyMs += ev.LatencyMs
	if ev.TimeMs > c.lastUsed {
		c.lastUsed = ev.TimeMs
	}
}

func (c *counter) avg() int64 {
	if c.requests == 0 {
		return 0
	}
	return c.totalLatencyMs / c.requests
}

type providerAgg struct {
	counter
	name string
}

type routeAgg struct {
	counter
	clientModel string
	targetModel string
	providerID  string
}

// Bucket is one minute of the rolling time-series. Minute is the Unix epoch
// minute (TimeMs / 60000). SumLatencyMs is internal; AvgLatencyMs is derived
// for output.
type Bucket struct {
	Minute       int64 `json:"minute"`
	Requests     int64 `json:"requests"`
	Success      int64 `json:"success"`
	Failed       int64 `json:"failed"`
	SumLatencyMs int64 `json:"-"`
	AvgLatencyMs int64 `json:"avgLatencyMs"`
}

type store struct {
	mu          sync.Mutex
	ring        []Event
	next        int
	full        bool
	overall     counter
	byProvider  map[string]*providerAgg
	byRoute     map[string]*routeAgg
	buckets     map[int64]*Bucket
	subscribers map[chan Event]struct{}
}

var s = &store{
	ring:        make([]Event, eventRingCapacity),
	byProvider:  make(map[string]*providerAgg),
	byRoute:     make(map[string]*routeAgg),
	buckets:     make(map[int64]*Bucket),
	subscribers: make(map[chan Event]struct{}),
}

// Record ingests one forward outcome: it updates the aggregate counters and
// time-series bucket, appends to the ring buffer, and broadcasts to live
// subscribers. Safe to call from the request path; sends to subscribers are
// non-blocking.
func Record(ev Event) {
	if ev.TimeMs == 0 {
		ev.TimeMs = time.Now().UnixMilli()
	}

	s.mu.Lock()

	s.ring[s.next] = ev
	s.next = (s.next + 1) % eventRingCapacity
	if s.next == 0 {
		s.full = true
	}

	s.overall.add(ev)

	if ev.ProviderID != "" {
		p := s.byProvider[ev.ProviderID]
		if p == nil {
			p = &providerAgg{}
			s.byProvider[ev.ProviderID] = p
		}
		if ev.ProviderName != "" {
			p.name = ev.ProviderName
		}
		p.add(ev)
	}

	if ev.RouteID != "" {
		r := s.byRoute[ev.RouteID]
		if r == nil {
			r = &routeAgg{}
			s.byRoute[ev.RouteID] = r
		}
		r.clientModel = ev.ClientModel
		r.targetModel = ev.TargetModel
		r.providerID = ev.ProviderID
		r.add(ev)
	}

	minute := ev.TimeMs / 60000
	b := s.buckets[minute]
	if b == nil {
		b = &Bucket{Minute: minute}
		s.buckets[minute] = b
	}
	b.Requests++
	if ev.Ok {
		b.Success++
	} else {
		b.Failed++
	}
	b.SumLatencyMs += ev.LatencyMs
	s.pruneBucketsLocked(minute)

	for ch := range s.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}

	s.mu.Unlock()
}

func (s *store) pruneBucketsLocked(nowMinute int64) {
	cutoff := nowMinute - bucketWindowMins
	for m := range s.buckets {
		if m < cutoff {
			delete(s.buckets, m)
		}
	}
}

// OverallStat is the aggregate across all forwarded requests.
type OverallStat struct {
	Requests     int64 `json:"requests"`
	Success      int64 `json:"success"`
	Failed       int64 `json:"failed"`
	AvgLatencyMs int64 `json:"avgLatencyMs"`
	LastUsed     int64 `json:"lastUsed"`
}

// Overall returns the aggregate across every forwarded request.
func Overall() OverallStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return OverallStat{
		Requests:     s.overall.requests,
		Success:      s.overall.success,
		Failed:       s.overall.failed,
		AvgLatencyMs: s.overall.avg(),
		LastUsed:     s.overall.lastUsed,
	}
}

// ProviderStat pairs a provider's identity with its aggregated counters.
type ProviderStat struct {
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName"`
	Requests     int64  `json:"requests"`
	Success      int64  `json:"success"`
	Failed       int64  `json:"failed"`
	AvgLatencyMs int64  `json:"avgLatencyMs"`
	LastUsed     int64  `json:"lastUsed"`
}

// ProviderStats returns per-provider aggregates, busiest first.
func ProviderStats() []ProviderStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProviderStat, 0, len(s.byProvider))
	for id, p := range s.byProvider {
		out = append(out, ProviderStat{
			ProviderID:   id,
			ProviderName: p.name,
			Requests:     p.requests,
			Success:      p.success,
			Failed:       p.failed,
			AvgLatencyMs: p.avg(),
			LastUsed:     p.lastUsed,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out
}

// RouteStat pairs a route's identity with its aggregated counters.
type RouteStat struct {
	RouteID      string `json:"routeId"`
	ClientModel  string `json:"clientModel"`
	TargetModel  string `json:"targetModel,omitempty"`
	ProviderID   string `json:"providerId"`
	Requests     int64  `json:"requests"`
	Success      int64  `json:"success"`
	Failed       int64  `json:"failed"`
	AvgLatencyMs int64  `json:"avgLatencyMs"`
	LastUsed     int64  `json:"lastUsed"`
}

// RouteStats returns per-route aggregates, busiest first.
func RouteStats() []RouteStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RouteStat, 0, len(s.byRoute))
	for id, r := range s.byRoute {
		out = append(out, RouteStat{
			RouteID:      id,
			ClientModel:  r.clientModel,
			TargetModel:  r.targetModel,
			ProviderID:   r.providerID,
			Requests:     r.requests,
			Success:      r.success,
			Failed:       r.failed,
			AvgLatencyMs: r.avg(),
			LastUsed:     r.lastUsed,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out
}

// EventFilter selects and paginates events for the log view. Status is
// "success", "error", or "" (any). Model is a case-insensitive substring
// matched against both client and target model names.
type EventFilter struct {
	ProviderID string
	Status     string
	Model      string
	Offset     int
	Limit      int
}

// Events returns events newest-first after applying the filter, plus the total
// number of matches before pagination.
func Events(f EventFilter) ([]Event, int) {
	s.mu.Lock()
	all := s.snapshotEventsLocked()
	s.mu.Unlock()

	model := strings.ToLower(f.Model)
	matched := make([]Event, 0, len(all))
	for _, e := range all {
		if f.ProviderID != "" && e.ProviderID != f.ProviderID {
			continue
		}
		if f.Status == "success" && !e.Ok {
			continue
		}
		if f.Status == "error" && e.Ok {
			continue
		}
		if model != "" &&
			!strings.Contains(strings.ToLower(e.ClientModel), model) &&
			!strings.Contains(strings.ToLower(e.TargetModel), model) {
			continue
		}
		matched = append(matched, e)
	}

	total := len(matched)
	start := f.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := total
	if f.Limit > 0 && start+f.Limit < end {
		end = start + f.Limit
	}
	return matched[start:end], total
}

// snapshotEventsLocked returns a copy of the ring in newest-first order.
func (s *store) snapshotEventsLocked() []Event {
	var out []Event
	if !s.full {
		out = make([]Event, s.next)
		copy(out, s.ring[:s.next])
	} else {
		out = make([]Event, 0, eventRingCapacity)
		out = append(out, s.ring[s.next:]...)
		out = append(out, s.ring[:s.next]...)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Subscribe registers a live event subscriber and returns its channel plus a
// cancel function that must be called to release resources.
func Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subscribers, ch)
			s.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// TimeSeries returns the last `minutes` per-minute buckets ending at the
// current minute, oldest first, with gaps filled by empty buckets.
func TimeSeries(minutes int) []Bucket {
	if minutes <= 0 || minutes > bucketWindowMins {
		minutes = 60
	}
	nowMinute := time.Now().UnixMilli() / 60000

	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Bucket, 0, minutes)
	for i := minutes - 1; i >= 0; i-- {
		m := nowMinute - int64(i)
		if b := s.buckets[m]; b != nil {
			bc := *b
			if bc.Requests > 0 {
				bc.AvgLatencyMs = bc.SumLatencyMs / bc.Requests
			}
			out = append(out, bc)
		} else {
			out = append(out, Bucket{Minute: m})
		}
	}
	return out
}

// Percentiles reports latency percentiles computed from the successful events
// currently in the ring buffer.
type Percentiles struct {
	P50   int64 `json:"p50"`
	P95   int64 `json:"p95"`
	P99   int64 `json:"p99"`
	Count int   `json:"count"`
}

// LatencyPercentiles computes p50/p95/p99 over successful events in the ring.
func LatencyPercentiles() Percentiles {
	s.mu.Lock()
	all := s.snapshotEventsLocked()
	s.mu.Unlock()

	lat := make([]int64, 0, len(all))
	for _, e := range all {
		if e.Ok {
			lat = append(lat, e.LatencyMs)
		}
	}
	if len(lat) == 0 {
		return Percentiles{}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pick := func(p float64) int64 {
		idx := int(float64(len(lat)-1) * p)
		return lat[idx]
	}
	return Percentiles{
		P50:   pick(0.50),
		P95:   pick(0.95),
		P99:   pick(0.99),
		Count: len(lat),
	}
}

// Reset clears all counters, events, and time-series data. Subscribers stay
// connected and will receive subsequent events.
func Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = make([]Event, eventRingCapacity)
	s.next = 0
	s.full = false
	s.overall = counter{}
	s.byProvider = make(map[string]*providerAgg)
	s.byRoute = make(map[string]*routeAgg)
	s.buckets = make(map[int64]*Bucket)
}
