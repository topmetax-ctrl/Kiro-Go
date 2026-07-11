package proxy

import (
	"fmt"
	"time"
)

// Typed errors for the web_search execution path. The handler classifies on
// these types (never on string matching) to decide retry, account failure, and
// the client-facing status code.

// SearchConfigError means the request or server config is invalid (e.g. both
// domain filters set, feature enabled without a key). Not retryable; surfaces
// as a 400/500 configuration error. Never fails a Kiro account.
type SearchConfigError struct {
	Reason string
}

func (e *SearchConfigError) Error() string { return "web_search config error: " + e.Reason }

// SearchProviderError wraps a failure from the search provider (Tavily). Kind
// distinguishes transient from terminal so the runner knows whether a retry was
// already attempted. A provider error must NOT mark the Kiro account as failed.
type SearchProviderError struct {
	Kind SearchProviderErrorKind
	// StatusCode is the provider HTTP status when applicable (0 otherwise).
	StatusCode int
	// RetryAfter is the provider's Retry-After hint (0 if absent). Honored by the
	// provider's backoff on a retryable error.
	RetryAfter time.Duration
	Err        error
}

func (e *SearchProviderError) Error() string {
	return fmt.Sprintf("web_search provider error (%s, status=%d): %v", e.Kind, e.StatusCode, e.Err)
}
func (e *SearchProviderError) Unwrap() error { return e.Err }

type SearchProviderErrorKind string

const (
	SearchErrAuth        SearchProviderErrorKind = "auth"         // 401/403 — terminal, do not retry
	SearchErrRateLimit   SearchProviderErrorKind = "rate_limit"   // 429 — retryable with backoff
	SearchErrInvalid     SearchProviderErrorKind = "invalid"      // 400 — terminal
	SearchErrTimeout     SearchProviderErrorKind = "timeout"      // transient — retryable
	SearchErrUpstream5xx SearchProviderErrorKind = "upstream_5xx" // retryable
	SearchErrMalformed   SearchProviderErrorKind = "malformed"    // undecodable response — terminal
)

// retryable reports whether a provider error kind is worth another attempt.
func (k SearchProviderErrorKind) retryable() bool {
	switch k {
	case SearchErrRateLimit, SearchErrTimeout, SearchErrUpstream5xx:
		return true
	default:
		return false
	}
}

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
