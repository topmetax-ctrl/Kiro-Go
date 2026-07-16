package proxy

import (
	"fmt"
)

// Typed errors for the web_search runner tier (tool-loop). The handler
// classifies on these types (never on string matching) to decide retry and the
// client-facing status code. The search-core errors (SearchConfigError,
// SearchProviderError) live in search_errors.go.

// MixedToolUseError means a single Kiro round emitted both an internal
// web_search call and an external client tool call. The runner refuses to
// advance (executing half and dropping the rest would corrupt the turn); the
// handler surfaces a clear Claude-compatible error. Documented limitation.
type MixedToolUseError struct {
	InternalNames []string
	ExternalNames []string
}

func (e *MixedToolUseError) Error() string {
	return fmt.Sprintf("web_search cannot be combined with client tools in one turn (internal=%v external=%v)", e.InternalNames, e.ExternalNames)
}

// LoopLimitError means the runner hit maxRounds or maxSearches before the model
// produced a final answer. The handler still returns the best available text
// from the finalization round; this type is informational for logging.
type LoopLimitError struct {
	Reason string
}

func (e *LoopLimitError) Error() string { return "web_search loop limit: " + e.Reason }
