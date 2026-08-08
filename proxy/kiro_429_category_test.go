package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// AWS reports two unrelated failures with HTTP 429: real quota exhaustion, and an
// anti-abuse throttle whose body names a "suspicious activity" investigation. They
// need opposite handling — a fixed quota cooldown versus exponential backoff,
// since each rapid retry renews AWS's investigation timer — and the response body
// is the only thing that tells them apart.
//
// The 429 branch in CallKiroAPIContext used to close the body unread and return a
// bare "quota exhausted on <endpoint>", which made KiroErrAntiAbuse unreachable
// in production: categorizeUpstream never saw the marker, and the substring
// fallback in handleAccountFailure had no body to match. Every throttle was
// therefore cooled down as an exhausted account.
//
// These tests pin the categorization at the point where the status code and body
// are both still available.
func TestCallKiroAPIContextCategorizes429(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantCategory KiroErrorCategory
	}{
		{
			name:         "anti-abuse throttle",
			body:         `{"__type":"ThrottlingException","message":"We detected suspicious activity on your account."}`,
			wantCategory: KiroErrAntiAbuse,
		},
		{
			name:         "genuine quota exhaustion",
			body:         `{"__type":"ThrottlingException","message":"Monthly request quota exceeded."}`,
			wantCategory: KiroErrQuota,
		},
		{
			name:         "empty body defaults to quota",
			body:         "",
			wantCategory: KiroErrQuota,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			defer setupIntegrityTestUpstream(t, server)()

			err := CallKiroAPIContext(context.Background(), integrityTestAccount(),
				integrityTestPayload(), &KiroStreamCallback{})
			if err == nil {
				t.Fatal("expected an error from a 429 response")
			}

			var ke *KiroUpstreamError
			if !errors.As(err, &ke) {
				t.Fatalf("429 must surface as *KiroUpstreamError so the category survives; got %T: %v", err, err)
			}
			if ke.StatusCode != http.StatusTooManyRequests {
				t.Errorf("StatusCode=%d, want 429", ke.StatusCode)
			}
			if ke.Category != tc.wantCategory {
				t.Errorf("Category=%q, want %q (body was %q)", ke.Category, tc.wantCategory, ke.Body)
			}
			if ke.Body != tc.body {
				t.Errorf("Body=%q, want %q — the body must be read, not discarded", ke.Body, tc.body)
			}
		})
	}
}

// handleAccountFailure prefers the typed category over substring matching. This
// checks the two paths agree for the anti-abuse case, which is the one the
// discarded body used to break: with the body present, even the legacy substring
// fallback classifies it correctly.
func TestAntiAbuseSurvivesBothClassificationPaths(t *testing.T) {
	body := `{"__type":"ThrottlingException","message":"suspicious activity detected"}`

	typed := &KiroUpstreamError{
		Category:   categorizeUpstream(http.StatusTooManyRequests, body),
		StatusCode: http.StatusTooManyRequests,
		Endpoint:   "Kiro CLI",
		Body:       body,
	}
	if typed.Category != KiroErrAntiAbuse {
		t.Fatalf("typed path: got %q, want %q", typed.Category, KiroErrAntiAbuse)
	}

	// Legacy substring path, as used for errors that predate the typed one.
	if !isAntiAbuseMessage(typed.Error()) {
		t.Errorf("legacy path: %q must be recognized as anti-abuse", typed.Error())
	}
	if isQuotaErrorMessage(typed.Error()) {
		t.Error("legacy path: an anti-abuse throttle must not also match quota, " +
			"or the quota cooldown would win depending on switch order")
	}

	// The old error string carried neither marker, so it fell through to quota.
	legacy := errors.New("quota exhausted on Kiro CLI")
	if isAntiAbuseMessage(legacy.Error()) {
		t.Error("the pre-fix error string cannot name anti-abuse; that was the bug")
	}
	if !isQuotaErrorMessage(legacy.Error()) {
		t.Error("the pre-fix error string was classified as quota")
	}
}
