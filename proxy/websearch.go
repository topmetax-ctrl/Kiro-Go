package proxy

import "strings"

// extractSearchQuery pulls the query string out of a web_search tool_use input.
// Kiro emits {"query": "..."}; we accept a couple of common aliases defensively
// since the model occasionally picks a different field name.
func extractSearchQuery(input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	for _, key := range []string{"query", "q", "search_query", "searchQuery"} {
		if v, ok := input[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// isWebSearchToolName reports whether a tool name — as it appears in a Kiro
// tool_use event, either the sanitized "webSearch" or the original "web_search"
// — refers to the web search tool the proxy executes itself.
func isWebSearchToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web_search", "websearch":
		return true
	}
	return false
}
