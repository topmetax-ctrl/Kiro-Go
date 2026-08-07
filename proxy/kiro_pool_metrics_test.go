package proxy

import (
	"errors"
	"testing"

	"kiro-go/metrics"
)

func TestKiroStatusForMapsErrorCategories(t *testing.T) {
	cases := map[string]int{
		"quota":     429,
		"overage":   429,
		"auth":      401,
		"suspended": 403,
		"profile":   424,
		"unknown":   500,
		"":          500,
	}
	for errType, want := range cases {
		got := kiroStatusFor(kiroMetric{ErrorType: errType})
		if got != want {
			t.Errorf("errorType %q -> %d, want %d", errType, got, want)
		}
	}
	if got := kiroStatusFor(kiroMetric{Ok: true}); got != 200 {
		t.Errorf("success -> %d, want 200", got)
	}
}

func TestKiroAccountLabelFallsBackToIDPrefix(t *testing.T) {
	if got := kiroAccountLabel(""); got != "" {
		t.Fatalf("empty id -> %q, want empty", got)
	}
	// No such account in config: fall back to a short id prefix so the UI still
	// has a stable label rather than a blank cell.
	if got := kiroAccountLabel("abcdefghijklmnop"); got != "abcdefgh" {
		t.Fatalf("unknown id -> %q, want abcdefgh", got)
	}
	if got := kiroAccountLabel("short"); got != "short" {
		t.Fatalf("short id -> %q, want short", got)
	}
}

func TestRecordSuccessLogSplitFeedsMetrics(t *testing.T) {
	metrics.Reset()
	h := &Handler{}

	h.recordSuccessLogSplit("claude", "claude-sonnet-4", "acct-1", 1200, 340, 2.5, 1500)

	d, ok := metrics.ProviderDetailFor(metrics.KiroPoolID, 60)
	if !ok {
		t.Fatal("kiro pool not recorded in metrics")
	}
	if d.Requests != 1 || d.Success != 1 {
		t.Fatalf("requests/success = %d/%d, want 1/1", d.Requests, d.Success)
	}
	// The input/output split must survive; a combined total would show out=0.
	if d.InputTokens != 1200 || d.OutputTokens != 340 {
		t.Fatalf("tokens = %d/%d, want 1200/340", d.InputTokens, d.OutputTokens)
	}
	if d.CostUSD != 2.5 {
		t.Fatalf("credits = %v, want 2.5", d.CostUSD)
	}
	if d.AvgLatencyMs != 1500 {
		t.Fatalf("latency = %d, want 1500", d.AvgLatencyMs)
	}
	if len(d.Models) != 1 || d.Models[0].Model != "claude-sonnet-4" {
		t.Fatalf("model breakdown = %+v", d.Models)
	}
	if len(d.Accounts) != 1 || d.Accounts[0].AccountID != "acct-1" {
		t.Fatalf("account breakdown = %+v", d.Accounts)
	}

	// And it must still land in the RequestLog ring with the combined total.
	logs := h.getRequestLogs()
	if len(logs) != 1 || logs[0].Tokens != 1540 {
		t.Fatalf("request log = %+v, want 1 entry with 1540 tokens", logs)
	}
}

func TestRecordFailureWithDetailsFeedsMetrics(t *testing.T) {
	metrics.Reset()
	h := &Handler{}

	h.recordFailureWithDetails("openai", "gpt-4o", "acct-2", errors.New("quota exceeded"))

	d, ok := metrics.ProviderDetailFor(metrics.KiroPoolID, 60)
	if !ok {
		t.Fatal("kiro pool not recorded in metrics")
	}
	if d.Failed != 1 || d.FailStreak != 1 {
		t.Fatalf("failed=%d streak=%d, want 1/1", d.Failed, d.FailStreak)
	}
	if len(d.RecentErrs) != 1 || d.RecentErrs[0].Message == "" {
		t.Fatalf("recent errors = %+v", d.RecentErrs)
	}
	if d.RecentErrs[0].Status != 429 {
		t.Fatalf("quota error status = %d, want 429", d.RecentErrs[0].Status)
	}
}

func TestRecordFailureWithNilErrorIsNotCounted(t *testing.T) {
	metrics.Reset()
	h := &Handler{}
	// A nil error means "no detail worth logging"; it must not manufacture a
	// metrics event either, or failure counts would double.
	h.recordFailureWithDetails("claude", "m", "a", nil)
	// Reset keeps known provider names (so filter dropdowns stay labeled), so
	// assert on the recorded request count rather than the entry's existence.
	if d, ok := metrics.ProviderDetailFor(metrics.KiroPoolID, 60); ok && d.Requests != 0 {
		t.Fatalf("nil error produced %d metrics events, want 0", d.Requests)
	}
}

func TestKiroPoolAppearsAlongsideForwardProviders(t *testing.T) {
	metrics.Reset()
	h := &Handler{}
	h.recordSuccessLogSplit("claude", "m", "acct", 10, 5, 0, 100)
	metrics.Record(metrics.Event{
		ProviderID: "up-1", ProviderName: "some-upstream", Ok: true, Status: 200, LatencyMs: 50,
	})

	stats := metrics.ProviderStats()
	var foundPool, foundUpstream bool
	for _, p := range stats {
		if p.ProviderID == metrics.KiroPoolID && p.ProviderName == metrics.KiroPoolName {
			foundPool = true
		}
		if p.ProviderID == "up-1" {
			foundUpstream = true
		}
	}
	if !foundPool || !foundUpstream {
		t.Fatalf("comparison table must list pool and upstream together: %+v", stats)
	}
}
