package search

import (
	"fmt"
	"time"
)

// Typed errors for the search core, shared by the providers, router, and
// orchestrator. The runner and handler classify on these types (never on string
// matching) to decide retry, account failure, and the client-facing status code.

// ConfigError means the request or server config is invalid (e.g. both
// domain filters set, feature enabled without a key). Not retryable; surfaces
// as a 400/500 configuration error. Never fails a Kiro account.
type ConfigError struct {
	Reason string
}

func (e *ConfigError) Error() string { return "web_search config error: " + e.Reason }

// ProviderError wraps a failure from the search provider (Tavily). Kind
// distinguishes transient from terminal so the runner knows whether a retry was
// already attempted. A provider error must NOT mark the Kiro account as failed.
type ProviderError struct {
	Kind ProviderErrorKind
	// StatusCode is the provider HTTP status when applicable (0 otherwise).
	StatusCode int
	// RetryAfter is the provider's Retry-After hint (0 if absent). Honored by the
	// provider's backoff on a retryable error.
	RetryAfter time.Duration
	Err        error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("web_search provider error (%s, status=%d): %v", e.Kind, e.StatusCode, e.Err)
}
func (e *ProviderError) Unwrap() error { return e.Err }

type ProviderErrorKind string

const (
	ErrAuth        ProviderErrorKind = "auth"         // 401/403 — terminal, do not retry
	ErrRateLimit   ProviderErrorKind = "rate_limit"   // 429 — retryable with backoff
	ErrInvalid     ProviderErrorKind = "invalid"      // 400 — terminal
	ErrTimeout     ProviderErrorKind = "timeout"      // transient — retryable
	ErrUpstream5xx ProviderErrorKind = "upstream_5xx" // retryable
	ErrMalformed   ProviderErrorKind = "malformed"    // undecodable response — terminal
)

// retryable reports whether a provider error kind is worth another attempt.
func (k ProviderErrorKind) retryable() bool {
	switch k {
	case ErrRateLimit, ErrTimeout, ErrUpstream5xx:
		return true
	default:
		return false
	}
}
