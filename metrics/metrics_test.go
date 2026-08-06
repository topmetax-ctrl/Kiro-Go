package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// reset clears global state between tests, since the store is a package-level
// singleton shared by every test in this package.
func reset(t *testing.T) {
	t.Helper()
	s = newStore()
}

func nowMs() int64 { return time.Now().UnixMilli() }

func ev(providerID string, ok bool, status int, latency int64) Event {
	return Event{
		TimeMs:       nowMs(),
		ClientModel:  "test-model",
		ProviderID:   providerID,
		ProviderName: "P-" + providerID,
		Status:       status,
		LatencyMs:    latency,
		Ok:           ok,
	}
}

func TestRecordAggregatesTokensAndCost(t *testing.T) {
	reset(t)
	e := ev("p1", true, 200, 1000)
	e.InputTokens = 100
	e.OutputTokens = 50
	e.CostUSD = 0.25
	Record(e)
	Record(e)

	o := Overall()
	if o.Requests != 2 || o.Success != 2 {
		t.Fatalf("requests/success = %d/%d, want 2/2", o.Requests, o.Success)
	}
	if o.InputTokens != 200 || o.OutputTokens != 100 {
		t.Fatalf("tokens = %d/%d, want 200/100", o.InputTokens, o.OutputTokens)
	}
	if o.CostUSD != 0.5 {
		t.Fatalf("cost = %v, want 0.5", o.CostUSD)
	}

	ps := ProviderStats()
	if len(ps) != 1 || ps[0].InputTokens != 200 || ps[0].CostUSD != 0.5 {
		t.Fatalf("provider stat = %+v", ps)
	}
	if ps[0].SuccessRate != 100 {
		t.Fatalf("successRate = %v, want 100", ps[0].SuccessRate)
	}
}

func TestSuccessRateIsNegativeWithoutTraffic(t *testing.T) {
	reset(t)
	// A provider that only ever had in-flight requests (no completions) must not
	// report a misleading 0% success rate.
	done := BeginInFlight("p1", "P-p1")
	defer done()
	ps := ProviderStats()
	if len(ps) != 1 {
		t.Fatalf("want 1 provider, got %d", len(ps))
	}
	if ps[0].SuccessRate != -1 {
		t.Fatalf("successRate = %v, want -1", ps[0].SuccessRate)
	}
}

func TestTTFBAveragesOnlyOverKnownSamples(t *testing.T) {
	reset(t)
	a := ev("p1", true, 200, 1000)
	a.TTFBMs = 200
	Record(a)
	b := ev("p1", true, 200, 1000) // no TTFB measured
	Record(b)

	ps := ProviderStats()
	if ps[0].AvgTTFBMs != 200 {
		t.Fatalf("avgTTFB = %d, want 200 (must not be diluted by unknown samples)", ps[0].AvgTTFBMs)
	}
}

func TestFailStreakAndHealth(t *testing.T) {
	reset(t)
	for i := 0; i < 3; i++ {
		Record(ev("p1", false, 500, 10))
	}
	ps := ProviderStats()
	if ps[0].FailStreak != 3 || ps[0].Healthy {
		t.Fatalf("streak=%d healthy=%v, want 3/false", ps[0].FailStreak, ps[0].Healthy)
	}

	Record(ev("p1", true, 200, 10))
	ps = ProviderStats()
	if ps[0].FailStreak != 0 || !ps[0].Healthy {
		t.Fatalf("streak=%d healthy=%v, want 0/true", ps[0].FailStreak, ps[0].Healthy)
	}
	if ps[0].MaxStreak != 3 {
		t.Fatalf("maxStreak = %d, want 3", ps[0].MaxStreak)
	}
}

func TestInFlightTracking(t *testing.T) {
	reset(t)
	d1 := BeginInFlight("p1", "P-p1")
	d2 := BeginInFlight("p1", "P-p1")
	ps := ProviderStats()
	if ps[0].InFlight != 2 || ps[0].PeakInFlight != 2 {
		t.Fatalf("inflight=%d peak=%d, want 2/2", ps[0].InFlight, ps[0].PeakInFlight)
	}
	d1()
	d1() // idempotent: must not double-decrement
	ps = ProviderStats()
	if ps[0].InFlight != 1 {
		t.Fatalf("inflight = %d, want 1 after idempotent done", ps[0].InFlight)
	}
	d2()
	ps = ProviderStats()
	if ps[0].InFlight != 0 || ps[0].PeakInFlight != 2 {
		t.Fatalf("inflight=%d peak=%d, want 0/2", ps[0].InFlight, ps[0].PeakInFlight)
	}
}

func TestStatusHistogramIsBounded(t *testing.T) {
	reset(t)
	for i := 0; i < topStatusCodes*3; i++ {
		Record(ev("p1", false, 400+i, 10))
	}
	d, ok := ProviderDetailFor("p1", 60)
	if !ok {
		t.Fatal("provider not found")
	}
	if len(d.Statuses) > topStatusCodes {
		t.Fatalf("status codes = %d, want <= %d", len(d.Statuses), topStatusCodes)
	}
}

func TestProviderDetailBreakdowns(t *testing.T) {
	reset(t)
	a := ev("p1", true, 200, 1000)
	a.ClientModel = "model-a"
	a.AccountID = "acc1"
	a.AccountLabel = "a@example.com"
	a.OutputTokens = 10
	Record(a)

	b := ev("p1", false, 429, 500)
	b.ClientModel = "model-b"
	b.AccountID = "acc2"
	b.ErrorMsg = "rate limited"
	Record(b)

	d, ok := ProviderDetailFor("p1", 60)
	if !ok {
		t.Fatal("provider not found")
	}
	if len(d.Models) != 2 || len(d.Accounts) != 2 {
		t.Fatalf("models=%d accounts=%d, want 2/2", len(d.Models), len(d.Accounts))
	}
	if len(d.RecentErrs) != 1 || d.RecentErrs[0].Status != 429 {
		t.Fatalf("recent errors = %+v", d.RecentErrs)
	}
	if len(d.Minutes) != 60 {
		t.Fatalf("minutes = %d, want 60", len(d.Minutes))
	}
	var found bool
	for _, st := range d.Statuses {
		if st.Status == 429 && st.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("status 429 missing from %+v", d.Statuses)
	}
}

func TestRecentErrorsAreCappedAndNewestFirst(t *testing.T) {
	reset(t)
	for i := 0; i < recentErrorsKept+5; i++ {
		e := ev("p1", false, 500, 10)
		e.TimeMs = int64(i) + 1
		e.ErrorMsg = "err"
		Record(e)
	}
	d, _ := ProviderDetailFor("p1", 60)
	if len(d.RecentErrs) != recentErrorsKept {
		t.Fatalf("kept %d errors, want %d", len(d.RecentErrs), recentErrorsKept)
	}
	if d.RecentErrs[0].TimeMs <= d.RecentErrs[1].TimeMs {
		t.Fatalf("errors not newest-first: %d then %d", d.RecentErrs[0].TimeMs, d.RecentErrs[1].TimeMs)
	}
}

func TestEventFilters(t *testing.T) {
	reset(t)
	base := nowMs()

	a := ev("p1", true, 200, 10)
	a.TimeMs = base
	Record(a)

	b := ev("p2", false, 429, 10)
	b.TimeMs = base + 1000
	Record(b)

	c := ev("p1", false, 499, 10)
	c.TimeMs = base + 2000
	c.Canceled = true
	Record(c)

	if _, n := Events(EventFilter{ProviderID: "p1"}); n != 2 {
		t.Fatalf("provider filter = %d, want 2", n)
	}
	if _, n := Events(EventFilter{Status: "canceled"}); n != 1 {
		t.Fatalf("canceled filter = %d, want 1", n)
	}
	if _, n := Events(EventFilter{StatusCode: 429}); n != 1 {
		t.Fatalf("status code filter = %d, want 1", n)
	}
	if _, n := Events(EventFilter{SinceMs: base + 1000}); n != 2 {
		t.Fatalf("since filter = %d, want 2", n)
	}
	if _, n := Events(EventFilter{UntilMs: base}); n != 1 {
		t.Fatalf("until filter = %d, want 1", n)
	}
	// Only the 429 is an upstream error: the 499 is a client cancellation and is
	// reachable through Status:"canceled", not through the error view.
	if _, n := Events(EventFilter{Status: "error"}); n != 1 {
		t.Fatalf("error filter = %d, want 1 (cancellations are not upstream errors)", n)
	}
}

func TestCancellationIsNotAProviderFailure(t *testing.T) {
	reset(t)
	// A client that disconnects mid-stream must not be blamed on the upstream:
	// the provider answered fine, the user simply stopped reading.
	Record(ev("p1", true, 200, 100))
	c := ev("p1", false, 499, 50)
	c.Canceled = true
	Record(c)

	ps := ProviderStats()
	if ps[0].Requests != 2 {
		t.Fatalf("requests = %d, want 2 (cancellations still count as volume)", ps[0].Requests)
	}
	if ps[0].Failed != 0 || ps[0].Canceled != 1 {
		t.Fatalf("failed/canceled = %d/%d, want 0/1", ps[0].Failed, ps[0].Canceled)
	}
	// One success, one cancellation: the cancellation leaves the denominator, so
	// the rate is 100% rather than 50%.
	if ps[0].SuccessRate != 100 {
		t.Fatalf("successRate = %v, want 100", ps[0].SuccessRate)
	}
	if !ps[0].Healthy || ps[0].FailStreak != 0 {
		t.Fatalf("healthy=%v streak=%d, want true/0", ps[0].Healthy, ps[0].FailStreak)
	}
}

func TestCancellationsStayOutOfTheErrorList(t *testing.T) {
	reset(t)
	// A canceled stream carries the upstream's 200, so letting it into the error
	// list would render a nonsensical "200 error" row.
	for i := 0; i < 3; i++ {
		c := ev("p1", false, 200, 10)
		c.Canceled = true
		Record(c)
	}
	d, ok := ProviderDetailFor("p1", 60)
	if !ok {
		t.Fatal("provider not found")
	}
	if len(d.RecentErrs) != 0 {
		t.Fatalf("recent errors = %+v, want none", d.RecentErrs)
	}
	if !d.Healthy {
		t.Fatal("three cancellations must not mark a provider unhealthy")
	}
	// With no decided request, the rate is unknown rather than 0%.
	if d.SuccessRate != -1 {
		t.Fatalf("successRate = %v, want -1", d.SuccessRate)
	}
}

func TestResetProviderLeavesOthersIntact(t *testing.T) {
	reset(t)
	Record(ev("p1", true, 200, 100))
	Record(ev("p1", false, 500, 100))
	Record(ev("p2", true, 200, 100))

	if !ResetProvider("p1") {
		t.Fatal("ResetProvider returned false")
	}
	if ResetProvider("nope") {
		t.Fatal("ResetProvider on unknown id should return false")
	}

	for _, p := range ProviderStats() {
		switch p.ProviderID {
		case "p1":
			if p.Requests != 0 {
				t.Fatalf("p1 requests = %d, want 0", p.Requests)
			}
			if p.ProviderName == "" {
				t.Fatal("p1 name should survive reset so filters stay labeled")
			}
		case "p2":
			if p.Requests != 1 {
				t.Fatalf("p2 requests = %d, want 1", p.Requests)
			}
		}
	}

	// Global totals must be rebuilt from surviving events only.
	o := Overall()
	if o.Requests != 1 {
		t.Fatalf("overall requests = %d, want 1", o.Requests)
	}
	if _, n := Events(EventFilter{ProviderID: "p1"}); n != 0 {
		t.Fatalf("p1 events remaining = %d, want 0", n)
	}
	if _, n := Events(EventFilter{ProviderID: "p2"}); n != 1 {
		t.Fatalf("p2 events = %d, want 1", n)
	}
}

func TestResetProviderPreservesTotalsOlderThanTheRing(t *testing.T) {
	reset(t)
	// Simulate a restart: lifetime counters loaded from disk, with an event ring
	// that is empty because it is never persisted. Resetting one provider must
	// subtract only that provider's totals, not collapse the aggregate to what
	// the ring happens to hold.
	s.overall = counter{requests: 5000, success: 4000, failed: 1000, totalLatencyMs: 500000}
	p1 := newProviderAgg()
	p1.name = "p1"
	p1.counter = counter{requests: 1000, success: 900, failed: 100, totalLatencyMs: 100000}
	s.byProvider["p1"] = p1
	p2 := newProviderAgg()
	p2.name = "p2"
	p2.counter = counter{requests: 4000, success: 3100, failed: 900, totalLatencyMs: 400000}
	s.byProvider["p2"] = p2

	if !ResetProvider("p1") {
		t.Fatal("ResetProvider returned false")
	}

	o := Overall()
	if o.Requests != 4000 || o.Success != 3100 || o.Failed != 900 {
		t.Fatalf("overall = %d/%d/%d, want 4000/3100/900", o.Requests, o.Success, o.Failed)
	}
	for _, p := range ProviderStats() {
		if p.ProviderID == "p2" && p.Requests != 4000 {
			t.Fatalf("p2 requests = %d, want 4000", p.Requests)
		}
		if p.ProviderID == "p1" && p.Requests != 0 {
			t.Fatalf("p1 requests = %d, want 0", p.Requests)
		}
	}
}

func TestResetProviderNeverGoesNegative(t *testing.T) {
	reset(t)
	// A provider whose recorded total exceeds the aggregate (possible only with a
	// hand-edited or partially-written stats file) must clamp, not underflow.
	s.overall = counter{requests: 10}
	p := newProviderAgg()
	p.counter = counter{requests: 999}
	s.byProvider["p1"] = p

	ResetProvider("p1")
	if got := Overall().Requests; got != 0 {
		t.Fatalf("overall requests = %d, want 0 (clamped)", got)
	}
}

func TestResetKeepsNamesAndInFlight(t *testing.T) {
	reset(t)
	Record(ev("p1", true, 200, 10))
	done := BeginInFlight("p1", "P-p1")

	Reset()

	ps := ProviderStats()
	if len(ps) != 1 || ps[0].ProviderName != "P-p1" {
		t.Fatalf("provider name lost after reset: %+v", ps)
	}
	if ps[0].InFlight != 1 {
		t.Fatalf("inflight = %d, want 1 preserved across reset", ps[0].InFlight)
	}
	// The outstanding request must not underflow the gauge when it completes.
	done()
	ps = ProviderStats()
	if ps[0].InFlight != 0 {
		t.Fatalf("inflight = %d, want 0", ps[0].InFlight)
	}
}

func TestHourlyHistoryRoundTrip(t *testing.T) {
	reset(t)
	e := ev("p1", true, 200, 1000)
	e.InputTokens = 7
	e.OutputTokens = 3
	Record(e)

	dir := t.TempDir()
	path := filepath.Join(dir, "forward_stats.json")
	if err := Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	reset(t)
	if err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	ps := ProviderStats()
	if len(ps) != 1 || ps[0].Requests != 1 || ps[0].InputTokens != 7 {
		t.Fatalf("counters not restored: %+v", ps)
	}

	h := HistoryFor("p1", 24)
	if len(h) != 24 {
		t.Fatalf("history len = %d, want 24", len(h))
	}
	last := h[len(h)-1]
	if last.Requests != 1 || last.OutputTokens != 3 {
		t.Fatalf("last hour bucket = %+v, want 1 request / 3 out tokens", last)
	}
	if last.AvgLatencyMs != 1000 {
		t.Fatalf("avg latency = %d, want 1000", last.AvgLatencyMs)
	}
}

func TestHistoryForAllProvidersAggregates(t *testing.T) {
	reset(t)
	Record(ev("p1", true, 200, 100))
	Record(ev("p2", true, 200, 100))
	Record(ev("p2", false, 500, 100))

	h := HistoryFor("", 24)
	last := h[len(h)-1]
	if last.Requests != 3 || last.Success != 2 || last.Failed != 1 {
		t.Fatalf("aggregate hour = %+v, want 3/2/1", last)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	reset(t)
	if err := Load(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
}

func TestLoadLegacyFileWithoutNewFields(t *testing.T) {
	reset(t)
	// A file written by the previous build: no tokens, no hours, no byModel.
	legacy := `{"overall":{"requests":5,"success":4,"failed":1,"totalLatencyMs":5000,"lastUsed":123},
	"byProvider":{"p1":{"requests":5,"success":4,"failed":1,"totalLatencyMs":5000,"lastUsed":123,"name":"Legacy"}},
	"byRoute":{}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "old.json")
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err != nil {
		t.Fatalf("legacy load: %v", err)
	}
	ps := ProviderStats()
	if len(ps) != 1 || ps[0].Requests != 5 || ps[0].ProviderName != "Legacy" {
		t.Fatalf("legacy state not restored: %+v", ps)
	}
	if ps[0].InputTokens != 0 || ps[0].AvgTTFBMs != 0 {
		t.Fatalf("absent dimensions should be zero, got %+v", ps[0])
	}
	if ps[0].AvgLatencyMs != 1000 {
		t.Fatalf("avg latency = %d, want 1000", ps[0].AvgLatencyMs)
	}
}

func TestPercentilesPerProvider(t *testing.T) {
	reset(t)
	for _, ms := range []int64{100, 200, 300, 400} {
		Record(ev("p1", true, 200, ms))
	}
	for i := 0; i < 4; i++ {
		Record(ev("p2", true, 200, 9000))
	}
	d, _ := ProviderDetailFor("p1", 60)
	if d.Percentiles.Count != 4 {
		t.Fatalf("count = %d, want 4 (p2 must not leak in)", d.Percentiles.Count)
	}
	if d.Percentiles.P50 > 400 {
		t.Fatalf("p50 = %d, want <= 400", d.Percentiles.P50)
	}
}

func TestKiroPoolRecordsUnderSentinelID(t *testing.T) {
	reset(t)
	e := ev(KiroPoolID, true, 200, 100)
	e.ProviderName = KiroPoolName
	e.AccountID = "acc-1"
	e.AccountLabel = "user@example.com"
	Record(e)

	d, ok := ProviderDetailFor(KiroPoolID, 60)
	if !ok {
		t.Fatal("kiro pool not recorded")
	}
	if len(d.Accounts) != 1 || d.Accounts[0].AccountLabel != "user@example.com" {
		t.Fatalf("account breakdown = %+v", d.Accounts)
	}
}

func TestSubscribeReceivesEvents(t *testing.T) {
	reset(t)
	ch, cancel := Subscribe()
	defer cancel()
	Record(ev("p1", true, 200, 10))
	select {
	case got := <-ch:
		if got.ProviderID != "p1" {
			t.Fatalf("got provider %q", got.ProviderID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestRPMOverWindow(t *testing.T) {
	reset(t)
	for i := 0; i < 10; i++ {
		Record(ev("p1", true, 200, 10))
	}
	ps := ProviderStats()
	// 10 requests in the current minute, averaged over the 5-minute window.
	if ps[0].RPM != 2 {
		t.Fatalf("rpm = %v, want 2", ps[0].RPM)
	}
}
