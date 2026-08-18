package apikey

import (
	"errors"
	"net/http"
)

// Sentinel errors for repository / service control flow.
var (
	ErrNotFound     = errors.New("api key not found")
	ErrDuplicate    = errors.New("api key already exists")
	ErrEmptySecret  = errors.New("api key value must not be empty")
	ErrInvalidInput = errors.New("invalid input")
	ErrSession      = errors.New("portal session expired")
	ErrPortalToken  = errors.New("invalid portal token")
	ErrUnavailable  = errors.New("api key store unavailable")
)

// AuthError is returned by Authenticate / OpenSession. Status and Code match
// the existing /v1 Claude and OpenAI envelopes.
type AuthError struct {
	Status  int
	Code    string
	Machine string
	Message string
}

func (e *AuthError) Error() string { return e.Message }

func errInvalidKey() *AuthError {
	return &AuthError{Status: http.StatusUnauthorized, Code: "authentication_error", Machine: "invalid_api_key", Message: "Invalid or missing API key"}
}

func errDisabled() *AuthError {
	return &AuthError{Status: http.StatusUnauthorized, Code: "authentication_error", Machine: "api_key_disabled", Message: "API key disabled"}
}

func errExpired() *AuthError {
	return &AuthError{Status: http.StatusUnauthorized, Code: "authentication_error", Machine: "api_key_expired", Message: "API key expired"}
}

func errTokenQuota() *AuthError {
	return &AuthError{Status: http.StatusTooManyRequests, Code: "rate_limit_error", Machine: "token_quota_exceeded", Message: "token limit exceeded"}
}

func errCreditQuota() *AuthError {
	return &AuthError{Status: http.StatusTooManyRequests, Code: "rate_limit_error", Machine: "credit_quota_exceeded", Message: "credit limit exceeded"}
}

func errRequestQuota() *AuthError {
	return &AuthError{Status: http.StatusTooManyRequests, Code: "rate_limit_error", Machine: "request_quota_exceeded", Message: "request limit exceeded"}
}

func errNoKeysConfigured() *AuthError {
	return &AuthError{Status: http.StatusUnauthorized, Code: "authentication_error", Machine: "invalid_api_key", Message: "API key authentication is required but no keys are configured"}
}

// QueryError is a 4xx portal data-plane error. It is never used as an
// authentication failure.
type QueryError struct {
	Status  int
	Code    string
	Message string
}

func (e *QueryError) Error() string { return e.Message }

func errInvalidRange(msg string) *QueryError {
	return &QueryError{Status: http.StatusBadRequest, Code: "invalid_range", Message: msg}
}

func errRangeTooLarge(msg string) *QueryError {
	return &QueryError{Status: http.StatusBadRequest, Code: "range_too_large", Message: msg}
}

func errInvalidCursor(msg string) *QueryError {
	return &QueryError{Status: http.StatusBadRequest, Code: "invalid_cursor", Message: msg}
}

func errInvalidMetric(msg string) *QueryError {
	return &QueryError{Status: http.StatusBadRequest, Code: "invalid_metric", Message: msg}
}

func errInvalidFilter(msg string) *QueryError {
	return &QueryError{Status: http.StatusBadRequest, Code: "invalid_filter", Message: msg}
}
