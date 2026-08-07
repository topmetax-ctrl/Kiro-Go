package search

import (
	"context"
	"time"
)

// Request is the provider-agnostic input for one web search. The
// orchestrator builds it from a web_search tool_use plus the request's
// WebSearchPolicy and the resolved config.
type Request struct {
	Query          string
	MaxResults     int
	SearchDepth    string   // "basic" or "advanced" (Tavily); SearXNG ignores it
	IncludeAnswer  bool     // request a provider-generated answer where supported
	AllowedDomains []string // mutually exclusive with BlockedDomains (validated upstream)
	BlockedDomains []string

	// Language biases results ("auto" or an ISO code). SearXNG "language" param.
	Language string
	// SafeSearch: 0=none, 1=moderate, 2=strict. SearXNG "safesearch" param.
	SafeSearch int
	// Categories restricts SearXNG categories (e.g. ["general"]).
	Categories []string
	// TimeRange, when set ("day"/"week"/"month"/"year"), restricts result recency.
	TimeRange string
}

// Result is one normalized hit. Providers map their native shape onto this.
type Result struct {
	Title         string
	URL           string
	Content       string
	Score         float64
	PublishedDate string
	Engine        string // discovery engine/source that produced the hit (SearXNG)
}

// Response is the normalized provider output.
type Response struct {
	Query    string
	Answer   string
	Results  []Result
	Provider string
	// Credits is provider cost for this call (Tavily usage.credits); 0 for free
	// providers like SearXNG. Tracked separately from Kiro credits.
	Credits int
}

// ProviderHealthState is a coarse health signal used by the router to skip a
// provider that is known to be failing without paying its full timeout.
type ProviderHealthState int

const (
	// ProviderHealthy is the default: the provider is eligible.
	ProviderHealthy ProviderHealthState = iota
	// ProviderDegraded means recent calls were slow or partially failing; the
	// provider is still eligible but the router may prefer a fallback.
	ProviderDegraded
	// ProviderUnhealthy means recent calls failed hard; the router skips it until
	// the circuit half-opens.
	ProviderUnhealthy
)

// ProviderHealth reports a provider's current health plus the last-observed
// latency, for observability and routing decisions.
type ProviderHealth struct {
	State       ProviderHealthState
	LastLatency time.Duration
	LastError   string
}

// Provider is a web-search discovery backend. SearXNG (free, primary) and
// Tavily (optional, paid-capped fallback) implement it. Implementations must:
//   - honor ctx cancellation (NewRequestWithContext),
//   - return a *ProviderError (typed kind) on failure, never a bare error,
//   - never log the API key or Authorization header.
type Provider interface {
	Name() string
	Search(ctx context.Context, req Request) (Response, error)
	// Health reports the provider's current eligibility. A provider with no
	// health tracking returns ProviderHealthy.
	Health() ProviderHealth
}
