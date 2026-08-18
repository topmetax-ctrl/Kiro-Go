package proxy

import (
	"context"
	"kiro-go/apikey"
	"kiro-go/config"
	"net/http"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// apiKeyContextKey is an unexported type used as the context key for the matched ApiKeyEntry
// so it cannot collide with keys defined in other packages.
type apiKeyContextKey struct{}

// clientIPContextKey carries the caller's resolved source IP down the request
// pipeline so the metrics funnel can attribute traffic per source IP without
// threading the IP through every function signature.
type clientIPContextKey struct{}

type requestIDContextKey struct{}

// authError describes why authentication failed. status is the HTTP status code to send.
type authError struct {
	status  int
	code    string
	message string
}

func (e *authError) Error() string { return e.message }

func newAuthError(status int, code, message string) *authError {
	return &authError{status: status, code: code, message: message}
}

// extractProvidedKey reads the API key from Authorization (Bearer ...) or X-Api-Key header.
func extractProvidedKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return v
	}
	return ""
}

func shouldReserveRequest(r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses":
		return true
	default:
		return false
	}
}

func apiKeyRecordToEntry(rec apikey.Record) *config.ApiKeyEntry {
	e := &config.ApiKeyEntry{
		ID:            rec.Key.ID,
		Name:          rec.Key.Name,
		Enabled:       rec.Key.Enabled,
		Migrated:      rec.Key.Migrated,
		CreatedAt:     rec.Key.CreatedAt.Unix(),
		TokenLimit:    rec.Quota.TokenLimit,
		CreditLimit:   rec.Quota.CreditLimit,
		TokensUsed:    rec.Usage.TotalTokens,
		CreditsUsed:   rec.Usage.Credits,
		RequestsCount: rec.Usage.RequestsTotal,
	}
	if rec.Key.LastUsedAt != nil {
		e.LastUsedAt = rec.Key.LastUsedAt.Unix()
	}
	return e
}

func mapAPIKeyAuthErr(err error) error {
	if err == nil {
		return nil
	}
	if ae, ok := err.(*apikey.AuthError); ok {
		return newAuthError(ae.Status, ae.Code, ae.Message)
	}
	return newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
}

// authenticate validates an incoming request against the configured API keys.
//
// Master switch: config.RequireApiKey. When false, missing or unknown keys still
// pass (open access). A known, enabled key is still bound so the usage portal
// can attribute traffic without forcing the gate on.
//
// When RequireApiKey is true:
//  1. If the v2 store has keys (or the in-memory config list does), the provided
//     key MUST match an enabled, in-quota entry.
//  2. Else if the legacy single ApiKey field is set, the provided key MUST match it.
//  3. Else → fail-closed.
func (h *Handler) authenticate(r *http.Request) (*config.ApiKeyEntry, error) {
	if !config.IsApiKeyRequired() {
		return h.bindOptionalAPIKey(r)
	}

	provided := extractProvidedKey(r)
	reserve := shouldReserveRequest(r)

	if h.keys != nil && h.keys.HasKeys() {
		rec, err := h.keys.Authenticate(provided, reserve)
		if err != nil {
			return nil, mapAPIKeyAuthErr(err)
		}
		return apiKeyRecordToEntry(rec), nil
	}

	if config.HasApiKeys() {
		if provided == "" {
			return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
		}
		entry := config.FindApiKeyByValue(provided)
		if entry == nil {
			return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
		}
		if !entry.Enabled {
			return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "API key disabled")
		}
		if overToken, overCredit := config.ApiKeyOverLimit(*entry); overToken || overCredit {
			if overToken {
				return nil, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "token limit exceeded")
			}
			return nil, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "credit limit exceeded")
		}
		return entry, nil
	}

	// Legacy single-key path.
	expected := config.GetApiKey()
	if expected == "" {
		return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "API key authentication is required but no keys are configured")
	}
	if provided == "" || provided != expected {
		return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
	}
	return nil, nil
}

// bindOptionalAPIKey attributes a presented key while the master gate is off.
// Unknown or missing keys stay anonymous. A known disabled / expired / exhausted
// key still fails so the caller sees the same identity they sent.
func (h *Handler) bindOptionalAPIKey(r *http.Request) (*config.ApiKeyEntry, error) {
	provided := extractProvidedKey(r)
	if provided == "" {
		return nil, nil
	}
	reserve := shouldReserveRequest(r)
	if h.keys != nil && h.keys.HasKeys() {
		if _, err := h.keys.Lookup(provided); err != nil {
			return nil, nil
		}
		rec, err := h.keys.Authenticate(provided, reserve)
		if err != nil {
			return nil, mapAPIKeyAuthErr(err)
		}
		return apiKeyRecordToEntry(rec), nil
	}
	if !config.HasApiKeys() {
		return nil, nil
	}
	entry := config.FindApiKeyByValue(provided)
	if entry == nil {
		return nil, nil
	}
	if !entry.Enabled {
		return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "API key disabled")
	}
	if overToken, overCredit := config.ApiKeyOverLimit(*entry); overToken || overCredit {
		if overToken {
			return nil, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "token limit exceeded")
		}
		return nil, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "credit limit exceeded")
	}
	return entry, nil
}

func withApiKeyContext(r *http.Request, entry *config.ApiKeyEntry) *http.Request {
	if entry == nil {
		return r
	}
	ctx := context.WithValue(r.Context(), apiKeyContextKey{}, entry.ID)
	return r.WithContext(ctx)
}

func apiKeyIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(apiKeyContextKey{}).(string); ok {
		return v
	}
	return ""
}

func withClientIPContext(r *http.Request, ip string) *http.Request {
	if ip == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), clientIPContextKey{}, ip))
}

func clientIPFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(clientIPContextKey{}).(string); ok {
		return v
	}
	return ""
}

func withRequestIDContext(r *http.Request, id string) *http.Request {
	if id == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id))
}

func requestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDContextKey{}).(string); ok {
		return v
	}
	return ""
}

func requestIDFromRequest(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if v == "" {
		v = strings.TrimSpace(r.Header.Get("X-Request-ID"))
	}
	if v != "" && len(v) <= 128 && isRequestIDSafe(v) {
		return v
	}
	return uuid.NewString()
}

func isRequestIDSafe(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || (!unicode.IsPrint(r) && r != ' ') {
			return false
		}
	}
	return true
}
