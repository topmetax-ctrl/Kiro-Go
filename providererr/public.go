// Package providererr is the trust-boundary for upstream/custom-provider
// failures. Internal diagnostics stay inside the process (and admin-only
// storage). PublicError is the only representation allowed to cross a client,
// portal, or other untrusted API.
package providererr

// Public taxonomy codes. These match apikey.Error* strings so portal events
// and client envelopes share one vocabulary. Local gateway errors
// (invalid API key, quota, validation) are NOT produced here.
const (
	CodeRateLimited = "provider_rate_limited"
	CodeTimeout     = "provider_timeout"
	CodeUnavailable = "provider_unavailable"
	CodeRejected    = "provider_rejected"
	CodeError       = "provider_error"
	CodeInternal    = "internal_error"
	CodeCancelled   = "client_cancelled"
)

const (
	MsgRateLimited = "The upstream service is temporarily rate limited."
	MsgTimeout     = "The upstream service timed out."
	MsgUnavailable = "The upstream service is temporarily unavailable."
	MsgRejected    = "The upstream service rejected the request."
	MsgError       = "An upstream service error occurred."
	MsgInternal    = "An internal server error occurred."
	MsgCancelled   = "The client cancelled the request."
)

// PublicError is the client/portal view of a provider failure. It has no
// provider identity, raw body, credential state, or routing internals.
type PublicError struct {
	Code       string
	Message    string
	RequestID  string
	HTTPStatus int
	Retryable  bool
	// Type is the protocol envelope type (api_error, server_error, rate_limit_error).
	Type string
}

func (e PublicError) MessageOrDefault() string {
	if e.Message != "" {
		return e.Message
	}
	return MessageForCode(e.Code)
}

// MessageForCode returns the stable generic client message for a public code.
func MessageForCode(code string) string {
	switch code {
	case CodeRateLimited:
		return MsgRateLimited
	case CodeTimeout:
		return MsgTimeout
	case CodeUnavailable:
		return MsgUnavailable
	case CodeRejected:
		return MsgRejected
	case CodeError:
		return MsgError
	case CodeCancelled:
		return MsgCancelled
	default:
		return MsgInternal
	}
}

// EnvelopeType maps a public code onto the Anthropic/OpenAI error.type field.
func EnvelopeType(code string, claude bool) string {
	switch code {
	case CodeRateLimited:
		if claude {
			return "rate_limit_error"
		}
		return "rate_limit_error"
	case CodeRejected:
		if claude {
			return "invalid_request_error"
		}
		return "invalid_request_error"
	default:
		if claude {
			return "api_error"
		}
		return "server_error"
	}
}
