package proxy

import (
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// newSearchOrchestratorFromConfig builds the production SearchOrchestrator from
// the resolved web-search config. It is free-first: SearXNG is wired as the
// primary provider (no API key needed) and Tavily is appended only when it is
// enabled AND a key is resolvable. Provider order follows routing.primaryProvider
// then routing.fallbackProviders, filtered to those actually usable.
//
// The paid-usage gate is enforced here: Tavily is marked paid and given the
// monthly-budget gate, so in free-only mode the router stops using it once the
// budget is spent. It returns nil when no provider is usable, so the caller can
// treat web-search as unconfigured rather than build a router that always fails.
func newSearchOrchestratorFromConfig() SearchOrchestrator {
	ws := config.GetWebSearchConfig()

	// Build the pool of usable providers keyed by name.
	pool := map[string]providerEntry{}

	if config.SearXNGProviderEnabled() {
		if p, err := NewSearXNGProvider(ws.SearXNG.BaseURL); err == nil {
			pool[providerSearXNG] = providerEntry{provider: p, paid: false}
		} else {
			logger.Warnf("[WebSearch] searxng disabled: %v", err)
		}
	}

	if config.TavilyProviderEnabled() {
		budget := getTavilyBudget()
		pool[providerTavily] = providerEntry{
			provider:    NewTavilyProvider(""),
			paid:        true,
			budgetAllow: budget.allow,
		}
	}

	if len(pool) == 0 {
		return nil
	}

	// Order: primary first, then declared fallbacks, then any leftover usable
	// provider not explicitly listed (so a configured-but-unlisted provider is not
	// silently dropped).
	ordered := make([]providerEntry, 0, len(pool))
	used := map[string]bool{}
	appendProvider := func(name string) {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || used[name] {
			return
		}
		if e, ok := pool[name]; ok {
			ordered = append(ordered, e)
			used[name] = true
		}
	}
	appendProvider(ws.Routing.PrimaryProvider)
	for _, fb := range ws.Routing.FallbackProviders {
		appendProvider(fb)
	}
	for name := range pool {
		appendProvider(name)
	}

	quality := newHeuristicQualityEvaluator(ws.SearXNG.MinimumResults)
	router := newProviderRouter(ordered, quality, ws.Routing.AllowPaidUsage)

	var cache SearchCache
	if ws.Cache.Enabled != nil && *ws.Cache.Enabled {
		cache = newLRUSearchCache(ws.Cache.MaxEntries)
	} else {
		cache = noopSearchCache{}
	}

	reranker := newHeuristicReranker()

	// Per-call timeout: split the total request budget across the providers we
	// might try, so one slow provider cannot eat the whole budget.
	total := time.Duration(ws.Limits.TotalTimeoutSeconds) * time.Second
	perCall := routerCallTimeout(total, len(ordered))
	// A single SearXNG call also has its own configured timeout; use the smaller.
	if sx := time.Duration(ws.SearXNG.TimeoutSeconds) * time.Second; sx > 0 && sx < perCall {
		perCall = sx
	}

	cacheTTL := time.Duration(ws.Cache.TTLSeconds) * time.Second

	logger.Infof("[WebSearch] orchestrator ready: providers=%v allowPaid=%v cache=%v",
		router.providerNames(), ws.Routing.AllowPaidUsage, ws.Cache.Enabled != nil && *ws.Cache.Enabled)

	return newSearchOrchestrator(router, cache, reranker, perCall, ws.Reranking.MaxFinalResults, ws.Routing.Mode, cacheTTL)
}
