// Package metrics tracks outcomes of every request this proxy serves, both the
// upstream-forwarding path (requests relayed to external OpenAI/Anthropic-compatible
// providers) and the Kiro account pool, which is recorded under the synthetic
// provider id KiroPoolID so both appear side by side in the admin dashboard.
//
// It aggregates per-provider, per-route, per-model and per-account counters,
// retains a bounded ring buffer of recent events for the log view, keeps rolling
// per-minute buckets (global and per-provider) plus per-provider hourly rollups
// for long-term history, and fans live events out to SSE subscribers.
//
// The package depends only on the standard library and never imports config or
// proxy, so callers pass all identity (provider/route/account ids and names) into
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
	eventRingCapacity = 5000 // recent events retained for the log view
	bucketWindowMins  = 120  // per-minute time-series buckets kept (rolling)
	hourWindowHours   = 720  // per-provider hourly rollups kept (30 days)
	subscriberBuffer  = 256  // per-subscriber channel depth before dropping
	recentErrorsKept  = 10   // most recent error samples retained per provider
	topStatusCodes    = 12   // distinct status codes retained per provider

	// KiroPoolID is the synthetic provider id under which Kiro account-pool
	// traffic is recorded, so the pool appears alongside forwarding providers.
	// It is a fixed sentinel, never a real UpstreamProvider UUID.
	KiroPoolID   = "__kiro_pool__"
	KiroPoolName = "Kiro Pool"
)

// Event is a single request outcome. It is retained in the ring buffer and
// broadcast to live subscribers. TimeMs is Unix milliseconds.
//
// For forwarded requests ProviderID is the UpstreamProvider id; for Kiro-pool
// requests it is KiroPoolID and AccountID/AccountLabel identify the account that
// served the request.
type Event struct {
	TimeMs         int64   `json:"time"`
	ClientModel    string  `json:"clientModel"`
	TargetModel    string  `json:"targetModel,omitempty"`
	RouteID        string  `json:"routeId,omitempty"`
	ProviderID     string  `json:"providerId,omitempty"`
	ProviderName   string  `json:"providerName,omitempty"`
	ConnectionID   string  `json:"connectionId,omitempty"`
	ConnectionName string  `json:"connectionName,omitempty"`
	AccountID      string  `json:"accountId,omitempty"`
	AccountLabel   string  `json:"accountLabel,omitempty"`
	Endpoint       string  `json:"endpoint,omitempty"` // claude/openai/responses/websearch
	ClientIP       string  `json:"clientIp,omitempty"` // caller's source IP; empty when not resolved
	RequestID      string  `json:"requestId,omitempty"`
	ApiKeyID       string  `json:"apiKeyId,omitempty"`
	Status         int     `json:"status"`
	LatencyMs      int64   `json:"latencyMs"`
	TTFBMs         int64   `json:"ttfbMs,omitempty"` // time to first response byte; 0 when unknown
	InputTokens    int64   `json:"inputTokens,omitempty"`
	OutputTokens   int64   `json:"outputTokens,omitempty"`
	CostUSD        float64 `json:"costUsd,omitempty"`
	Stream         bool    `json:"stream"`
	Canceled       bool    `json:"canceled,omitempty"` // client disconnected mid-flight
	Ok             bool    `json:"ok"`
	ErrorMsg       string  `json:"errorMsg,omitempty"`
	// Attempt is the zero-based index of this try within a route's ranked target
	// list. 0 means the route's preferred provider served it; >0 means a failover
	// happened, which is the signal that a higher-priority target is unhealthy.
	Attempt int `json:"attempt,omitempty"`
}

// counter aggregates outcomes for the overall stream or one provider/route/model.
//
// Latency and token totals are running sums; averages are derived on read. TTFB
// keeps its own count because it is only known for a subset of requests (streams
// and non-stream relays where the caller measured it), so averaging it over
// `requests` would understate it.
type counter struct {
	requests       int64
	success        int64
	failed         int64
	canceled       int64
	streamed       int64
	totalLatencyMs int64
	totalTTFBMs    int64
	ttfbCount      int64
	inputTokens    int64
	outputTokens   int64
	costUSD        float64
	lastUsed       int64
}

func (c *counter) add(ev Event) {
	c.requests++
	switch {
	case ev.Canceled:
		// A client disconnect is neither an upstream success nor an upstream
		// failure. Counting it as failed would blame the provider for the
		// client's choice and drag down its success rate, so cancellations are
		// tracked as their own third category.
		c.canceled++
	case ev.Ok:
		c.success++
	default:
		c.failed++
	}
	if ev.Stream {
		c.streamed++
	}
	c.totalLatencyMs += ev.LatencyMs
	if ev.TTFBMs > 0 {
		c.totalTTFBMs += ev.TTFBMs
		c.ttfbCount++
	}
	c.inputTokens += ev.InputTokens
	c.outputTokens += ev.OutputTokens
	c.costUSD += ev.CostUSD
	if ev.TimeMs > c.lastUsed {
		c.lastUsed = ev.TimeMs
	}
}

// sub removes another counter's totals from this one. Used when a single
// provider is reset: its lifetime contribution must come out of the aggregate,
// which spans more history than the bounded event ring can replay.
func (c *counter) sub(o counter) {
	c.requests -= o.requests
	c.success -= o.success
	c.failed -= o.failed
	c.canceled -= o.canceled
	c.streamed -= o.streamed
	c.totalLatencyMs -= o.totalLatencyMs
	c.totalTTFBMs -= o.totalTTFBMs
	c.ttfbCount -= o.ttfbCount
	c.inputTokens -= o.inputTokens
	c.outputTokens -= o.outputTokens
	c.costUSD -= o.costUSD
	// lastUsed is a max, not a sum: it cannot be un-mixed, so it is left as is
	// rather than guessed at.
	c.clampNonNegative()
}

// clampNonNegative guards against drift: counters loaded from an older on-disk
// file may not perfectly account for every per-provider total, and a negative
// request count would render as nonsense.
func (c *counter) clampNonNegative() {
	if c.requests < 0 {
		c.requests = 0
	}
	if c.success < 0 {
		c.success = 0
	}
	if c.failed < 0 {
		c.failed = 0
	}
	if c.canceled < 0 {
		c.canceled = 0
	}
	if c.streamed < 0 {
		c.streamed = 0
	}
	if c.totalLatencyMs < 0 {
		c.totalLatencyMs = 0
	}
	if c.totalTTFBMs < 0 {
		c.totalTTFBMs = 0
	}
	if c.ttfbCount < 0 {
		c.ttfbCount = 0
	}
	if c.inputTokens < 0 {
		c.inputTokens = 0
	}
	if c.outputTokens < 0 {
		c.outputTokens = 0
	}
	if c.costUSD < 0 {
		c.costUSD = 0
	}
}

func (c *counter) avg() int64 {
	if c.requests == 0 {
		return 0
	}
	return c.totalLatencyMs / c.requests
}

func (c *counter) avgTTFB() int64 {
	if c.ttfbCount == 0 {
		return 0
	}
	return c.totalTTFBMs / c.ttfbCount
}

// providerAgg holds a provider's counters plus the dimensions that only make
// sense per provider: status-code histogram, health/streak tracking, recent
// error samples, live concurrency, and its own minute/hour time series.
type providerAgg struct {
	counter
	name       string
	byStatus   map[int]int64
	byModel    map[string]*counter
	byAccount  map[string]*accountAgg
	recentErrs []ErrorSample
	curStreak  int64 // consecutive failures right now
	maxStreak  int64
	lastOkMs   int64
	lastFailMs int64
	inFlight   int64
	peakFlight int64
	minutes    map[int64]*Bucket
	hours      map[int64]*Bucket
}

func newProviderAgg() *providerAgg {
	return &providerAgg{
		byStatus:  make(map[int]int64),
		byModel:   make(map[string]*counter),
		byAccount: make(map[string]*accountAgg),
		minutes:   make(map[int64]*Bucket),
		hours:     make(map[int64]*Bucket),
	}
}

type accountAgg struct {
	counter
	label string
}

type routeAgg struct {
	counter
	clientModel string
	targetModel string
	providerID  string
}

// ErrorSample is one retained failure, shown in the provider detail panel so an
// operator can see what is actually failing without grepping logs.
type ErrorSample struct {
	TimeMs  int64  `json:"time"`
	Status  int    `json:"status"`
	Model   string `json:"model,omitempty"`
	Message string `json:"message,omitempty"`
}

// Bucket is one minute (or one hour) of a time series. Minute is the Unix epoch
// minute (TimeMs / 60000); for hourly buckets it is the Unix epoch hour.
// SumLatencyMs is internal; AvgLatencyMs is derived for output.
type Bucket struct {
	Minute       int64 `json:"minute"`
	Requests     int64 `json:"requests"`
	Success      int64 `json:"success"`
	Failed       int64 `json:"failed"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	// CostUSD lets spend be summed over an arbitrary window (the Stats tab's
	// range filter) rather than only all-time from the provider counter.
	CostUSD      float64 `json:"costUsd"`
	SumLatencyMs int64   `json:"-"`
	AvgLatencyMs int64   `json:"avgLatencyMs"`
}

func (b *Bucket) add(ev Event) {
	b.Requests++
	// Cancellations count toward volume but toward neither outcome, matching
	// counter.add: the error band of a sparkline should show upstream failures,
	// not the user closing a tab. Requests may therefore exceed Success+Failed.
	switch {
	case ev.Canceled:
	case ev.Ok:
		b.Success++
	default:
		b.Failed++
	}
	b.InputTokens += ev.InputTokens
	b.OutputTokens += ev.OutputTokens
	b.CostUSD += ev.CostUSD
	b.SumLatencyMs += ev.LatencyMs
}

// derive fills AvgLatencyMs on a copy for output.
func (b Bucket) derive() Bucket {
	if b.Requests > 0 {
		b.AvgLatencyMs = b.SumLatencyMs / b.Requests
	}
	return b
}

type store struct {
	mu          sync.Mutex
	ring        []Event
	next        int
	full        bool
	overall     counter
	byProvider  map[string]*providerAgg
	byRoute     map[string]*routeAgg
	byIP        map[string]*counter
	buckets     map[int64]*Bucket
	subscribers map[chan Event]struct{}
}

var s = newStore()

func newStore() *store {
	return &store{
		ring:        make([]Event, eventRingCapacity),
		byProvider:  make(map[string]*providerAgg),
		byRoute:     make(map[string]*routeAgg),
		byIP:        make(map[string]*counter),
		buckets:     make(map[int64]*Bucket),
		subscribers: make(map[chan Event]struct{}),
	}
}

// BeginInFlight marks one request as in flight against a provider and returns a
// done func that must be called when the request finishes. It powers the live
// concurrency and peak-concurrency figures. Safe to call with an empty id (no-op).
func BeginInFlight(providerID, providerName string) func() {
	if providerID == "" {
		return func() {}
	}
	s.mu.Lock()
	p := s.providerLocked(providerID, providerName)
	p.inFlight++
	if p.inFlight > p.peakFlight {
		p.peakFlight = p.inFlight
	}
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if pa := s.byProvider[providerID]; pa != nil && pa.inFlight > 0 {
				pa.inFlight--
			}
			s.mu.Unlock()
		})
	}
}

// providerLocked fetches or creates a provider aggregate. Caller holds s.mu.
func (s *store) providerLocked(id, name string) *providerAgg {
	p := s.byProvider[id]
	if p == nil {
		p = newProviderAgg()
		s.byProvider[id] = p
	}
	if name != "" {
		p.name = name
	}
	return p
}

// Record ingests one request outcome: it updates the aggregate counters, the
// per-provider breakdowns and time series, appends to the ring buffer, and
// broadcasts to live subscribers. Safe to call from the request path; sends to
// subscribers are non-blocking.
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

	minute := ev.TimeMs / 60000
	hour := ev.TimeMs / 3600000

	if ev.ProviderID != "" {
		p := s.providerLocked(ev.ProviderID, ev.ProviderName)
		p.add(ev)

		if ev.Status != 0 {
			// Bound the histogram so a pathological upstream emitting many distinct
			// codes cannot grow this map without limit.
			if _, seen := p.byStatus[ev.Status]; seen || len(p.byStatus) < topStatusCodes {
				p.byStatus[ev.Status]++
			}
		}

		if m := strings.TrimSpace(ev.ClientModel); m != "" {
			mc := p.byModel[m]
			if mc == nil {
				mc = &counter{}
				p.byModel[m] = mc
			}
			mc.add(ev)
		}

		if ev.AccountID != "" {
			ac := p.byAccount[ev.AccountID]
			if ac == nil {
				ac = &accountAgg{}
				p.byAccount[ev.AccountID] = ac
			}
			if ev.AccountLabel != "" {
				ac.label = ev.AccountLabel
			}
			ac.add(ev)
		}

		// Health and the recent-error list track upstream behaviour only. A client
		// disconnect leaves both untouched: it must not break a success streak,
		// mark a working provider dead, or occupy a slot in the error list (where
		// it would surface as a confusing "200 error").
		switch {
		case ev.Canceled:
			// no reliability signal either way
		case ev.Ok:
			p.curStreak = 0
			p.lastOkMs = ev.TimeMs
		default:
			p.curStreak++
			if p.curStreak > p.maxStreak {
				p.maxStreak = p.curStreak
			}
			p.lastFailMs = ev.TimeMs
			p.recentErrs = append(p.recentErrs, ErrorSample{
				TimeMs:  ev.TimeMs,
				Status:  ev.Status,
				Model:   ev.ClientModel,
				Message: ev.ErrorMsg,
			})
			if len(p.recentErrs) > recentErrorsKept {
				p.recentErrs = p.recentErrs[len(p.recentErrs)-recentErrorsKept:]
			}
		}

		pb := p.minutes[minute]
		if pb == nil {
			pb = &Bucket{Minute: minute}
			p.minutes[minute] = pb
		}
		pb.add(ev)
		pruneBuckets(p.minutes, minute-bucketWindowMins)

		ph := p.hours[hour]
		if ph == nil {
			ph = &Bucket{Minute: hour}
			p.hours[hour] = ph
		}
		ph.add(ev)
		pruneBuckets(p.hours, hour-hourWindowHours)
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

	if ev.ClientIP != "" {
		ic := s.byIP[ev.ClientIP]
		// Bound the map so a flood of distinct source IPs (e.g. a scan) cannot
		// grow it without limit. Once full, only already-tracked IPs update.
		if ic == nil && len(s.byIP) < maxTrackedIPs {
			ic = &counter{}
			s.byIP[ev.ClientIP] = ic
		}
		if ic != nil {
			ic.add(ev)
		}
	}

	b := s.buckets[minute]
	if b == nil {
		b = &Bucket{Minute: minute}
		s.buckets[minute] = b
	}
	b.add(ev)
	pruneBuckets(s.buckets, minute-bucketWindowMins)

	for ch := range s.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}

	s.mu.Unlock()
}

// pruneBuckets drops buckets older than cutoff (exclusive).
func pruneBuckets(m map[int64]*Bucket, cutoff int64) {
	for k := range m {
		if k < cutoff {
			delete(m, k)
		}
	}
}

// OverallStat is the aggregate across all requests.
type OverallStat struct {
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	Canceled     int64   `json:"canceled"`
	AvgLatencyMs int64   `json:"avgLatencyMs"`
	AvgTTFBMs    int64   `json:"avgTtfbMs"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
	LastUsed     int64   `json:"lastUsed"`
}

// Overall returns the aggregate across every recorded request.
func Overall() OverallStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.overall
	return OverallStat{
		Requests:     c.requests,
		Success:      c.success,
		Failed:       c.failed,
		Canceled:     c.canceled,
		AvgLatencyMs: c.avg(),
		AvgTTFBMs:    c.avgTTFB(),
		InputTokens:  c.inputTokens,
		OutputTokens: c.outputTokens,
		CostUSD:      c.costUSD,
		LastUsed:     c.lastUsed,
	}
}

// ProviderStat pairs a provider's identity with its aggregated counters and the
// derived health/throughput figures shown in the comparison table.
type ProviderStat struct {
	ProviderID   string  `json:"providerId"`
	ProviderName string  `json:"providerName"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	Canceled     int64   `json:"canceled"`
	Streamed     int64   `json:"streamed"`
	SuccessRate  float64 `json:"successRate"` // 0..100, -1 when no requests
	AvgLatencyMs int64   `json:"avgLatencyMs"`
	AvgTTFBMs    int64   `json:"avgTtfbMs"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
	RPM          float64 `json:"rpm"`          // requests/min over the last 5 min
	TPM          float64 `json:"tpm"`          // tokens/min over the last 5 min
	TokensPerSec float64 `json:"tokensPerSec"` // output tokens per second of latency
	InFlight     int64   `json:"inFlight"`
	PeakInFlight int64   `json:"peakInFlight"`
	FailStreak   int64   `json:"failStreak"`
	MaxStreak    int64   `json:"maxFailStreak"`
	LastOk       int64   `json:"lastOk"`
	LastFail     int64   `json:"lastFail"`
	LastUsed     int64   `json:"lastUsed"`
	Healthy      bool    `json:"healthy"`
}

// successRate returns the success percentage, or -1 when there is no traffic so
// the UI can render "—" instead of a misleading 0%.
//
// Cancellations are excluded from the denominator: they measure client
// behaviour, not provider reliability, so a user who habitually aborts long
// answers must not make a perfectly healthy provider look like it is failing.
// A provider whose only traffic was canceled therefore reports -1 (unknown)
// rather than 0%.
func (c *counter) successRate() float64 {
	return rateOf(c.success, c.requests-c.canceled)
}

// rateOf is the shared success-percentage formula: -1 when nothing was decided,
// otherwise success as a percentage of the decided requests. Bucket-derived
// stats reuse it so a window-scoped rate is defined identically to an all-time
// one; buckets have no canceled counter, so their decided count is success+failed.
func rateOf(success, decided int64) float64 {
	if decided <= 0 {
		return -1
	}
	return float64(success) * 100 / float64(decided)
}

// tokensPerSec is output tokens divided by total latency seconds — a throughput
// figure comparable across providers regardless of response length.
//
// It is reported only once there is enough measured latency for the ratio to
// mean anything: with a sub-100ms total (a local or mocked upstream, where each
// request rounds to 0ms) the division yields millions of tokens/sec, which is
// arithmetically correct and completely misleading.
func (c *counter) tokensPerSec() float64 {
	const minLatencyMsForRate = 100
	if c.totalLatencyMs < minLatencyMsForRate {
		return 0
	}
	return float64(c.outputTokens) / (float64(c.totalLatencyMs) / 1000)
}

// rateWindowMins is the trailing window used for the live RPM/TPM figures.
const rateWindowMins = 5

// ratesLocked computes requests/min and tokens/min over the trailing window.
func ratesLocked(minutes map[int64]*Bucket, nowMinute int64) (rpm, tpm float64) {
	var reqs, toks int64
	for i := int64(0); i < rateWindowMins; i++ {
		if b := minutes[nowMinute-i]; b != nil {
			reqs += b.Requests
			toks += b.InputTokens + b.OutputTokens
		}
	}
	return float64(reqs) / rateWindowMins, float64(toks) / rateWindowMins
}

func (s *store) providerStatLocked(id string, p *providerAgg, nowMinute int64) ProviderStat {
	rpm, tpm := ratesLocked(p.minutes, nowMinute)
	return ProviderStat{
		ProviderID:   id,
		ProviderName: p.name,
		Requests:     p.requests,
		Success:      p.success,
		Failed:       p.failed,
		Canceled:     p.canceled,
		Streamed:     p.streamed,
		SuccessRate:  p.successRate(),
		AvgLatencyMs: p.avg(),
		AvgTTFBMs:    p.avgTTFB(),
		InputTokens:  p.inputTokens,
		OutputTokens: p.outputTokens,
		CostUSD:      p.costUSD,
		RPM:          rpm,
		TPM:          tpm,
		TokensPerSec: p.tokensPerSec(),
		InFlight:     p.inFlight,
		PeakInFlight: p.peakFlight,
		FailStreak:   p.curStreak,
		MaxStreak:    p.maxStreak,
		LastOk:       p.lastOkMs,
		LastFail:     p.lastFailMs,
		LastUsed:     p.lastUsed,
		// Healthy is a live signal, not a historical one: three consecutive
		// failures with no success since marks a provider as down.
		Healthy: p.curStreak < 3,
	}
}

// ProviderFailureStreak returns a provider's current consecutive-failure count
// and the timestamp (Unix ms) of its most recent failure, plus whether the
// provider is known at all.
//
// This is the same curStreak/lastFailMs pair behind ProviderStat.FailStreak and
// ProviderStat.Healthy, exposed on its own so the forwarder can consult it per
// request without building a full ProviderStat (which computes rates and walks
// the minute buckets). Keeping one source of truth is deliberate: a provider the
// panel shows as unhealthy is exactly the one the forwarder deprioritizes — see
// proxy/forward_cooldown.go.
//
// ok is false for a provider with no recorded traffic, which callers should read
// as "no evidence against it", not "unhealthy".
func ProviderFailureStreak(providerID string) (streak int64, lastFailMs int64, ok bool) {
	if providerID == "" {
		return 0, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byProvider[providerID]
	if p == nil {
		return 0, 0, false
	}
	return p.curStreak, p.lastFailMs, true
}

// ProviderStats returns per-provider aggregates, busiest first.
func ProviderStats() []ProviderStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowMinute := time.Now().UnixMilli() / 60000
	out := make([]ProviderStat, 0, len(s.byProvider))
	for id, p := range s.byProvider {
		out = append(out, s.providerStatLocked(id, p, nowMinute))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out
}

// ProviderStatsWindow returns per-provider aggregates restricted to the last
// `hours`, busiest first. It backs the Stats tab's range filter, where the
// all-time counters of ProviderStats would make the selector look inert.
//
// Volume, outcome, token, cost and latency figures are summed from the retained
// time-series buckets. Everything else a ProviderStat carries is a live signal
// rather than a windowed total — in-flight, failure streak, last-used — and is
// copied from the provider as-is; ranking a provider "unhealthy an hour ago" is
// not what those fields mean.
//
// Sub-window granularity follows the same rule as apiGetForwardHistory: ranges
// up to bucketWindowMins are served from the per-minute buckets, longer ones
// from the hourly rollups. Per-minute buckets are memory-only, so a short window
// reads empty for a while after a restart.
//
// AvgTTFBMs, Canceled, Streamed and TokensPerSec are left zero: buckets do not
// record them, and a windowed view must not silently substitute all-time values.
func ProviderStatsWindow(hours int) []ProviderStat {
	if hours <= 0 {
		return ProviderStats()
	}
	useMinutes := hours*60 <= bucketWindowMins
	now := time.Now().UnixMilli()

	var cutoff int64
	var windowMins float64
	if useMinutes {
		cutoff = now/60000 - int64(hours*60) + 1
		windowMins = float64(hours * 60)
	} else {
		if hours > hourWindowHours {
			hours = hourWindowHours
		}
		cutoff = now/3600000 - int64(hours) + 1
		windowMins = float64(hours) * 60
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ProviderStat, 0, len(s.byProvider))
	for id, p := range s.byProvider {
		src := p.hours
		if useMinutes {
			src = p.minutes
		}
		var agg Bucket
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
		}

		st := ProviderStat{
			ProviderID:   id,
			ProviderName: p.name,
			Requests:     agg.Requests,
			Success:      agg.Success,
			Failed:       agg.Failed,
			SuccessRate:  rateOf(agg.Success, agg.Success+agg.Failed),
			InputTokens:  agg.InputTokens,
			OutputTokens: agg.OutputTokens,
			CostUSD:      agg.CostUSD,
			// Live signals, deliberately not windowed.
			InFlight:     p.inFlight,
			PeakInFlight: p.peakFlight,
			FailStreak:   p.curStreak,
			MaxStreak:    p.maxStreak,
			LastOk:       p.lastOkMs,
			LastFail:     p.lastFailMs,
			LastUsed:     p.lastUsed,
			Healthy:      p.curStreak < 3,
		}
		if agg.Requests > 0 {
			st.AvgLatencyMs = agg.SumLatencyMs / agg.Requests
		}
		if windowMins > 0 {
			// Average rate across the window, not the 5-minute live rate: mixing
			// the two would put a 30-day column next to a 5-minute one.
			st.RPM = float64(agg.Requests) / windowMins
			st.TPM = float64(agg.InputTokens+agg.OutputTokens) / windowMins
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out
}

// ProviderFailureState reports one provider's CURRENT consecutive-failure streak
// and the timestamp of the most recent failure (UnixMilli, 0 when none).
//
// This exists so a caller can implement a cooldown policy without keeping its own
// streak counter. The streak maintained by Record is already the authoritative
// one — it is what ProviderStat.Healthy is derived from and what persist.go saves
// and restores — so a second copy elsewhere would inevitably disagree with the
// number the admin panel shows.
//
// Both values are zero for an unknown provider, which reads naturally as "no
// failures on record".
//
// Note that cancellations move neither value: Record deliberately leaves the
// streak untouched for a client disconnect, so a user who habitually aborts long
// answers cannot cool down a perfectly healthy provider.
func ProviderFailureState(id string) (streak int64, lastFailMs int64) {
	if id == "" {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byProvider[id]
	if p == nil {
		return 0, 0
	}
	return p.curStreak, p.lastFailMs
}

// ModelStat is one model's traffic within a provider.
type ModelStat struct {
	Model        string  `json:"model"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	SuccessRate  float64 `json:"successRate"`
	AvgLatencyMs int64   `json:"avgLatencyMs"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
	LastUsed     int64   `json:"lastUsed"`
}

// AccountStat is one Kiro account's traffic within a provider. It is only
// populated for the Kiro pool, where Record receives an AccountID.
type AccountStat struct {
	AccountID    string  `json:"accountId"`
	AccountLabel string  `json:"accountLabel,omitempty"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	SuccessRate  float64 `json:"successRate"`
	AvgLatencyMs int64   `json:"avgLatencyMs"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	LastUsed     int64   `json:"lastUsed"`
}

// StatusStat is one HTTP status code's count within a provider.
type StatusStat struct {
	Status int   `json:"status"`
	Count  int64 `json:"count"`
}

// ProviderDetail is the full drill-down for one provider.
type ProviderDetail struct {
	ProviderStat
	Percentiles Percentiles   `json:"percentiles"`
	Models      []ModelStat   `json:"models"`
	Accounts    []AccountStat `json:"accounts"`
	Statuses    []StatusStat  `json:"statuses"`
	RecentErrs  []ErrorSample `json:"recentErrors"`
	Minutes     []Bucket      `json:"minutes"`
	// WindowHours is 0 for all-time, >0 when the headline ProviderStat numbers
	// are scoped to a time window (matching the Stats tab's range selector).
	// Models/Accounts/Statuses are always all-time because buckets do not carry
	// per-model or per-account breakdowns; live signals (InFlight etc.) are
	// always current.
	WindowHours int `json:"windowHours,omitempty"`
}

// ProviderDetailFor returns the drill-down for one provider. found is false when
// the provider has no recorded traffic.
func ProviderDetailFor(id string, minutes int) (ProviderDetail, bool) {
	if minutes <= 0 || minutes > bucketWindowMins {
		minutes = 60
	}
	s.mu.Lock()
	p := s.byProvider[id]
	if p == nil {
		s.mu.Unlock()
		return ProviderDetail{}, false
	}
	nowMinute := time.Now().UnixMilli() / 60000
	d := ProviderDetail{ProviderStat: s.providerStatLocked(id, p, nowMinute)}

	for m, c := range p.byModel {
		d.Models = append(d.Models, ModelStat{
			Model:        m,
			Requests:     c.requests,
			Success:      c.success,
			Failed:       c.failed,
			SuccessRate:  c.successRate(),
			AvgLatencyMs: c.avg(),
			InputTokens:  c.inputTokens,
			OutputTokens: c.outputTokens,
			CostUSD:      c.costUSD,
			LastUsed:     c.lastUsed,
		})
	}
	for aid, a := range p.byAccount {
		d.Accounts = append(d.Accounts, AccountStat{
			AccountID:    aid,
			AccountLabel: a.label,
			Requests:     a.requests,
			Success:      a.success,
			Failed:       a.failed,
			SuccessRate:  a.successRate(),
			AvgLatencyMs: a.avg(),
			InputTokens:  a.inputTokens,
			OutputTokens: a.outputTokens,
			LastUsed:     a.lastUsed,
		})
	}
	for code, n := range p.byStatus {
		d.Statuses = append(d.Statuses, StatusStat{Status: code, Count: n})
	}
	d.RecentErrs = append([]ErrorSample(nil), p.recentErrs...)
	// Newest error first, matching the event log's ordering.
	for i, j := 0, len(d.RecentErrs)-1; i < j; i, j = i+1, j-1 {
		d.RecentErrs[i], d.RecentErrs[j] = d.RecentErrs[j], d.RecentErrs[i]
	}
	d.Minutes = seriesLocked(p.minutes, nowMinute, minutes, 1)
	s.mu.Unlock()

	sort.Slice(d.Models, func(i, j int) bool { return d.Models[i].Requests > d.Models[j].Requests })
	sort.Slice(d.Accounts, func(i, j int) bool { return d.Accounts[i].Requests > d.Accounts[j].Requests })
	sort.Slice(d.Statuses, func(i, j int) bool { return d.Statuses[i].Count > d.Statuses[j].Count })

	d.Percentiles = percentilesFor(id)
	return d, true
}

// ProviderDetailWindow is like ProviderDetailFor but scopes the headline
// ProviderStat numbers to the given time window (same bucket logic as
// ProviderStatsWindow). Models/Accounts/Statuses remain all-time because
// buckets do not carry per-dimension breakdowns; live signals (InFlight,
// FailStreak, LastOk, Healthy) are always current.
//
// hours <= 0 falls through to ProviderDetailFor (all-time). minutes controls
// the per-minute sparkline width, same as ProviderDetailFor.
func ProviderDetailWindow(id string, hours, minutes int) (ProviderDetail, bool) {
	if hours <= 0 {
		return ProviderDetailFor(id, minutes)
	}
	if minutes <= 0 || minutes > bucketWindowMins {
		minutes = 60
	}

	useMinutes := hours*60 <= bucketWindowMins
	now := time.Now().UnixMilli()

	var cutoff int64
	var windowMins float64
	if useMinutes {
		nowMinute := now / 60000
		cutoff = nowMinute - int64(hours*60) + 1
		windowMins = float64(hours * 60)
	} else {
		if hours > hourWindowHours {
			hours = hourWindowHours
		}
		nowHour := now / 3600000
		cutoff = nowHour - int64(hours) + 1
		windowMins = float64(hours) * 60
	}

	s.mu.Lock()
	p := s.byProvider[id]
	if p == nil {
		s.mu.Unlock()
		return ProviderDetail{}, false
	}
	nowMinute := now / 60000

	// Aggregate the windowed buckets into a synthetic ProviderStat. Live
	// signals come from the live counters, not the historical buckets.
	src := p.hours
	if useMinutes {
		src = p.minutes
	}
	var agg Bucket
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
	}
	rpm, tpm := ratesLocked(p.minutes, nowMinute)
	windowed := ProviderStat{
		ProviderID:   id,
		ProviderName: p.name,
		Requests:     agg.Requests,
		Success:      agg.Success,
		Failed:       agg.Failed,
		SuccessRate:  rateOf(agg.Success, agg.Success+agg.Failed),
		InputTokens:  agg.InputTokens,
		OutputTokens: agg.OutputTokens,
		CostUSD:      agg.CostUSD,
		// Live signals.
		InFlight:     p.inFlight,
		PeakInFlight: p.peakFlight,
		FailStreak:   p.curStreak,
		MaxStreak:    p.maxStreak,
		LastOk:       p.lastOkMs,
		LastFail:     p.lastFailMs,
		LastUsed:     p.lastUsed,
		Healthy:      p.curStreak < 3,
		RPM:          rpm,
		TPM:          tpm,
	}
	if agg.Requests > 0 {
		windowed.AvgLatencyMs = agg.SumLatencyMs / agg.Requests
	}
	if windowMins > 0 {
		// Override RPM/TPM with the window average, matching ProviderStatsWindow.
		windowed.RPM = float64(agg.Requests) / windowMins
		windowed.TPM = float64(agg.InputTokens+agg.OutputTokens) / windowMins
	}

	d := ProviderDetail{ProviderStat: windowed, WindowHours: hours}

	// Models/Accounts/Statuses are all-time. The bucket maps do not carry
	// per-dimension breakdowns, and reconstructing them from the event ring
	// would be partial for windows longer than the ring covers.
	for m, c := range p.byModel {
		d.Models = append(d.Models, ModelStat{
			Model:        m,
			Requests:     c.requests,
			Success:      c.success,
			Failed:       c.failed,
			SuccessRate:  c.successRate(),
			AvgLatencyMs: c.avg(),
			InputTokens:  c.inputTokens,
			OutputTokens: c.outputTokens,
			CostUSD:      c.costUSD,
			LastUsed:     c.lastUsed,
		})
	}
	for aid, a := range p.byAccount {
		d.Accounts = append(d.Accounts, AccountStat{
			AccountID:    aid,
			AccountLabel: a.label,
			Requests:     a.requests,
			Success:      a.success,
			Failed:       a.failed,
			SuccessRate:  a.successRate(),
			AvgLatencyMs: a.avg(),
			InputTokens:  a.inputTokens,
			OutputTokens: a.outputTokens,
			LastUsed:     a.lastUsed,
		})
	}
	for code, n := range p.byStatus {
		d.Statuses = append(d.Statuses, StatusStat{Status: code, Count: n})
	}
	d.RecentErrs = append([]ErrorSample(nil), p.recentErrs...)
	for i, j := 0, len(d.RecentErrs)-1; i < j; i, j = i+1, j-1 {
		d.RecentErrs[i], d.RecentErrs[j] = d.RecentErrs[j], d.RecentErrs[i]
	}
	d.Minutes = seriesLocked(p.minutes, nowMinute, minutes, 1)
	s.mu.Unlock()

	sort.Slice(d.Models, func(i, j int) bool { return d.Models[i].Requests > d.Models[j].Requests })
	sort.Slice(d.Accounts, func(i, j int) bool { return d.Accounts[i].Requests > d.Accounts[j].Requests })
	sort.Slice(d.Statuses, func(i, j int) bool { return d.Statuses[i].Count > d.Statuses[j].Count })

	d.Percentiles = percentilesFor(id)
	return d, true
}

// seriesLocked materialises the last n buckets ending at now, oldest first, with
// gaps filled by empty buckets. step is 1 for both minute and hour maps (the key
// unit differs, not the stride). Caller holds s.mu.
func seriesLocked(m map[int64]*Bucket, now int64, n int, step int64) []Bucket {
	out := make([]Bucket, 0, n)
	for i := int64(n) - 1; i >= 0; i -= step {
		k := now - i
		if b := m[k]; b != nil {
			out = append(out, b.derive())
		} else {
			out = append(out, Bucket{Minute: k})
		}
	}
	return out
}

// MinuteHistoryFor returns the last `minutes` per-minute buckets for one
// provider, oldest first. An empty id aggregates across all providers.
//
// This exists because HistoryFor is hour-granular: asking it for one hour yields
// a single bar, which is not a trend. Short ranges therefore read the per-minute
// buckets instead.
//
// Unlike the hourly rollups, per-minute buckets are in-memory only (they are not
// written to the persisted snapshot) and are pruned after bucketWindowMins, so
// this series restarts empty after a process restart.
func MinuteHistoryFor(id string, minutes int) []Bucket {
	if minutes <= 0 || minutes > bucketWindowMins {
		minutes = 60
	}
	nowMinute := time.Now().UnixMilli() / 60000

	s.mu.Lock()
	defer s.mu.Unlock()

	if id != "" {
		p := s.byProvider[id]
		if p == nil {
			return []Bucket{}
		}
		return seriesLocked(p.minutes, nowMinute, minutes, 1)
	}
	// s.buckets is the global per-minute series, already aggregated on write, so
	// there is nothing to merge here.
	return seriesLocked(s.buckets, nowMinute, minutes, 1)
}

// HistoryFor returns the last `hours` hourly buckets for one provider, oldest
// first. An empty id returns the aggregate across all providers.
func HistoryFor(id string, hours int) []Bucket {
	if hours <= 0 || hours > hourWindowHours {
		hours = 24
	}
	nowHour := time.Now().UnixMilli() / 3600000

	s.mu.Lock()
	defer s.mu.Unlock()

	if id != "" {
		p := s.byProvider[id]
		if p == nil {
			return []Bucket{}
		}
		return seriesLocked(p.hours, nowHour, hours, 1)
	}

	// Aggregate every provider's hourly buckets into one series.
	merged := make(map[int64]*Bucket, hours)
	for _, p := range s.byProvider {
		for k, b := range p.hours {
			if k < nowHour-int64(hours) {
				continue
			}
			t := merged[k]
			if t == nil {
				t = &Bucket{Minute: k}
				merged[k] = t
			}
			t.Requests += b.Requests
			t.Success += b.Success
			t.Failed += b.Failed
			t.InputTokens += b.InputTokens
			t.OutputTokens += b.OutputTokens
			t.CostUSD += b.CostUSD
			t.SumLatencyMs += b.SumLatencyMs
		}
	}
	return seriesLocked(merged, nowHour, hours, 1)
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
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
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
			InputTokens:  r.inputTokens,
			OutputTokens: r.outputTokens,
			LastUsed:     r.lastUsed,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out
}

// EventFilter selects and paginates events for the log view. Status is
// "success", "error", "canceled", or "" (any). Model is a case-insensitive
// substring matched against both client and target model names. SinceMs/UntilMs
// bound the time range (0 = unbounded). StatusCode filters on the exact HTTP
// status when non-zero.
type EventFilter struct {
	ProviderID string
	AccountID  string
	Status     string
	StatusCode int
	Model      string
	SinceMs    int64
	UntilMs    int64
	Offset     int
	Limit      int
}

func (f EventFilter) matches(e Event, model string) bool {
	if f.ProviderID != "" && e.ProviderID != f.ProviderID {
		return false
	}
	if f.AccountID != "" && e.AccountID != f.AccountID {
		return false
	}
	switch f.Status {
	case "success":
		if !e.Ok {
			return false
		}
	case "error":
		// "canceled" is a separate filter value and a separate counter, so the
		// error view must not double-count client disconnects as upstream errors.
		if e.Ok || e.Canceled {
			return false
		}
	case "canceled":
		if !e.Canceled {
			return false
		}
	}
	if f.StatusCode != 0 && e.Status != f.StatusCode {
		return false
	}
	if f.SinceMs != 0 && e.TimeMs < f.SinceMs {
		return false
	}
	if f.UntilMs != 0 && e.TimeMs > f.UntilMs {
		return false
	}
	if model != "" &&
		!strings.Contains(strings.ToLower(e.ClientModel), model) &&
		!strings.Contains(strings.ToLower(e.TargetModel), model) {
		return false
	}
	return true
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
		if f.matches(e, model) {
			matched = append(matched, e)
		}
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
	return seriesLocked(s.buckets, nowMinute, minutes, 1)
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
func LatencyPercentiles() Percentiles { return percentilesFor("") }

// percentilesFor computes latency percentiles over successful events in the
// ring, optionally restricted to one provider. Because the ring is bounded and
// in-memory, these are recent-window figures that reset on restart.
func percentilesFor(providerID string) Percentiles {
	s.mu.Lock()
	all := s.snapshotEventsLocked()
	s.mu.Unlock()

	lat := make([]int64, 0, len(all))
	for _, e := range all {
		if !e.Ok {
			continue
		}
		if providerID != "" && e.ProviderID != providerID {
			continue
		}
		lat = append(lat, e.LatencyMs)
	}
	if len(lat) == 0 {
		return Percentiles{}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pick := func(p float64) int64 {
		idx := int(float64(len(lat)-1) * p)
		return lat[idx]
	}
	return Percentiles{P50: pick(0.50), P95: pick(0.95), P99: pick(0.99), Count: len(lat)}
}

// ProviderNames returns id -> display name for every provider with recorded
// traffic, so the UI can label filters without a separate config lookup.
func ProviderNames() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.byProvider))
	for id, p := range s.byProvider {
		out[id] = p.name
	}
	return out
}

// Reset clears all counters, events, and time-series data. Subscribers stay
// connected and will receive subsequent events. In-flight counts are preserved
// so requests still running do not underflow the gauge when they finish.
func Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	inflight := make(map[string]int64, len(s.byProvider))
	names := make(map[string]string, len(s.byProvider))
	for id, p := range s.byProvider {
		if p.inFlight > 0 {
			inflight[id] = p.inFlight
		}
		names[id] = p.name
	}

	s.ring = make([]Event, eventRingCapacity)
	s.next = 0
	s.full = false
	s.overall = counter{}
	s.byProvider = make(map[string]*providerAgg)
	s.byRoute = make(map[string]*routeAgg)
	s.byIP = make(map[string]*counter)
	s.buckets = make(map[int64]*Bucket)

	// Re-seed provider entries so names survive the reset (the filter dropdown
	// would otherwise empty until traffic resumes) and in-flight stays balanced.
	for id, name := range names {
		p := newProviderAgg()
		p.name = name
		p.inFlight = inflight[id]
		p.peakFlight = inflight[id]
		s.byProvider[id] = p
	}
}

// ResetProvider clears counters, history and retained errors for one provider,
// leaving every other provider untouched. Events already in the shared ring are
// dropped for that provider so the log view agrees with the counters.
func ResetProvider(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.byProvider[id]
	if p == nil {
		return false
	}

	// Subtract this provider's lifetime totals from the aggregate. The ring holds
	// only the most recent events, so rebuilding the overall counter from it would
	// silently discard every older request — including all persisted history.
	s.overall.sub(p.counter)

	fresh := newProviderAgg()
	fresh.name = p.name
	fresh.inFlight = p.inFlight
	fresh.peakFlight = p.inFlight
	s.byProvider[id] = fresh

	for rid, r := range s.byRoute {
		if r.providerID == id {
			delete(s.byRoute, rid)
		}
	}

	// Rebuild the ring and the per-minute series without this provider's events.
	// Both are recent-window views, so reconstructing them from the ring is exact.
	kept := s.snapshotEventsLocked()
	s.ring = make([]Event, eventRingCapacity)
	s.next = 0
	s.full = false
	s.buckets = make(map[int64]*Bucket)
	for i := len(kept) - 1; i >= 0; i-- {
		e := kept[i]
		if e.ProviderID == id {
			continue
		}
		s.ring[s.next] = e
		s.next = (s.next + 1) % eventRingCapacity
		if s.next == 0 {
			s.full = true
		}
		minute := e.TimeMs / 60000
		b := s.buckets[minute]
		if b == nil {
			b = &Bucket{Minute: minute}
			s.buckets[minute] = b
		}
		b.add(e)
	}
	return true
}
