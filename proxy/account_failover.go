package proxy

import (
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
	"strings"
	"time"
)

const maxAccountRetryAttempts = 3

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
	// A disabled account leaves the routable pool; drop its cached models so the
	// global aggregate stops advertising models only it offered.
	h.modelCache.DropAccount(account.ID)
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	// Prefer the typed category when the upstream layer produced one: the decision
	// was made where the HTTP status code was authoritative, so it cannot be fooled
	// by digits embedded in request-IDs or bodies. Fall back to legacy substring
	// matching for non-HTTP producers (profile resolution, suspension probes) and
	// any error that predates the typed path.
	var ke *KiroUpstreamError
	if errors.As(err, &ke) && ke.Category != KiroErrUnknown {
		h.applyFailureCategory(account, ke.Category)
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		h.applyFailureCategory(account, KiroErrOverage)
	case isQuotaErrorMessage(errMsg):
		h.applyFailureCategory(account, KiroErrQuota)
	case isSuspensionErrorMessage(errMsg):
		h.applyFailureCategory(account, KiroErrSuspension)
	case isProfileUnavailableErrorMessage(errMsg):
		h.applyFailureCategory(account, KiroErrProfileUnavail)
	case isAuthErrorMessage(errMsg):
		h.applyFailureCategory(account, KiroErrAuth)
	default:
		h.pool.RecordError(account.ID, false)
	}
}

// applyFailureCategory maps a classified failure to its pool/ban side effects.
// Kept as a single switch so the typed and legacy paths cannot drift apart.
func (h *Handler) applyFailureCategory(account *config.Account, cat KiroErrorCategory) {
	switch cat {
	case KiroErrOverage:
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case KiroErrQuota:
		h.pool.RecordError(account.ID, true)
	case KiroErrSuspension:
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case KiroErrProfileUnavail:
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case KiroErrAuth:
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}
