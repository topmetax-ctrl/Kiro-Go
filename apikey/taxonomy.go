package apikey

import "strings"

// Public error codes persisted on request_events.error_code and exposed on
// portal/admin events. Internal provider/account identifiers never appear here.
const (
	ErrorValidation           = "validation_error"
	ErrorAuthenticationFailed = "authentication_failed"
	ErrorAPIKeyDisabled       = "api_key_disabled"
	ErrorAPIKeyExpired        = "api_key_expired"
	ErrorRequestQuota         = "request_quota_exceeded"
	ErrorTokenQuota           = "token_quota_exceeded"
	ErrorCreditQuota          = "credit_quota_exceeded"
	ErrorNoAvailableAccounts  = "no_available_accounts"
	ErrorProviderRateLimited  = "provider_rate_limited"
	ErrorProviderError        = "provider_error"
	ErrorProviderTimeout      = "provider_timeout"
	ErrorClientCancelled      = "client_cancelled"
	ErrorServerTimeout        = "server_timeout"
	ErrorInternal             = "internal_error"
	ErrorUnsettledReservation = "unsettled_reservation"
)

var knownErrorCodes = map[string]struct{}{
	ErrorValidation:           {},
	ErrorAuthenticationFailed: {},
	ErrorAPIKeyDisabled:       {},
	ErrorAPIKeyExpired:        {},
	ErrorRequestQuota:         {},
	ErrorTokenQuota:           {},
	ErrorCreditQuota:          {},
	ErrorNoAvailableAccounts:  {},
	ErrorProviderRateLimited:  {},
	ErrorProviderError:        {},
	ErrorProviderTimeout:      {},
	ErrorClientCancelled:      {},
	ErrorServerTimeout:        {},
	ErrorInternal:             {},
	ErrorUnsettledReservation: {},
}

func KnownErrorCode(code string) bool {
	_, ok := knownErrorCodes[code]
	return ok
}

// ClassifyPublicError maps an HTTP status plus a client-facing type/message
// onto the public taxonomy. Messages are matched on sanitized text only.
func ClassifyPublicError(status int, errType, message string) string {
	msg := strings.ToLower(strings.TrimSpace(message))
	typ := strings.ToLower(strings.TrimSpace(errType))

	switch {
	case strings.Contains(msg, "no available account"):
		return ErrorNoAvailableAccounts
	case strings.Contains(msg, "token limit"):
		return ErrorTokenQuota
	case strings.Contains(msg, "credit limit"):
		return ErrorCreditQuota
	case strings.Contains(msg, "request limit"):
		return ErrorRequestQuota
	case strings.Contains(msg, "disabled"):
		return ErrorAPIKeyDisabled
	case strings.Contains(msg, "expired"):
		return ErrorAPIKeyExpired
	}

	if status == 401 || typ == "authentication_error" {
		return ErrorAuthenticationFailed
	}
	if status == 499 || typ == "canceled" || typ == "quota" || typ == "overage" || strings.Contains(msg, "cancel") {
		if typ == "quota" || typ == "overage" {
			return ErrorProviderRateLimited
		}
		return ErrorClientCancelled
	}
	if status == 408 {
		return ErrorServerTimeout
	}
	if status == 429 {
		if typ == "rate_limit_error" {
			return ErrorProviderRateLimited
		}
		return ErrorProviderRateLimited
	}
	if typ == "invalid_request_error" || status == 400 || status == 404 || status == 422 {
		return ErrorValidation
	}
	if status == 504 || strings.Contains(msg, "timeout") || strings.Contains(typ, "timeout") {
		return ErrorProviderTimeout
	}
	if status == 503 {
		return ErrorProviderError
	}
	if status >= 500 {
		return ErrorProviderError
	}
	if status >= 400 && status < 500 {
		return ErrorValidation
	}
	return ErrorInternal
}

// V1TokenCreditEnforcement is always soft: token/credit limits are checked at
// the next Authenticate, so the in-flight request may slightly overrun.
const V1TokenCreditEnforcement = EnforceSoft

// V1RequestEnforcement is exact reservation when a request limit is set.
const V1RequestEnforcementReserved = "reserved"
const V1RequestEnforcementNone = "none"

func RequestEnforcementFor(requestLimit int64) string {
	if requestLimit > 0 {
		return V1RequestEnforcementReserved
	}
	return V1RequestEnforcementNone
}
