package proxy

import (
	"kiro-go/config"
	"kiro-go/search"
)

// forwardWebSearchStrategy is the executable strategy for a forwarded route
// that carries a web_search server tool. The resolver normalizes the
// provider's raw string once; handler branches on the typed result.
type forwardWebSearchStrategy int

const (
	forwardWebSearchUnspecified forwardWebSearchStrategy = iota // unset -> raw passthrough
	forwardWebSearchNative                                      // provider handles it
	forwardWebSearchLocal                                       // proxy rewrites + loops
	forwardWebSearchUnsupported                                 // explicit error
)

func (s forwardWebSearchStrategy) String() string {
	switch s {
	case forwardWebSearchNative:
		return "native"
	case forwardWebSearchLocal:
		return "local"
	case forwardWebSearchUnsupported:
		return "unsupported"
	default:
		return ""
	}
}

// resolveForwardWebSearchStrategy maps the first eligible non-pool target's
// provider strategy to an executable strategy. Unspecified preserves the
// pre-Phase-2 behavior (raw passthrough) and never silently becomes local.
func resolveForwardWebSearchStrategy(targets []config.ResolvedTarget) forwardWebSearchStrategy {
	for _, rt := range targets {
		if rt.Provider.ID == config.KiroPoolTargetID {
			continue
		}
		switch rt.Provider.WebSearchStrategyResolved() {
		case config.ProviderWebSearchStrategyNative:
			return forwardWebSearchNative
		case config.ProviderWebSearchStrategyLocal:
			return forwardWebSearchLocal
		case config.ProviderWebSearchStrategyUnsupported:
			return forwardWebSearchUnsupported
		default:
			return forwardWebSearchUnspecified
		}
	}
	return forwardWebSearchUnspecified
}

// forwardLocalWebSearchError classifies why a local loop cannot proceed.
type forwardLocalWebSearchError struct {
	Reason string
}

func (e *forwardLocalWebSearchError) Error() string { return e.Reason }

// checkForwardLocalPrecondition validates policy/provider readiness for a local loop.
// Returns a typed error the caller can map to a 400/500.
func checkForwardLocalPrecondition(policy WebSearchPolicy, hasWebSearch bool) error {
	if !hasWebSearch {
		return nil
	}
	if policy.Enabled {
		return nil
	}
	if !config.WebSearchToggledOn() {
		return &forwardLocalWebSearchError{Reason: "web_search is not enabled"}
	}
	if !config.SearXNGProviderEnabled() && !config.TavilyProviderEnabled() {
		return &search.ConfigError{Reason: "web_search is enabled but no search provider is configured (set a SearXNG base URL or a Tavily API key)"}
	}
	return &search.ConfigError{Reason: "web_search misconfigured for forwarding provider"}
}
