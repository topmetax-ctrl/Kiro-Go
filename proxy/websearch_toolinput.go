package proxy

import "strings"

// This file holds the tool-input helpers used by the fork's own web-search
// stack (search package + webSearchExecutor + kiroConversationRunner), which
// executes searches through SearXNG / Tavily rather than through Kiro's MCP
// endpoint. The MCP-based stack lives in websearch.go and websearch_loop.go and
// reads its query straight off the incoming ClaudeRequest instead, so the two
// stacks need different accessors and are deliberately kept apart.

// extractToolInputQuery pulls the query string out of a web_search tool_use
// input. Kiro emits {"query": "..."}; a couple of common aliases are accepted
// defensively since the model occasionally picks a different field name.
//
// Named for the input shape rather than the operation because websearch.go has
// its own extractSearchQuery that takes a *ClaudeRequest.
func extractToolInputQuery(input map[string]interface{}) string {
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
