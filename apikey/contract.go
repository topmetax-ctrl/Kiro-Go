package apikey

import "fmt"

// ValidatePublicEvent checks the inference telemetry contract. Empty
// client/effective model is allowed only for admissions that never parsed a
// body (validation_error on malformed JSON).
func ValidatePublicEvent(ev PublicEvent) error {
	if ev.RequestID == "" {
		return fmt.Errorf("missing requestId")
	}
	switch ev.Status {
	case OutcomeSuccess, OutcomeFailed, OutcomeCancelled, OutcomeRejected:
	default:
		return fmt.Errorf("invalid status %q", ev.Status)
	}
	if ev.Endpoint == "" {
		return fmt.Errorf("missing endpoint")
	}
	if ev.Status == OutcomeSuccess {
		if ev.ClientModel == "" && ev.EffectiveModel == "" && ev.Model == "" {
			return fmt.Errorf("success missing client/effective model")
		}
	}
	if ev.Status != OutcomeSuccess {
		if ev.ErrorCode == "" {
			return fmt.Errorf("%s missing errorCode", ev.Status)
		}
		if !KnownErrorCode(ev.ErrorCode) {
			return fmt.Errorf("unknown errorCode %q", ev.ErrorCode)
		}
	} else if ev.ErrorCode != "" && !KnownErrorCode(ev.ErrorCode) {
		return fmt.Errorf("unknown errorCode %q", ev.ErrorCode)
	}
	if ev.InputTokens < 0 || ev.OutputTokens < 0 || ev.TotalTokens < 0 {
		return fmt.Errorf("negative token counts")
	}
	if ev.TotalTokens != ev.InputTokens+ev.OutputTokens {
		return fmt.Errorf("totalTokens %d != input+output %d", ev.TotalTokens, ev.InputTokens+ev.OutputTokens)
	}
	if ev.UsageSource != "" {
		switch ev.UsageSource {
		case UsageSourceNone, UsageSourceUpstream, UsageSourceMetering, UsageSourceStreamObserved, UsageSourceEstimator:
		default:
			return fmt.Errorf("unknown usageSource %q", ev.UsageSource)
		}
	}
	return nil
}
