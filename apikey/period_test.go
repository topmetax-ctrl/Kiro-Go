package apikey

import (
	"testing"
	"time"
)

func TestPeriodKeyUTC(t *testing.T) {
	// 2026-08-18 01:00 UTC is a Tuesday.
	ts := time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC)
	if got := periodKey(ResetDaily, ts); got != "2026-08-18" {
		t.Fatalf("daily: %s", got)
	}
	if got := periodKey(ResetMonthly, ts); got != "2026-08" {
		t.Fatalf("monthly: %s", got)
	}
	if got := periodKey(ResetLifetime, ts); got != PeriodLifetime {
		t.Fatalf("lifetime: %s", got)
	}
	wk := periodKey(ResetWeekly, ts)
	if wk != "2026-W34" {
		t.Fatalf("weekly ISO: %s", wk)
	}
}

func TestPeriodBoundsDailyRoll(t *testing.T) {
	ts := time.Date(2026, 8, 18, 23, 0, 0, 0, time.UTC)
	start, end := periodBounds(ResetDaily, ts)
	if start.Hour() != 0 || start.Day() != 18 {
		t.Fatalf("start %v", start)
	}
	if end == nil || !end.Equal(time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end %v", end)
	}
}

func TestRecordStatus(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	rec := Record{Key: Key{Enabled: true}, Quota: Quota{TokenLimit: 100}, Usage: Usage{TotalTokens: 100}}
	if rec.Status(now) != StatusExhausted {
		t.Fatalf("got %s", rec.Status(now))
	}
	rec.Key.Enabled = false
	if rec.Status(now) != StatusDisabled {
		t.Fatalf("disabled: %s", rec.Status(now))
	}
	exp := now.Add(-time.Hour)
	rec.Key.Enabled = true
	rec.Key.ExpiresAt = &exp
	rec.Usage.TotalTokens = 0
	if rec.Status(now) != StatusExpired {
		t.Fatalf("expired: %s", rec.Status(now))
	}
}
