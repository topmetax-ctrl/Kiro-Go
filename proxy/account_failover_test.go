package proxy

import (
	"errors"
	"testing"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

// TestCategorizeUpstreamStatusDriven pins the core Phase-1 guarantee: the failure
// category is decided by the authoritative HTTP status code, so digits or words
// embedded in an upstream body (request-IDs, unrelated JSON) cannot flip it.
func TestCategorizeUpstreamStatusDriven(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   KiroErrorCategory
	}{
		{"401 is auth", 401, `{"message":"Forbidden"}`, KiroErrAuth},
		{"403 is auth", 403, "", KiroErrAuth},
		{"429 is quota", 429, "", KiroErrQuota},
		{"402 with overage marker", 402, "OVERAGE limit exceeded", KiroErrOverage},
		{"402 without overage is unknown", 402, "payment required", KiroErrUnknown},
		// The crux: a 500 body that merely CONTAINS "429" / "overage" as part of a
		// request-id must NOT be classified as quota/overage — status 500 wins.
		{"500 body mentioning 429 stays unknown", 500, "trace req-429abc failed", KiroErrUnknown},
		{"500 body mentioning overage stays unknown", 500, "internal error id=overage-77", KiroErrUnknown},
		// Non-status-driven markers upstream reports with a generic code.
		{"suspension marker", 500, "Your User ID temporarily is suspended", KiroErrSuspension},
		{"profile unavailable marker", 500, "no available Kiro profile", KiroErrProfileUnavail},
	}

	for _, tc := range tests {
		if got := categorizeUpstream(tc.status, tc.body); got != tc.want {
			t.Fatalf("%s: categorizeUpstream(%d, %q) = %q, want %q", tc.name, tc.status, tc.body, got, tc.want)
		}
	}
}

// TestHandleAccountFailurePrefersTypedCategory proves the typed path wins over
// substring matching: a KiroUpstreamError whose body contains "quota" but whose
// status is 500 must take the soft-error branch (RecordError, not a 1h quota
// cooldown), which the legacy string path would have misclassified.
func TestKiroUpstreamErrorPreservesLegacyString(t *testing.T) {
	e := &KiroUpstreamError{Category: KiroErrAuth, StatusCode: 403, Endpoint: "Kiro", Body: "Forbidden"}
	if got, want := e.Error(), "HTTP 403 from Kiro: Forbidden"; got != want {
		t.Fatalf("Error() = %q, want %q (legacy shape must be preserved)", got, want)
	}
	// errors.As must recover the typed error through a wrap.
	wrapped := errors.New("boom")
	e2 := &KiroUpstreamError{Category: KiroErrQuota, StatusCode: 429, Err: wrapped}
	var target *KiroUpstreamError
	if !errors.As(e2, &target) || target.Category != KiroErrQuota {
		t.Fatalf("errors.As failed to recover typed category")
	}
	if !errors.Is(e2, wrapped) {
		t.Fatalf("Unwrap chain broken: errors.Is could not find wrapped cause")
	}
}
