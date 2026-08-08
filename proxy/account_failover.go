package proxy

import (
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
	"strings"
)

const maxAccountRetryAttempts = 3

// maxSameAccountStreamRetries bounds same-account recovery of a truncated
// stream. Kiro IDE caps its truncation retry at one (Dt3 = 1 in extension.js),
// but that budget was set for a single-credential client. This proxy also
// rotates accounts, and the two recover different failures: a same-account
// retry helps when the upstream hiccupped, rotation helps when that account's
// backend is unhealthy. Two is therefore not a copy of the IDE.
//
// Cost of the extra attempt is bounded and small. Truncation returns nil from
// CallKiroAPIContext, so it never reaches the endpoint fallback or
// maxStreamAttemptsPerEndpoint - those fire only on transport errors. Worst
// case across maxAccountRetryAttempts accounts is 3*(1+budget) requests: 9 at
// two retries versus 6 at one.
//
// The payoff lands mostly on the three fully buffered paths, whose canRetry is
// nil: a non-stream client gets a 500 with nothing usable, so one more chance
// is worth more there than on a stream that has already flushed partial text.
const maxSameAccountStreamRetries = 2

// errUpstreamTruncatedResponse is a soft failure raised when a transport-clean
// stream carried content but never a terminal signal. It is retryable on the
// same account and must not mark the account unhealthy.
//
// There is deliberately no empty-response error here. A stream that produced no
// output at all is already caught one layer down: parseEventStreamTracked
// returns errEmptyKiroStream when !sawOutput (proxy/kiro.go), and
// CallKiroAPIContext retries it internally. Since sawOutput is set by exactly
// the three signals classifyStreamIntegrity measures (content, reasoning,
// toolUse), an all-zero measurement can never reach this layer with a nil
// error.
var errUpstreamTruncatedResponse = errors.New("upstream truncated response without stop reason")

// classifyStreamIntegrity decides whether an upstream stream that returned no
// transport error is actually complete. parseEventStream reports success on a
// clean EOF, so a stream that died mid-answer is otherwise indistinguishable
// from a finished one.
//
// Complete when a tool call was delivered, or when a stopReason arrived AND the
// turn actually produced an answer. The tool-call case matches Kiro IDE, whose
// empty and truncation predicates each require toolCallCount === 0.
//
// Truncated when content arrived without any terminal signal.
//
// Reasoning-only with no answer is STRICTER THAN THE IDE, deliberately. The
// IDE's truncation predicate ends in (contentChars > 0 || !reasoningSeen), so
// reasoning with no answer and no stopReason is treated as complete there and
// is never retried. That is the exact shape of the production symptom this
// proxy exists to fix: thinking streams in full, then the turn dies before the
// answer or the tool call. Handing a client reasoning with no answer as a
// successful turn is what made the failure invisible, so it is classified as
// truncated here.
//
// # A STOP REASON DOES NOT PROVE THE TURN PRODUCED ANYTHING
//
// Checking stopReason first (which this function used to do) reopened exactly
// the hole the reasoning rule above was written to close. Upstream can send
// reasoningContentEvent frames followed by metadataEvent{stopReason:"end_turn"}
// and no assistantResponseEvent and no toolUseEvent at all. That combination
// survived every guard: reasoning sets sawOutput in parseEventStreamTracked, so
// errEmptyKiroStream never fires, and the non-empty stopReason short-circuited
// here before the content check was ever reached. The client got
// message_delta(stop_reason=end_turn) + message_stop with an empty body — a
// turn that just ends mid-conversation with nothing on screen.
//
// So an empty turn is judged by whether the stop reason EXPLAINS the emptiness:
//
//   - max_tokens / context_window_exceeded / refusal / stop_sequence: yes. The
//     budget was consumed (often by thinking), the model declined, or a stop
//     sequence hit immediately. Retrying burns quota to reproduce the same
//     outcome, and the client needs the real reason to react correctly.
//   - end_turn (and anything unrecognized, which mapClaudeStopReason also folds
//     into end_turn): no. "The turn finished normally" and "there is no answer"
//     cannot both be true, so the stream is treated as truncated and retried.
func classifyStreamIntegrity(contentChars, toolCallCount int, stopReason string, sawReasoning bool) error {
	// A delivered tool call is a real answer even with no prose alongside it.
	if toolCallCount > 0 {
		return nil
	}
	if contentChars > 0 {
		if strings.TrimSpace(stopReason) != "" {
			return nil
		}
		// Content with no terminal signal: died mid-answer.
		return errUpstreamTruncatedResponse
	}
	// Nothing client-visible was produced. Only a stop reason that accounts for
	// that is allowed to pass; sawReasoning is deliberately not a substitute for
	// an answer.
	if stopReasonExplainsEmptyTurn(stopReason) {
		return nil
	}
	return errUpstreamTruncatedResponse
}

// stopReasonExplainsEmptyTurn reports whether stopReason is itself a sufficient
// account of a turn that carried no text and no tool call.
//
// The vocabulary matches mapClaudeStopReason's switch, which is what the client
// ultimately sees. Anything not listed there becomes end_turn, and an empty
// end_turn is the silent failure this guard exists to catch — so the default is
// deliberately "does not explain".
func stopReasonExplainsEmptyTurn(stopReason string) bool {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "max_tokens", "max_output_tokens", "length",
		"model_context_window_exceeded", "context_window_exceeded",
		"refusal", "content_filter", "content_filtered", "guardrail_intervened",
		"stop_sequence":
		return true
	default:
		return false
	}
}

// isStreamIntegrityError reports whether err is a soft integrity failure.
// Callers may rotate accounts on these, but must not run them through
// handleAccountFailure: an upstream blip should not mark an account unhealthy.
func isStreamIntegrityError(err error) bool {
	return errors.Is(err, errUpstreamTruncatedResponse)
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	// Only match genuine quota exhaustion, not anti-abuse "suspicious activity" flags.
	if strings.Contains(msg, "suspicious activity") {
		return false
	}
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
}

// isAntiAbuseMessage reports whether the error indicates an AWS anti-abuse
// temporary limit (429 with "suspicious activity"). These should NOT be treated
// as quota exhaustion — the account is not out of credits, it's being throttled.
func isAntiAbuseMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") && strings.Contains(msg, "suspicious activity")
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

	if !account.Enabled && account.BanStatus == banStatus && account.BanReason == banReason {
		return
	}

	if err := config.SetAccountBanStatus(account.ID, banStatus, banReason); err != nil {
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
	// Must precede the quota arm: an anti-abuse throttle is also a 429, but it
	// needs exponential backoff rather than the quota cooldown.
	case isAntiAbuseMessage(errMsg):
		h.applyFailureCategory(account, KiroErrAntiAbuse)
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
	case KiroErrAntiAbuse:
		// AWS anti-abuse temporary limit ("suspicious activity").
		// Exponential backoff (5→10→20→40→80 min) avoids renewing
		// the AWS-side investigation timer with rapid retries.
		h.pool.RecordAntiAbuse(account.ID)
		logger.Warnf("[AccountFailover] Anti-abuse throttle for %s", account.Email)
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
