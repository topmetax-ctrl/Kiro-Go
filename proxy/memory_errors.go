package proxy

import (
	"fmt"
	"time"
)

// Typed errors for the memory sidecar path. Callers classify on these types
// (never on string matching) to decide retry and fail-open behavior. A memory
// error must NEVER fail a Kiro account or break the LLM request path.

// MemoryConfigError means the memory config is invalid (e.g. enabled without a
// base URL, unknown provider). Not retryable; surfaces as a configuration error.
type MemoryConfigError struct {
	Reason string
}

func (e *MemoryConfigError) Error() string { return "memory config error: " + e.Reason }

// MemoryProviderError wraps a failure from the memory backend (Mem0). Kind
// distinguishes transient from terminal so the provider knows whether a retry
// is worthwhile.
type MemoryProviderError struct {
	Kind MemoryProviderErrorKind
	// StatusCode is the backend HTTP status when applicable (0 otherwise).
	StatusCode int
	// RetryAfter is the backend's Retry-After hint (0 if absent).
	RetryAfter time.Duration
	Err        error
}

func (e *MemoryProviderError) Error() string {
	return fmt.Sprintf("memory provider error (%s, status=%d): %v", e.Kind, e.StatusCode, e.Err)
}
func (e *MemoryProviderError) Unwrap() error { return e.Err }

type MemoryProviderErrorKind string

const (
	MemoryErrAuth        MemoryProviderErrorKind = "auth"         // 401/403 — terminal
	MemoryErrRateLimit   MemoryProviderErrorKind = "rate_limit"   // 429 — retryable
	MemoryErrInvalid     MemoryProviderErrorKind = "invalid"      // 400 — terminal
	MemoryErrTimeout     MemoryProviderErrorKind = "timeout"      // transient — retryable
	MemoryErrUpstream5xx MemoryProviderErrorKind = "upstream_5xx" // retryable
	MemoryErrMalformed   MemoryProviderErrorKind = "malformed"    // undecodable response — terminal
)

// retryable reports whether a provider error kind is worth another attempt.
func (k MemoryProviderErrorKind) retryable() bool {
	switch k {
	case MemoryErrRateLimit, MemoryErrTimeout, MemoryErrUpstream5xx:
		return true
	default:
		return false
	}
}
