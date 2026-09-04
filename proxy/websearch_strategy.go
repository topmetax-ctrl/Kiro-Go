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

// resolveForwardWebSearchStrategy maps the route's provider strategies to one
// executable strategy for the handler's forwarding fork.
//
// The PRIMARY (first non-pool target) governs: its strategy is what decides
// raw-passthrough vs local loop vs explicit error. The one exception is an
// unsupported primary with a capable (native/local) backup — that route can
// still serve, so we adopt the backup's strategy instead of erroring out
// (tryForwardUpstream then skips the unsupported primary per-target).
//
// Unspecified preserves the pre-Phase-2 behavior (raw passthrough) and never
// silently becomes local.
func resolveForwardWebSearchStrategy(targets []config.ResolvedTarget) forwardWebSearchStrategy {
	var first forwardWebSearchStrategy = forwardWebSearchUnspecified
	var backup forwardWebSearchStrategy = forwardWebSearchUnspecified
	for _, rt := range targets {
		if rt.Provider.ID == config.KiroPoolTargetID {
			continue
		}
		st := forwardWebSearchUnspecified
		switch rt.Provider.WebSearchStrategyResolved() {
		case config.ProviderWebSearchStrategyNative:
			st = forwardWebSearchNative
		case config.ProviderWebSearchStrategyLocal:
			st = forwardWebSearchLocal
		case config.ProviderWebSearchStrategyUnsupported:
			st = forwardWebSearchUnsupported
		}
		if first == forwardWebSearchUnspecified {
			first = st
			if st == forwardWebSearchNative || st == forwardWebSearchLocal {
				return st // primary is capable; it governs the route
			}
			continue
		}
		// Primary was unsupported: remember the first capable backup so the route
		// is not rejected outright when a later target can serve.
		if first == forwardWebSearchUnsupported && backup == forwardWebSearchUnspecified &&
			(st == forwardWebSearchNative || st == forwardWebSearchLocal) {
			backup = st
		}
	}
	if first == forwardWebSearchUnsupported && backup != forwardWebSearchUnspecified {
		return backup
	}
	return first
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
