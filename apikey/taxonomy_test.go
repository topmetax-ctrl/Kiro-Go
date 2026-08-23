package apikey

import "testing"

func TestClassifyPublicError(t *testing.T) {
	cases := []struct {
		status  int
		errType string
		msg     string
		want    string
	}{
		{503, "api_error", "No available accounts", ErrorNoAvailableAccounts},
		{400, "invalid_request_error", "Invalid JSON", ErrorValidation},
		{401, "authentication_error", "Invalid or missing API key", ErrorAuthenticationFailed},
		{401, "authentication_error", "API key disabled", ErrorAPIKeyDisabled},
		{429, "rate_limit_error", "token limit exceeded", ErrorTokenQuota},
		{429, "rate_limit_error", "request limit exceeded", ErrorRequestQuota},
		{429, "api_error", "too many requests", ErrorProviderRateLimited},
		{499, "canceled", "client canceled", ErrorClientCancelled},
		{500, "api_error", "upstream exploded", ErrorProviderError},
		{503, "api_error", "temporarily down", ErrorProviderUnavailable},
		{504, "api_error", "gateway timeout", ErrorProviderTimeout},
	}
	for _, tc := range cases {
		got := ClassifyPublicError(tc.status, tc.errType, tc.msg)
		if got != tc.want {
			t.Errorf("status=%d type=%s msg=%q: got %s want %s", tc.status, tc.errType, tc.msg, got, tc.want)
		}
	}
}

func TestValidatePublicEvent(t *testing.T) {
	ok := PublicEvent{
		RequestID: "r1", Endpoint: "claude", Status: OutcomeSuccess,
		ClientModel: "claude-sonnet-4.5", Model: "claude-sonnet-4.5",
		InputTokens: 3, OutputTokens: 2, TotalTokens: 5, UsageSource: UsageSourceEstimator,
	}
	if err := ValidatePublicEvent(ok); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.Status = "nope"
	if err := ValidatePublicEvent(bad); err == nil {
		t.Fatal("expected invalid status")
	}
	fail := ok
	fail.Status = OutcomeFailed
	fail.ErrorCode = ""
	if err := ValidatePublicEvent(fail); err == nil {
		t.Fatal("failed without errorCode")
	}
}
