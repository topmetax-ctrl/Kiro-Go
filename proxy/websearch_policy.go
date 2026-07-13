package proxy

import (
	"strings"

	"kiro-go/config"
	"kiro-go/search"
)

// WebSearchPolicy carries the effective, validated settings for one logical
// request's web_search execution. It merges the Anthropic native tool metadata
// (max_uses, domain filters, user_location) with the server-side config caps.
//
// Native metadata is parsed here and NEVER forwarded to Kiro in the tool spec:
// Kiro does not understand it, and the search itself runs proxy-side.
type WebSearchPolicy struct {
	// Enabled reflects config.WebSearchEnabled() at request build time: the
	// feature toggle AND a non-empty provider key. When false the runner is
	// bypassed and any web_search tool_use passes through unchanged.
	Enabled bool

	// MaxSearches bounds total provider calls for the whole logical request.
	// Derived from min(request max_uses, config MaxSearches) with 0 meaning
	// "use the config cap".
	MaxSearches int

	// MaxRounds bounds Kiro reasoning round-trips. Distinct from MaxSearches:
	// one round may issue several web_search calls.
	MaxRounds int

	// MaxResults caps results fed back per query.
	MaxResults int

	// MaxConcurrentSearches bounds parallel provider calls within one round. 0 or
	// 1 means sequential.
	MaxConcurrentSearches int

	// AllowedDomains / BlockedDomains are mutually exclusive (validated). Empty
	// slices mean "no domain restriction".
	AllowedDomains []string
	BlockedDomains []string

	// UserLocation is optional geo-biasing passed through to providers that
	// support it. Tavily currently ignores it; kept for forward compatibility.
	UserLocation *WebSearchUserLocation
}

// isWebSearchTool reports whether a Claude tool spec refers to the web_search
// server tool. Anthropic sends either a bare name "web_search" (client-style)
// or a versioned type like "web_search_20250305"; recognize both.
func isWebSearchTool(tool ClaudeTool) bool {
	if strings.EqualFold(strings.TrimSpace(tool.Name), "web_search") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(tool.Type), "web_search_")
}

// webSearchQuerySchema is the explicit input schema injected for the web_search
// tool before it is forwarded to Kiro. A runtime probe showed Kiro fills the
// query even with an empty {"type":"object"}, but production must not depend on
// that implicit behavior — we state the contract.
func webSearchQuerySchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The web search query",
			},
		},
		"required": []interface{}{"query"},
	}
}

// extractWebSearchPolicy scans the request tools for a web_search server tool
// and builds the effective policy. ok is false when no web_search tool is
// present (the runner should not engage). err is non-nil only for an invalid
// native spec (e.g. both allowed and blocked domains) so the handler can reject
// with a clear message instead of silently mis-searching.
func extractWebSearchPolicy(tools []ClaudeTool) (policy WebSearchPolicy, ok bool, err error) {
	var found *ClaudeTool
	for i := range tools {
		if isWebSearchTool(tools[i]) {
			found = &tools[i]
			break
		}
	}
	if found == nil {
		return WebSearchPolicy{}, false, nil
	}

	cfg := config.GetWebSearchConfig()
	policy = WebSearchPolicy{
		Enabled:               config.WebSearchEnabled(),
		MaxSearches:           cfg.Limits.MaxSearchesPerRequest,
		MaxRounds:             cfg.Limits.MaxRounds,
		MaxResults:            cfg.Limits.MaxResultsPerSearch,
		MaxConcurrentSearches: cfg.Limits.MaxConcurrentSearches,
	}

	allowed := normalizeDomains(found.AllowedDomains)
	blocked := normalizeDomains(found.BlockedDomains)
	if len(allowed) > 0 && len(blocked) > 0 {
		return WebSearchPolicy{}, true, &search.ConfigError{
			Reason: "web_search tool specifies both allowed_domains and blocked_domains; only one is permitted",
		}
	}
	policy.AllowedDomains = allowed
	policy.BlockedDomains = blocked
	policy.UserLocation = found.UserLocation

	// Effective search budget: request max_uses (if positive) capped by config.
	if found.MaxUses != nil {
		mu := *found.MaxUses
		if mu < 0 {
			return WebSearchPolicy{}, true, &search.ConfigError{
				Reason: "web_search max_uses must be >= 0",
			}
		}
		if mu > 0 && mu < policy.MaxSearches {
			policy.MaxSearches = mu
		}
	}

	return policy, true, nil
}

// normalizeDomains trims, lowercases, strips any scheme, drops empties, and
// deduplicates a domain filter list. Bare host (optionally with path) only —
// the native contract does not accept scheme-qualified entries.
func normalizeDomains(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.TrimSpace(strings.ToLower(d))
		if d == "" {
			continue
		}
		if i := strings.Index(d, "://"); i >= 0 {
			d = d[i+3:]
		}
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
