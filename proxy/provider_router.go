package proxy

import (
	"context"
	"errors"
	"time"

	"kiro-go/logger"
)

// Canonical provider name tokens, mirrored from config for use in routing and
// metrics without importing the config constants (they are unexported there).
const (
	providerSearXNG = "searxng"
	providerTavily  = "tavily"
)

// providerEntry pairs a discovery provider with the paid-usage flag that governs
// whether it may be used when routing.allowPaidUsage is false.
type providerEntry struct {
	provider SearchProvider
	// paid marks a provider that can incur cost (Tavily). When the router is in
	// free-only mode, a paid provider is used only after its budget gate allows it.
	paid bool
	// budgetAllow is an optional gate consulted before a paid call (Tavily monthly
	// budget). nil means "no budget gate" (free provider).
	budgetAllow func() bool
}

// ProviderRouter implements the free-first routing policy: try the primary
// provider, and fall back to the next provider only when the primary errors, is
// unhealthy, or returns results the quality evaluator rejects. It never falls
// back purely on an HTTP error code — the decision is quality-driven.
type ProviderRouter struct {
	entries []providerEntry
	quality SearchQualityEvaluator
	// allowPaid is the hard gate. When false, paid providers are skipped unless
	// their budgetAllow returns true (i.e. still within the free tier).
	allowPaid bool
}

// routerOutcome reports which provider produced the returned response and whether
// a fallback occurred, for observability.
type routerOutcome struct {
	Provider     string
	FallbackUsed bool
	Quality      SearchQuality
}

// newProviderRouter builds a router. entries are tried in order; the first is the
// primary. A nil evaluator defaults to a permissive one (any non-empty result set
// is acceptable), so the router still functions without quality gating.
func newProviderRouter(entries []providerEntry, quality SearchQualityEvaluator, allowPaid bool) *ProviderRouter {
	if quality == nil {
		quality = newHeuristicQualityEvaluator(1)
	}
	return &ProviderRouter{entries: entries, quality: quality, allowPaid: allowPaid}
}

// Route runs the free-first search. It returns the first acceptable response, or
// the last provider's response/error if none is acceptable. Context cancellation
// aborts immediately without trying further providers.
func (r *ProviderRouter) Route(ctx context.Context, req SearchRequest) (SearchResponse, routerOutcome, error) {
	var (
		lastErr    error
		lastResp   SearchResponse
		lastQual   SearchQuality
		lastName   string
		anyTried   bool
		firstIndex = -1
	)

	for i, e := range r.entries {
		if err := ctx.Err(); err != nil {
			return SearchResponse{}, routerOutcome{}, err
		}
		if e.provider == nil {
			continue
		}

		// Paid-usage gate: in free-only mode, a paid provider is used only while its
		// budget gate still allows a call. This is the "no accidental spend" line.
		if e.paid && !r.allowPaid {
			if e.budgetAllow != nil && !e.budgetAllow() {
				logger.Debugf("[WebSearch] router: skipping paid provider %s (budget exhausted, free-only)", e.provider.Name())
				continue
			}
		}

		// Skip a provider whose circuit is open (unhealthy), unless it is the only
		// one we have left to try.
		if e.provider.Health().State == ProviderUnhealthy && r.hasHealthyAfter(i) {
			logger.Debugf("[WebSearch] router: skipping unhealthy provider %s", e.provider.Name())
			continue
		}

		if firstIndex < 0 {
			firstIndex = i
		}
		anyTried = true

		resp, err := e.provider.Search(ctx, req)
		if err != nil {
			// Context cancellation is terminal — do not try other providers.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return SearchResponse{}, routerOutcome{}, err
			}
			lastErr = err
			lastName = e.provider.Name()
			logger.Debugf("[WebSearch] router: provider %s failed: %v; trying next", e.provider.Name(), err)
			continue
		}

		qual := r.quality.Evaluate(req.Query, resp.Results)
		lastResp, lastQual, lastName, lastErr = resp, qual, e.provider.Name(), nil
		if qual.Acceptable {
			return resp, routerOutcome{Provider: e.provider.Name(), FallbackUsed: i != firstIndex, Quality: qual}, nil
		}
		logger.Debugf("[WebSearch] router: provider %s low quality (%s); trying next", e.provider.Name(), qual.Reason)
	}

	if !anyTried {
		return SearchResponse{}, routerOutcome{}, &SearchConfigError{Reason: "no usable search provider configured"}
	}
	// Nothing was acceptable. If the last thing we saw was an error, surface it.
	// Otherwise return the last (sub-threshold) response so the model still gets
	// whatever was found — an empty or thin result set is not itself a failure.
	if lastErr != nil {
		return SearchResponse{}, routerOutcome{Provider: lastName, FallbackUsed: true}, lastErr
	}
	return lastResp, routerOutcome{Provider: lastName, FallbackUsed: firstIndex >= 0 && lastName != r.primaryName(), Quality: lastQual}, nil
}

// hasHealthyAfter reports whether any provider after index i is currently not
// unhealthy — used to decide whether we can afford to skip an unhealthy one.
func (r *ProviderRouter) hasHealthyAfter(i int) bool {
	for j := i + 1; j < len(r.entries); j++ {
		if r.entries[j].provider == nil {
			continue
		}
		if r.entries[j].provider.Health().State != ProviderUnhealthy {
			return true
		}
	}
	return false
}

func (r *ProviderRouter) primaryName() string {
	for _, e := range r.entries {
		if e.provider != nil {
			return e.provider.Name()
		}
	}
	return ""
}

// providerNames lists the configured provider names in routing order (for logs).
func (r *ProviderRouter) providerNames() []string {
	names := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		if e.provider != nil {
			names = append(names, e.provider.Name())
		}
	}
	return names
}

// recordTavilyCredits books Tavily spend against the monthly budget after a
// successful paid call. Safe to call with 0 (no-op).
func recordTavilyCredits(resp SearchResponse) {
	if resp.Provider == providerTavily && resp.Credits > 0 {
		getTavilyBudget().record(resp.Credits)
	}
}

// providerHealthByName reports the health of a named entry, for metrics.
func (r *ProviderRouter) providerHealthByName(name string) (ProviderHealth, bool) {
	for _, e := range r.entries {
		if e.provider != nil && e.provider.Name() == name {
			return e.provider.Health(), true
		}
	}
	return ProviderHealth{}, false
}

// routerCallTimeout derives a per-call deadline from a whole-request budget and
// how many providers might be tried, so one slow provider cannot eat the entire
// budget. Bounded to at least 1s.
func routerCallTimeout(total time.Duration, providers int) time.Duration {
	if providers <= 0 {
		providers = 1
	}
	d := total / time.Duration(providers)
	if d < time.Second {
		d = time.Second
	}
	return d
}
