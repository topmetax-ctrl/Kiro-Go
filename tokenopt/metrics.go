package tokenopt

import (
	"sort"
	"sync"
	"time"
)

// Engine names used as metrics labels. The label sets (engine x outcome x
// reason) are small and closed, so cardinality cannot grow with traffic.
const (
	EngineHeadroom = "headroom"
	EngineCaveman  = "caveman"
	EnginePonytail = "ponytail"
)

// Outcomes.
const (
	OutcomeApplied  = "applied"  // the request sent upstream was changed
	OutcomeSkipped  = "skipped"  // selected, but nothing was changed (see reason)
	OutcomeError    = "error"    // the engine failed; the request went out unchanged
	OutcomeObserved = "observed" // observe mode measured a would-be saving
)

// Reasons for skipped (and kinds for error, which reuse ErrKind values).
const (
	ReasonNoCandidates   = "no_candidates" // no tool result large enough / not protected
	ReasonNoGain         = "no_gain"       // the engine's output was not usable or not smaller
	ReasonCircuitOpen    = "circuit_open"  // engine failing; calls suspended
	ReasonUnconfigured   = "unconfigured"  // selected but no usable URL
	ReasonUnparseable    = "unparseable"   // body shape not understood; sent as-is
	ReasonTooLarge       = "payload_too_large"
	ReasonCanceled       = "canceled"
	ReasonObserveBusy    = "observe_busy"    // observe sampling is bounded; this request was not sampled
	ReasonAlreadyPresent = "already_present" // the client already carries this prompt
	ReasonAssetIntegrity = "asset_integrity" // vendored asset failed verification
	ReasonInjectFailed   = "inject_failed"
	ReasonFromMemo       = "from_memo" // applied entirely from remembered results; no engine call
)

// Event is one recent optimization decision, for the admin view.
type Event struct {
	Time     time.Time `json:"time"`
	Protocol string    `json:"protocol"`
	Engine   string    `json:"engine"`
	Outcome  string    `json:"outcome"`
	Reason   string    `json:"reason,omitempty"`
	// Candidates: tool results eligible for compression. Replaced: how many
	// were actually rewritten. FromMemo: how many of those were replayed.
	Candidates int `json:"candidates,omitempty"`
	Replaced   int `json:"replaced,omitempty"`
	FromMemo   int `json:"fromMemo,omitempty"`
	// BytesBefore/After cover only the replaced tool-result text, measured by
	// Kiro-Go itself on the bytes it handled.
	BytesBefore int `json:"bytesBefore,omitempty"`
	BytesAfter  int `json:"bytesAfter,omitempty"`
	// EstTokensSaved is Kiro-Go's own heuristic over that text; EngineTokensSaved
	// is what the engine reported with its own tokenizer. Neither is provider
	// billing; both are labelled so they are never mistaken for it.
	EstTokensSaved    int   `json:"estTokensSaved,omitempty"`
	EngineTokensSaved int   `json:"engineTokensSaved,omitempty"`
	DurationMs        int64 `json:"durationMs,omitempty"`
}

// Counter is one (engine, outcome, reason) tally.
type Counter struct {
	Engine  string `json:"engine"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	Count   int64  `json:"count"`
}

// MetricsSnapshot is the point-in-time view served to the admin API.
type MetricsSnapshot struct {
	Counters []Counter `json:"counters"`
	// Totals cover Headroom's replaced tool-result text only.
	Replaced          int64 `json:"replaced"`
	BytesBefore       int64 `json:"bytesBefore"`
	BytesAfter        int64 `json:"bytesAfter"`
	EstTokensSaved    int64 `json:"estTokensSaved"`
	EngineTokensSaved int64 `json:"engineTokensSaved"`
	// ObservedBytesSaved is what observe mode estimated it WOULD have saved.
	ObservedBytesSaved int64 `json:"observedBytesSaved"`
	// PhantomSavings counts replies that claimed token savings while no usable
	// text came back, i.e. savings that would not have reduced the request.
	PhantomSavings int64   `json:"phantomSavings"`
	AvgDurationMs  float64 `json:"avgDurationMs"`
	MaxDurationMs  int64   `json:"maxDurationMs"`
	Calls          int64   `json:"calls"`
	Recent         []Event `json:"recent"`
	// Provenance states what the figures are, so a reader cannot mistake them
	// for provider-reported usage.
	Provenance string `json:"provenance"`
}

const recentEvents = 50

// Metrics accumulates in-memory counters. It is deliberately not persisted:
// the existing forward-stats file has its own migration history, and
// optimization figures are diagnostics, not accounting.
type Metrics struct {
	mu     sync.Mutex
	counts map[[3]string]int64

	replaced, bytesBefore, bytesAfter int64
	estTokens, engineTokens           int64
	observedBytes, phantom            int64
	durNs, calls, durMaxNs            int64

	ring [recentEvents]Event
	head int
	n    int
}

func newMetrics() *Metrics { return &Metrics{counts: map[[3]string]int64{}} }

func (m *Metrics) count(engine, outcome, reason string) {
	m.mu.Lock()
	m.counts[[3]string{engine, outcome, reason}]++
	m.mu.Unlock()
}

// record tallies an engine decision and keeps it in the recent ring.
func (m *Metrics) record(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[[3]string{ev.Engine, ev.Outcome, ev.Reason}]++
	switch ev.Outcome {
	case OutcomeApplied:
		m.replaced += int64(ev.Replaced)
		m.bytesBefore += int64(ev.BytesBefore)
		m.bytesAfter += int64(ev.BytesAfter)
		m.estTokens += int64(ev.EstTokensSaved)
		m.engineTokens += int64(ev.EngineTokensSaved)
	case OutcomeObserved:
		m.observedBytes += int64(ev.BytesBefore - ev.BytesAfter)
	}
	if ev.DurationMs > 0 {
		ns := ev.DurationMs * int64(time.Millisecond)
		m.durNs += ns
		m.calls++
		if ns > m.durMaxNs {
			m.durMaxNs = ns
		}
	}
	m.ring[m.head] = ev
	m.head = (m.head + 1) % recentEvents
	if m.n < recentEvents {
		m.n++
	}
}

func (m *Metrics) addPhantom() {
	m.mu.Lock()
	m.phantom++
	m.mu.Unlock()
}

func (m *Metrics) snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := MetricsSnapshot{
		Replaced: m.replaced, BytesBefore: m.bytesBefore, BytesAfter: m.bytesAfter,
		EstTokensSaved: m.estTokens, EngineTokensSaved: m.engineTokens,
		ObservedBytesSaved: m.observedBytes, PhantomSavings: m.phantom,
		MaxDurationMs: m.durMaxNs / int64(time.Millisecond), Calls: m.calls,
		Counters: []Counter{}, Recent: []Event{},
		Provenance: "Byte figures are measured by Kiro-Go on the tool-result text it replaced. " +
			"Token figures are estimates (a local heuristic and the engine's own tokenizer) and are " +
			"not provider-reported usage; provider usage is unchanged and appears in the usual stats.",
	}
	if m.calls > 0 {
		s.AvgDurationMs = float64(m.durNs) / float64(m.calls) / float64(time.Millisecond)
	}
	for k, v := range m.counts {
		s.Counters = append(s.Counters, Counter{Engine: k[0], Outcome: k[1], Reason: k[2], Count: v})
	}
	sort.Slice(s.Counters, func(i, j int) bool {
		a, b := s.Counters[i], s.Counters[j]
		if a.Engine != b.Engine {
			return a.Engine < b.Engine
		}
		if a.Outcome != b.Outcome {
			return a.Outcome < b.Outcome
		}
		return a.Reason < b.Reason
	})
	// Newest first.
	for i := 0; i < m.n; i++ {
		s.Recent = append(s.Recent, m.ring[(m.head-1-i+recentEvents*2)%recentEvents])
	}
	return s
}
