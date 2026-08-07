package search

import (
	"sync"
	"time"
)

// healthTracker is a small, concurrency-safe circuit breaker shared by search
// providers. It trips to unhealthy after a run of consecutive failures and
// half-opens after a cooldown so a recovered provider becomes eligible again.
//
// It is deliberately simple: the router treats Unhealthy as "skip unless it is
// the only option", never as a hard permanent exclusion.
type healthTracker struct {
	mu sync.Mutex

	consecutiveFailures int
	failureThreshold    int           // trip to unhealthy at this many consecutive failures
	cooldown            time.Duration // how long to stay tripped before half-opening
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

// health returns the current coarse state. A tripped circuit half-opens (reports
// Degraded, not Unhealthy) once the cooldown has elapsed, so the router will try
// it again; a subsequent success closes it, a failure re-trips it.
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
