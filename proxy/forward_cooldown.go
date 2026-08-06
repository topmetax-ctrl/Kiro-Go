package proxy

// Cooldown for forward targets: trying a provider that is currently failing LAST
// instead of spending a round-trip proving it is still down.
//
// Note this demotes rather than excludes — see applyForwardCooldown for why a
// route must never lose a target outright.
//
// WHY THIS HOLDS NO STATE OF ITS OWN
//
// The obvious implementation is a healthTracker per provider, as memory_health.go
// and search/provider_health.go each do for their own subsystem. That would be
// wrong here, because metrics already maintains the authoritative per-provider
// failure streak: metrics.Record updates curStreak/lastFailMs on every forwarded
// outcome, persist.go round-trips both across restarts, and ProviderStat.Healthy
// (curStreak < 3) is what the admin panel already renders.
//
// A second counter would be a copy of that state updated on a different code
// path, so the two would drift: the panel could show a green provider while the
// router skipped it, or the reverse. Instead this file holds only POLICY —
// thresholds and the ordering decision — and reads the streak from metrics. There
// is one source of truth for "is this provider failing", and the operator sees
// the same number the router acts on.
//
// The breaker is therefore implicit rather than a state machine:
//
//	closed    = streak < forwardCooldownThreshold
//	open      = streak >= threshold AND last failure within forwardCooldownWindow
//	half-open = streak >= threshold AND window elapsed (the target is tried again;
//	            a success zeroes the streak in metrics, a failure re-arms it)
//
// Half-open falls out of measuring "time since last failure" rather than "time
// since tripped": once the window passes with no new failure the target is
// eligible again, and the very next outcome either clears or re-arms it. No
// timer, no goroutine, no reset bookkeeping.

import (
	"time"

	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/metrics"
)

const (
	// forwardCooldownThreshold is the consecutive-failure count at which a target
	// starts being deprioritized. It matches metrics.ProviderStat.Healthy
	// (curStreak < 3) on purpose: the panel's health badge and the router's
	// decision must mean the same thing. It is also the value both existing
	// breakers in this repo use — newHealthTracker(3, 30s) in memory_health.go and
	// search/{searxng,tavily}_provider.go.
	forwardCooldownThreshold = 3

	// forwardCooldownWindow is how long after the most recent failure a tripped
	// target stays deprioritized. 30s matches the two existing breakers.
	// Deliberately short: the cost of being wrong is one wasted round-trip on a
	// recovered provider, while too long strands traffic on backups after the
	// primary is healthy again.
	forwardCooldownWindow = 30 * time.Second
)

// forwardTargetInCooldown reports whether a provider is currently tripped.
//
// Returns false for a provider with no recorded traffic, so a freshly configured
// target is always tried.
func forwardTargetInCooldown(providerID string, now time.Time) bool {
	streak, lastFailMs, ok := metrics.ProviderFailureStreak(providerID)
	if !ok || streak < forwardCooldownThreshold {
		return false
	}
	if lastFailMs <= 0 {
		// A streak with no failure timestamp should not happen, but treating it as
		// "not in cooldown" fails open: try the target rather than deprioritize it
		// on the strength of an incomplete record.
		return false
	}
	return now.Sub(time.UnixMilli(lastFailMs)) < forwardCooldownWindow
}

// applyForwardCooldown reorders resolved targets in place so tripped ones are
// tried last, preserving relative order within the healthy and tripped groups.
// It returns how many targets were demoted.
//
// Demotion rather than removal is the important part, and it mirrors the search
// router's "skip unless it is the only option" rule (hasHealthyAfter in
// search/provider_router.go). If every target for a route is tripped — a total
// outage, or several routes sharing one upstream — dropping them would turn a
// forwarded model into an unrouted one, which falls through to the Kiro pool and
// answers from an entirely different backend. Demoting keeps the route intact:
// the request still tries every target, just in a better order.
//
// Note this can reorder ACROSS priority tiers, which is the point: a tripped
// primary should lose to a healthy fallback. Operator priority still decides
// among targets of equal health.
func applyForwardCooldown(targets []config.ResolvedTarget, now time.Time) int {
	if len(targets) < 2 {
		// A single target is always tried: there is nothing to prefer over it, and
		// deprioritizing it would change nothing.
		return 0
	}

	healthy := make([]config.ResolvedTarget, 0, len(targets))
	tripped := make([]config.ResolvedTarget, 0, len(targets))
	for _, t := range targets {
		if forwardTargetInCooldown(t.Provider.ID, now) {
			tripped = append(tripped, t)
			continue
		}
		healthy = append(healthy, t)
	}
	if len(tripped) == 0 || len(healthy) == 0 {
		// Nothing tripped, or everything is — in which case the configured order
		// (operator priority) is as good a guess as any.
		return 0
	}

	copy(targets, healthy)
	copy(targets[len(healthy):], tripped)
	return len(tripped)
}

// logForwardCooldown reports a demotion once per request, naming the target that
// will actually be tried first so the operator can see the router working.
func logForwardCooldown(model string, demoted int, firstProvider string) {
	if demoted <= 0 {
		return
	}
	logger.Infof("[Forward] %s: %d target(s) in cooldown, preferring %s", model, demoted, firstProvider)
}
