package providererr

import "time"

// Category is the internal classification used for routing, metrics, and
// public mapping. It is never sent to clients.
type Category string

const (
	CategoryRateLimited Category = "rate_limited"
	CategoryTimeout     Category = "timeout"
	CategoryUnavailable Category = "unavailable"
	CategoryRejected    Category = "rejected"
	CategoryError       Category = "error"
	CategoryCanceled    Category = "canceled"
	CategoryInternal    Category = "internal"
)

// InternalError is the canonical parsed upstream failure. It is never
// JSON-serialized to a public API. Admin diagnostics are copied into a
// dedicated store after secret redaction.
type InternalError struct {
	RequestID string
	Source    string // "forward", "kiro", "search", "network"

	ProviderID     string
	ProviderName   string
	AccountID      string
	Endpoint       string
	ClientModel    string
	EffectiveModel string

	UpstreamStatus    int
	UpstreamCode      string
	UpstreamMessage   string
	UpstreamRequestID string
	RetryAfter        string

	Detail          string
	DetailTruncated bool

	NetworkKind string // timeout, dns, tls, refused, eof, canceled, deadline
	Category    Category
	Retryable   bool

	Timestamp time.Time
	LatencyMs int64
	Attempt   int
}

// AdminSummary is a short redacted line for metrics.Event.ErrorMsg and
// RequestLog.Error. It is admin-only and still must not contain credentials.
func (e InternalError) AdminSummary() string {
	if e.UpstreamCode != "" && e.UpstreamMessage != "" {
		return truncateRunes(e.UpstreamCode+": "+e.UpstreamMessage, 300)
	}
	if e.UpstreamMessage != "" {
		return truncateRunes(e.UpstreamMessage, 300)
	}
	if e.Detail != "" {
		return truncateRunes(e.Detail, 300)
	}
	if e.NetworkKind != "" {
		return e.NetworkKind
	}
	if e.Category != "" {
		return string(e.Category)
	}
	return "upstream error"
}

// Public maps this internal failure onto the client-safe representation.
func (e InternalError) Public() PublicError {
	code, status, retryable := MapPublic(e.Category, e.UpstreamStatus)
	return PublicError{
		Code:       code,
		Message:    MessageForCode(code),
		RequestID:  e.RequestID,
		HTTPStatus: status,
		Retryable:  retryable,
		Type:       EnvelopeType(code, true),
	}
}

// MapPublic is the single HTTP/code mapping table.
func MapPublic(cat Category, upstreamStatus int) (code string, httpStatus int, retryable bool) {
	switch cat {
	case CategoryRateLimited:
		return CodeRateLimited, 429, true
	case CategoryTimeout:
		return CodeTimeout, 504, true
	case CategoryUnavailable:
		status := 502
		if upstreamStatus == 503 {
			status = 503
		}
		return CodeUnavailable, status, true
	case CategoryRejected:
		status := 400
		if upstreamStatus == 404 {
			status = 404
		}
		if upstreamStatus == 409 {
			status = 409
		}
		if upstreamStatus == 422 {
			status = 422
		}
		return CodeRejected, status, false
	case CategoryCanceled:
		return CodeCancelled, 499, false
	case CategoryInternal:
		return CodeInternal, 500, false
	default:
		if upstreamStatus == 401 || upstreamStatus == 403 {
			return CodeError, 502, false
		}
		status := 502
		if upstreamStatus >= 500 && upstreamStatus <= 599 {
			status = 502
			if upstreamStatus == 503 {
				status = 503
				return CodeUnavailable, status, true
			}
		}
		return CodeError, status, true
	}
}
