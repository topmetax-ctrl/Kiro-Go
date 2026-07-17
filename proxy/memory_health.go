package proxy

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProviderHealthState is a coarse health signal for the memory sidecar's
// circuit breaker. It mirrors the search package's own health enum, but is kept
// local: memory is not a search provider, and the search breaker is unexported
// there. Each subsystem owns its own breaker (see search/provider_health.go).
type ProviderHealthState int

const (
	// ProviderHealthy is the default: recent calls succeeded.
	ProviderHealthy ProviderHealthState = iota
	// ProviderDegraded means recent calls were slow or partially failing.
	ProviderDegraded
	// ProviderUnhealthy means recent calls failed hard; the breaker is tripped
	// until the cooldown elapses.
	ProviderUnhealthy
)

// ProviderHealth reports the memory provider's current health plus the
// last-observed latency and error, for the fail-open decorator and observability.
type ProviderHealth struct {
	State       ProviderHealthState
	LastLatency time.Duration
	LastError   string
}

// healthTracker is a small, concurrency-safe circuit breaker for the memory
// sidecar. It trips to unhealthy after a run of consecutive failures and
// half-opens after a cooldown so a recovered backend becomes eligible again.
type healthTracker struct {
	mu sync.Mutex

	consecutiveFailures int
	failureThreshold    int
	cooldown            time.Duration
	trippedAt           time.Time

	lastLatency time.Duration
	lastError   string
}

func newHealthTracker(failureThreshold int, cooldown time.Duration) *healthTracker {
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &healthTracker{failureThreshold: failureThreshold, cooldown: cooldown}
}

// observeSuccess records a healthy call and closes the circuit.
func (h *healthTracker) observeSuccess(latency time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.consecutiveFailures = 0
	h.trippedAt = time.Time{}
	h.lastLatency = latency
	h.lastError = ""
}

// observeFailure records a failed call and trips the circuit at the threshold.
func (h *healthTracker) observeFailure(latency time.Duration, errMsg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.consecutiveFailures++
	h.lastLatency = latency
	h.lastError = errMsg
	if h.consecutiveFailures >= h.failureThreshold && h.trippedAt.IsZero() {
		h.trippedAt = time.Now()
	}
}

// health returns the current coarse state. A tripped circuit half-opens
// (reports Degraded, not Unhealthy) once the cooldown has elapsed.
func (h *healthTracker) health() ProviderHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	state := ProviderHealthy
	if !h.trippedAt.IsZero() {
		if time.Since(h.trippedAt) >= h.cooldown {
			state = ProviderDegraded // half-open
		} else {
			state = ProviderUnhealthy
		}
	} else if h.consecutiveFailures > 0 {
		state = ProviderDegraded
	}
	return ProviderHealth{State: state, LastLatency: h.lastLatency, LastError: h.lastError}
}

// parseRetryAfter parses a Retry-After header value expressed in seconds.
func parseRetryAfter(h string) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	return 0, false
}
