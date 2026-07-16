package proxy

import (
	"fmt"
	"strings"
)

// KiroErrorCategory classifies an upstream Kiro failure into the action the
// account-failover logic must take. It replaces brittle substring matching on
// error strings (which false-positives on request-IDs / bodies that merely
// contain "429" or "401") with a decision made once, at the point where the
// HTTP status code is authoritative.
type KiroErrorCategory string

const (
	KiroErrUnknown        KiroErrorCategory = "unknown"
	KiroErrQuota          KiroErrorCategory = "quota"
	KiroErrOverage        KiroErrorCategory = "overage"
	KiroErrSuspension     KiroErrorCategory = "suspension"
	KiroErrAuth           KiroErrorCategory = "auth"
	KiroErrProfileUnavail KiroErrorCategory = "profile_unavailable"
)

// KiroUpstreamError carries the categorized outcome of a non-2xx upstream
// response alongside the raw material needed for logging and backward-compatible
// error strings.
//
// The Error() string intentionally preserves the legacy "HTTP %d from %s: %s"
// shape so callers still on substring matching (and existing tests) keep working
// during the migration; new callers should type-assert via errors.As and switch
// on Category instead.
type KiroUpstreamError struct {
	Category   KiroErrorCategory
	StatusCode int    // 0 when the error is not an HTTP status (network, etc.)
	Endpoint   string // ep.Name, for logs
	Body       string // raw upstream body (may be empty)
	Err        error  // wrapped cause, if any
}

func (e *KiroUpstreamError) Error() string {
	if e.Endpoint != "" {
		return fmt.Sprintf("HTTP %d from %s: %s", e.StatusCode, e.Endpoint, e.Body)
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func (e *KiroUpstreamError) Unwrap() error { return e.Err }

// categorizeUpstream decides the failure category from the authoritative HTTP
// status code, using the body only to refine 402 → overage and to detect
// suspension / profile-unavailable markers that upstream reports with a generic
// status. Body matching here is a last-resort refinement, not the primary
// signal, so digits embedded in request-IDs cannot flip the category.
func categorizeUpstream(statusCode int, body string) KiroErrorCategory {
	lower := strings.ToLower(body)
	switch statusCode {
	case 402:
		if strings.Contains(lower, "overage") {
			return KiroErrOverage
		}
		return KiroErrUnknown
	case 401, 403:
		return KiroErrAuth
	case 429:
		return KiroErrQuota
	}

	// Non-status-driven markers upstream may return with other codes.
	switch {
	case strings.Contains(lower, "temporarily_suspended"),
		strings.Contains(lower, "temporarily is suspended"),
		strings.Contains(lower, "account suspended"):
		return KiroErrSuspension
	case strings.Contains(lower, "no available kiro profile"):
		return KiroErrProfileUnavail
	}
	return KiroErrUnknown
}
