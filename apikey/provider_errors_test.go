package apikey

import (
	"path/filepath"
	"testing"
	"time"
)

func TestProviderErrorDetailRoundTripAndCleanup(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "apikeys.db"), []byte("pepper"), Options{
		Retention: time.Hour,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.PutProviderErrorDetail(ProviderErrorDetail{
		RequestID: "req-1", Attempt: 0, ProviderName: "9router",
		UpstreamStatus: 403, UpstreamCode: "ACCOUNT_SUSPENDED",
		Detail: "account abc", PublicCode: ErrorProviderError, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProviderErrorDetails("req-1")
	if err != nil || len(got) != 1 || got[0].UpstreamCode != "ACCOUNT_SUSPENDED" {
		t.Fatalf("got %+v err=%v", got, err)
	}

	if err := s.PutProviderErrorDetail(ProviderErrorDetail{
		RequestID: "old", Detail: "gone", CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProviderErrorDetails("old"); err != ErrNotFound {
		t.Fatalf("expected old detail purged, err=%v", err)
	}
	if _, err := s.GetProviderErrorDetails("req-1"); err != nil {
		t.Fatalf("fresh detail purged: %v", err)
	}
}
