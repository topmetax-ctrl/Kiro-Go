package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// ProviderStatsWindow must actually scope its figures to the requested range —
// the Stats tab's selector is inert otherwise, which is the bug it exists to fix.
func TestProviderStatsWindowScopesToRange(t *testing.T) {
	reset(t)

	recent := ev("p1", true, 200, 1000)
	recent.InputTokens = 10
	recent.OutputTokens = 20
	recent.CostUSD = 0.5
	Record(recent)

	old := ev("p1", true, 200, 3000)
	old.TimeMs = nowMs() - 5*3600000 // 5 hours ago
	old.InputTokens = 100
	old.OutputTokens = 200
	old.CostUSD = 4
	Record(old)

	oneHour := ProviderStatsWindow(1)
	if len(oneHour) != 1 {
		t.Fatalf("want 1 provider, got %d", len(oneHour))
	}
	if oneHour[0].Requests != 1 {
		t.Fatalf("1h requests = %d, want 1 (the 5h-old event must be excluded)", oneHour[0].Requests)
	}
	if oneHour[0].CostUSD != 0.5 {
		t.Fatalf("1h cost = %v, want 0.5", oneHour[0].CostUSD)
	}
	if oneHour[0].InputTokens+oneHour[0].OutputTokens != 30 {
		t.Fatalf("1h tokens = %d, want 30", oneHour[0].InputTokens+oneHour[0].OutputTokens)
	}
	if oneHour[0].AvgLatencyMs != 1000 {
		t.Fatalf("1h avg latency = %d, want 1000", oneHour[0].AvgLatencyMs)
	}

	day := ProviderStatsWindow(24)
	if day[0].Requests != 2 {
		t.Fatalf("24h requests = %d, want 2", day[0].Requests)
	}
	if day[0].CostUSD != 4.5 {
		t.Fatalf("24h cost = %v, want 4.5", day[0].CostUSD)
	}
	if day[0].AvgLatencyMs != 2000 {
		t.Fatalf("24h avg latency = %d, want 2000", day[0].AvgLatencyMs)
	}
	// Live signals are current, not windowed.
	if day[0].ProviderName != "P-p1" || day[0].LastUsed == 0 {
		t.Fatalf("live fields not carried over: %+v", day[0])
	}
}

// A window with no decided traffic must report -1 (unknown), matching the
// all-time successRate contract, so the UI renders "—" instead of 0%.
func TestProviderStatsWindowUnknownSuccessRate(t *testing.T) {
	reset(t)
	old := ev("p1", true, 200, 1000)
	old.TimeMs = nowMs() - 5*3600000
	Record(old)

	got := ProviderStatsWindow(1)
	if len(got) != 1 {
		t.Fatalf("want 1 provider, got %d", len(got))
	}
	if got[0].SuccessRate != -1 {
		t.Fatalf("success rate = %v, want -1 for an empty window", got[0].SuccessRate)
	}
}

// hours <= 0 means "no window" and must behave exactly like ProviderStats.
func TestProviderStatsWindowZeroIsAllTime(t *testing.T) {
	reset(t)
	Record(ev("p1", true, 200, 1000))
	if ProviderStatsWindow(0)[0].Requests != ProviderStats()[0].Requests {
		t.Fatal("hours=0 must match all-time ProviderStats")
	}
}

// Hourly buckets carry cost so a windowed cost survives a restart. A snapshot
// written before this field existed simply loads it as zero.
func TestHourlyBucketCostRoundTrips(t *testing.T) {
	reset(t)
	e := ev("p1", true, 200, 1000)
	e.TimeMs = nowMs() - 3*3600000
	e.CostUSD = 2.5
	Record(e)

	path := filepath.Join(t.TempDir(), "m.json")
	if err := Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	reset(t)
	if err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	got := ProviderStatsWindow(24)
	if len(got) != 1 || got[0].CostUSD != 2.5 {
		t.Fatalf("windowed cost did not survive persist: %+v", got)
	}
}

// ptrOf is a test shorthand for the tri-state cache fields.
func ptrOf(n int64) *int64 { return &n }

// usageEvent builds an event with the given canonical totals plus a nested
// cache breakdown (nil = not reported). The canonical totals pointers mirror
// what the forwarding publisher writes: non-nil exactly when the flat total is
// positive.
func usageEvent(in, out int64, read, create *int64) Event {
	e := ev("p1", true, 200, 100)
	e.InputTokens = in
	e.OutputTokens = out
	e.Usage = &EventUsage{
		InputTokens:              ptrOfPositive(in),
		OutputTokens:             ptrOfPositive(out),
		CacheReadInputTokens:     read,
		CacheCreationInputTokens: create,
		Source:                   UsageSourceUpstream,
	}
	return e
}

func ptrOfPositive(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

func TestCacheAggregationWeightedNotAveraged(t *testing.T) {
	reset(t)
	// A: 1000 in, 1000 cached = 100%. B: 100000 in, 0 cached = 0%.
	// The weighted aggregate is 1000/101000 (~1%), NOT the 50% request average.
	Record(usageEvent(1000, 10, ptrOf(1000), ptrOf(0)))
	Record(usageEvent(100000, 500, ptrOf(0), nil))

	o := Overall()
	if o.CacheObservedRequests != 2 {
		t.Fatalf("observed = %d, want 2", o.CacheObservedRequests)
	}
	if o.CacheReadInputTokens != 1000 {
		t.Fatalf("reads = %d, want 1000", o.CacheReadInputTokens)
	}
	if o.CacheCreationInputTokens != 0 {
		t.Fatalf("creations = %d, want 0", o.CacheCreationInputTokens)
	}
	// 1000/101000 = 0.9901% — never 50.
	if want := 1000.0 * 100 / 101000.0; o.CacheHitRate < want-0.001 || o.CacheHitRate > want+0.001 {
		t.Fatalf("hit rate = %v, want ~%v (weighted)", o.CacheHitRate, want)
	}

	p := ProviderStats()
	if len(p) != 1 || p[0].CacheHitRate != o.CacheHitRate {
		t.Fatalf("provider cache hit = %+v", p)
	}
}

func TestCacheAggregationUnknownIsNotZero(t *testing.T) {
	reset(t)
	// Events without a cache breakdown must not enter the ratio's population:
	// no telemetry means unknown (-1), never a diluted 0%.
	Record(usageEvent(5000, 50, nil, nil))
	Record(ev("p1", true, 200, 10)) // no usage object at all (pool-style)

	o := Overall()
	if o.CacheObservedRequests != 0 {
		t.Fatalf("observed = %d, want 0", o.CacheObservedRequests)
	}
	if o.CacheHitRate != -1 {
		t.Fatalf("hit rate = %v, want -1 (unknown)", o.CacheHitRate)
	}
}

func TestCacheGenuineZeroHitIsZero(t *testing.T) {
	reset(t)
	// An upstream that explicitly reports cached_tokens: 0 has real telemetry:
	// a genuine 0% is a meaningful answer, not unknown.
	Record(usageEvent(800, 20, ptrOf(0), nil))
	o := Overall()
	if o.CacheHitRate != 0 {
		t.Fatalf("hit rate = %v, want 0 (explicitly reported)", o.CacheHitRate)
	}
}

func TestCacheAggregationSubtractsOnProviderReset(t *testing.T) {
	reset(t)
	Record(usageEvent(1000, 10, ptrOf(800), ptrOf(50)))
	Record(usageEvent(2000, 20, nil, nil)) // other provider
	Record(usageEvent(3000, 30, ptrOf(1500), nil))
	// Attribute one cache-reporting event to a second provider, then reset it.
	e := usageEvent(4000, 40, ptrOf(3200), nil)
	e.ProviderID = "p2"
	e.ProviderName = "P-p2"
	Record(e)

	if !ResetProvider("p2") {
		t.Fatal("ResetProvider(p2) = false")
	}
	o := Overall()
	if o.CacheObservedRequests != 2 || o.CacheReadInputTokens != 2300 {
		t.Fatalf("after reset: observed=%d reads=%d, want 2/2300", o.CacheObservedRequests, o.CacheReadInputTokens)
	}
	if want := 2300.0 * 100 / (1000 + 3000); o.CacheHitRate < want-0.001 || o.CacheHitRate > want+0.001 {
		t.Fatalf("hit rate = %v, want ~%v", o.CacheHitRate, want)
	}
}

func TestCachePersistRoundTrip(t *testing.T) {
	reset(t)
	Record(usageEvent(1000, 10, ptrOf(800), ptrOf(40)))
	Record(usageEvent(500, 5, nil, nil))

	path := filepath.Join(t.TempDir(), "metrics.json")
	if err := Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	s = newStore()
	if err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	o := Overall()
	if o.CacheReadInputTokens != 800 || o.CacheCreationInputTokens != 40 || o.CacheObservedRequests != 1 {
		t.Fatalf("reloaded cache totals = %+v", o)
	}
	if want := 800.0 * 100 / 1000.0; o.CacheHitRate < want-0.001 || o.CacheHitRate > want+0.001 {
		t.Fatalf("reloaded hit rate = %v, want %v (denominator preserved across restart)", o.CacheHitRate, want)
	}
}

func TestCacheFieldsAbsentInOldPersistedFile(t *testing.T) {
	reset(t)
	// A snapshot written before cache telemetry existed must load cleanly and
	// read as "unknown" hit rate, not a fabricated 0%.
	old := `{"overall":{"requests":3,"success":3,"inputTokens":50000,"outputTokens":2000},
		"byProvider":{"p-old":{"requests":3,"success":3,"inputTokens":50000,"outputTokens":2000,"name":"old"}}}`
	path := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	o := Overall()
	if o.Requests != 3 || o.InputTokens != 50000 {
		t.Fatalf("legacy totals = %+v", o)
	}
	if o.CacheHitRate != -1 || o.CacheObservedRequests != 0 {
		t.Fatalf("legacy cache = rate %v observed %d, want -1/0", o.CacheHitRate, o.CacheObservedRequests)
	}
}

func TestEventJSONBackwardCompatible(t *testing.T) {
	// Old consumers see no "usage" key on events without a breakdown, and a
	// pointer field that is absent stays distinct from an explicit zero.
	e := ev("p1", true, 200, 5)
	e.InputTokens = 100
	e.OutputTokens = 10
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "usage") {
		t.Fatalf("no-usage event serialized %s, want no usage key", b)
	}
	// Explicit zero cache survives the round trip as an explicit zero.
	e.Usage = &EventUsage{CacheReadInputTokens: ptrOf(0)}
	b, _ = json.Marshal(e)
	var back struct {
		Usage *EventUsage `json:"usage"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Usage == nil || back.Usage.CacheReadInputTokens == nil || *back.Usage.CacheReadInputTokens != 0 {
		t.Fatalf("explicit zero did not survive: %s", b)
	}
}

func TestUsageTotalsPresenceTriState(t *testing.T) {
	reset(t)
	// A stream that carried its input side but died before the final output
	// total: IN known, OUT unknown. The canonical pointers must keep that
	// distinction across the JSON round trip — unknown is not zero.
	e := ev("p1", true, 200, 5)
	e.InputTokens = 5079
	e.Usage = &EventUsage{
		InputTokens:          ptrOf(5079),
		CacheReadInputTokens: ptrOf(2400),
		Source:               UsageSourceUpstream,
		Protocol:             "anthropic",
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Usage *EventUsage `json:"usage"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Usage == nil {
		t.Fatal("usage lost")
	}
	if back.Usage.InputTokens == nil || *back.Usage.InputTokens != 5079 {
		t.Fatalf("canonical input = %v, want 5079", back.Usage.InputTokens)
	}
	if back.Usage.OutputTokens != nil {
		t.Fatalf("canonical output = %v, want nil (never reported)", back.Usage.OutputTokens)
	}
	if strings.Contains(string(b), `"outputTokens":0`) {
		t.Fatalf("unknown output serialized as zero: %s", b)
	}
}

func TestCachePopulationExplicitZeroInUnknownOut(t *testing.T) {
	reset(t)
	// The exact audit scenario: input known with NO cache opinion stays out of
	// the ratio's population; an explicit cache zero stays in (as a real
	// observation); the reporting event contributes both numerator and
	// denominator. 500/2000 = 25%, coverage 2 of 3.
	Record(usageEvent(1000, 5, nil, nil))        // A: input known, cache unknown
	Record(usageEvent(1000, 5, ptrOf(0), nil))   // B: cache explicitly zero
	Record(usageEvent(1000, 5, ptrOf(500), nil)) // C: cache 500

	o := Overall()
	if o.CacheReadInputTokens != 500 {
		t.Fatalf("reads = %d, want 500", o.CacheReadInputTokens)
	}
	if o.CacheObservedRequests != 2 {
		t.Fatalf("observed = %d, want 2 (unknown excluded, zero included)", o.CacheObservedRequests)
	}
	if want := 500.0 * 100 / 2000.0; o.CacheHitRate < want-0.001 || o.CacheHitRate > want+0.001 {
		t.Fatalf("hit rate = %v, want %v", o.CacheHitRate, want)
	}
}

func TestWindowedCacheAggregation(t *testing.T) {
	reset(t)
	// Two cache-reporting events in the current minute: weighted over the
	// population, 1000/4000 = 25% — never the 50% request average, and never
	// the lifetime figure of a different window.
	Record(usageEvent(1000, 10, ptrOf(1000), nil))
	Record(usageEvent(3000, 30, ptrOf(0), nil))
	// A third event that reports no cache must not enter the population.
	Record(usageEvent(7000, 70, nil, nil))

	w := ProviderStatsWindow(1)
	if len(w) != 1 {
		t.Fatalf("want 1 provider, got %d", len(w))
	}
	if w[0].CacheObservedRequests != 2 {
		t.Fatalf("windowed observed = %d, want 2", w[0].CacheObservedRequests)
	}
	if w[0].CacheReadInputTokens != 1000 {
		t.Fatalf("windowed reads = %d, want 1000", w[0].CacheReadInputTokens)
	}
	if want := 1000.0 * 100 / 4000.0; w[0].CacheHitRate < want-0.001 || w[0].CacheHitRate > want+0.001 {
		t.Fatalf("windowed hit rate = %v, want %v (population-weighted)", w[0].CacheHitRate, want)
	}

	d, ok := ProviderDetailFor("p1", 60)
	if !ok || d.CacheHitRate != w[0].CacheHitRate {
		t.Fatalf("detail window cache = %+v ok=%v, want %v", d.CacheHitRate, ok, w[0].CacheHitRate)
	}
}

func TestWindowWithoutCacheTelemetryIsUnknown(t *testing.T) {
	reset(t)
	// Traffic in the window, but nothing cache-reporting: the windowed rate is
	// unknown (-1), never a fabricated 0%.
	Record(usageEvent(1000, 10, nil, nil))

	w := ProviderStatsWindow(1)
	if len(w) != 1 || w[0].Requests != 1 {
		t.Fatalf("window traffic missing: %+v", w)
	}
	if w[0].CacheHitRate != -1 || w[0].CacheObservedRequests != 0 {
		t.Fatalf("windowed cache = %v/%d, want -1/0", w[0].CacheHitRate, w[0].CacheObservedRequests)
	}
}

func TestPersistRoundTripsBucketCache(t *testing.T) {
	reset(t)
	// An old event lands in the hourly rollup (per-minute buckets are
	// memory-only), so the windowed cache figures must survive a restart.
	e := usageEvent(1000, 10, ptrOf(800), ptrOf(40))
	e.TimeMs = nowMs() - 2*3600000
	Record(e)

	path := filepath.Join(t.TempDir(), "metrics.json")
	if err := Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	s = newStore()
	if err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	w := ProviderStatsWindow(24)
	if len(w) != 1 {
		t.Fatalf("want 1 provider after reload, got %d", len(w))
	}
	if w[0].CacheReadInputTokens != 800 || w[0].CacheCreationInputTokens != 40 || w[0].CacheObservedRequests != 1 {
		t.Fatalf("reloaded bucket cache = %+v", w[0])
	}
	if want := 80.0; w[0].CacheHitRate < want-0.01 || w[0].CacheHitRate > want+0.01 {
		t.Fatalf("reloaded windowed hit rate = %v, want %v", w[0].CacheHitRate, want)
	}
}

func TestUsageTotalsExplicitZeroSerializes(t *testing.T) {
	reset(t)
	// The other half of tri-state: an upstream-reported zero must reach the
	// activity table as an explicit 0, not collapse into the unknown "—".
	// omitempty drops only nil pointers, so a pointed-at zero serializes.
	e := ev("p1", true, 200, 5)
	e.Usage = &EventUsage{
		InputTokens:  ptrOf(0),
		OutputTokens: ptrOf(0),
		Source:       UsageSourceUpstream,
		Protocol:     "openai",
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Usage *EventUsage `json:"usage"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Usage.InputTokens == nil || *back.Usage.InputTokens != 0 || back.Usage.OutputTokens == nil || *back.Usage.OutputTokens != 0 {
		t.Fatalf("round trip lost the explicit zeros: %s", b)
	}
	if !strings.Contains(string(b), `"inputTokens":0`) || !strings.Contains(string(b), `"outputTokens":0`) {
		t.Fatalf("explicit zero dropped from serialization: %s", b)
	}
}

// ProviderStatsRange must scope to an arbitrary window the way
// ProviderStatsWindow scopes to the hour presets — the custom-range picker is
// inert otherwise.
func TestProviderStatsRangeScopesToWindow(t *testing.T) {
	reset(t)

	recent := ev("p1", true, 200, 200)
	recent.CostUSD = 0.5
	Record(recent)

	mid := ev("p1", true, 200, 200)
	mid.TimeMs = nowMs() - 5*3600000
	mid.CostUSD = 4
	Record(mid)

	now := nowMs()
	oneHour := ProviderStatsRange(now-3600000, 0)
	if len(oneHour) != 1 || oneHour[0].Requests != 1 {
		t.Fatalf("1h range requests = %+v, want 1", oneHour)
	}
	if oneHour[0].CostUSD != 0.5 {
		t.Fatalf("1h range cost = %v, want 0.5", oneHour[0].CostUSD)
	}

	day := ProviderStatsRange(now-24*3600000, 0)
	if len(day) != 1 || day[0].Requests != 2 || day[0].CostUSD != 4.5 {
		t.Fatalf("24h range = %d requests cost %v, want 2 / 4.5", day[0].Requests, day[0].CostUSD)
	}

	// A frozen window that ends in the past reads only what fell inside it,
	// including the trailing edge (to=0 means now, an explicit to is honored).
	oldOnly := ProviderStatsRange(now-6*3600000, now-4*3600000)
	if len(oldOnly) != 1 || oldOnly[0].Requests != 1 || oldOnly[0].CostUSD != 4 {
		t.Fatalf("closed range = %d requests cost %v, want 1 / 4", oldOnly[0].Requests, oldOnly[0].CostUSD)
	}
}

// A window reaching into the hourly rollups counts part-cut hour buckets in
// full — the documented "rounded out to whole hours" overcount — while a
// window starting exactly on an hour boundary excludes the bucket that ends
// at it.
func TestProviderStatsRangeCountsPartialHourBucketsFully(t *testing.T) {
	reset(t)
	nowHour := nowMs() / 3600000

	inBucket := ev("p1", true, 200, 100)
	inBucket.TimeMs = (nowHour - 4) * 3600000
	Record(inBucket)

	// Window starts half an hour after the event, inside the event's hour: the
	// bucket overlaps the window start, so the request counts.
	midStart := ProviderStatsRange((nowHour-4)*3600000+30*60000, 0)
	if len(midStart) != 1 || midStart[0].Requests != 1 {
		t.Fatalf("overlap-start range = %d requests, want 1 (edge hour counts in full)", midStart[0].Requests)
	}

	// Window starts exactly at the next hour boundary: the event's bucket ends
	// there, so it does not overlap.
	nextHour := ProviderStatsRange((nowHour-3)*3600000, 0)
	if len(nextHour) != 1 || nextHour[0].Requests != 0 {
		t.Fatalf("boundary-start range = %d requests, want 0", nextHour[0].Requests)
	}
}

// A window entirely inside the per-minute coverage reads minute buckets, so
// its edges are exact rather than padded out to the hour.
func TestProviderStatsRangeUsesMinuteBucketsInsideCoverage(t *testing.T) {
	reset(t)
	older := ev("p1", true, 200, 100)
	older.TimeMs = nowMs() - 95*60000
	Record(older)
	inner := ev("p1", true, 200, 100)
	inner.TimeMs = nowMs() - 89*60000
	Record(inner)

	got := ProviderStatsRange(nowMs()-90*60000, 0)
	if len(got) != 1 || got[0].Requests != 1 {
		t.Fatalf("minute-coverage range = %d requests, want 1", got[0].Requests)
	}
}

// A window reaching past the 30-day hourly retention must clamp forward
// instead of panicking or pretending the pruned data exists.
func TestProviderStatsRangeClampsToRetention(t *testing.T) {
	reset(t)
	ancient := ev("p1", true, 200, 100)
	ancient.TimeMs = nowMs() - 40*24*3600000
	Record(ancient)

	got := ProviderStatsRange(nowMs()-90*24*3600000, 0)
	if len(got) != 1 || got[0].Requests != 0 {
		t.Fatalf("retention-clamped range = %d requests, want 0", got[0].Requests)
	}
}

// The custom-range drill-down mirrors the windowed headline numbers, echoes
// the effective rounded-out coverage, and keeps the breakdown all-time.
func TestProviderDetailRangeScopesAndEchoesWindow(t *testing.T) {
	reset(t)
	old := ev("p1", true, 200, 1000)
	old.TimeMs = nowMs() - 5*3600000
	Record(old)
	recent := ev("p1", true, 200, 300)
	Record(recent)

	now := nowMs()
	d, ok := ProviderDetailRange("p1", now-3600000, 0)
	if !ok || d.Requests != 1 {
		t.Fatalf("1h detail requests = %d ok=%v, want 1", d.Requests, ok)
	}
	if d.AvgLatencyMs != 300 {
		t.Fatalf("1h detail avg = %d, want 300", d.AvgLatencyMs)
	}
	if d.WindowFrom == 0 || d.WindowTo < now || d.WindowTo > now+2*60000 {
		t.Fatalf("detail window echo = [%d, %d], outside plausible coverage", d.WindowFrom, d.WindowTo)
	}
	total := int64(0)
	for _, m := range d.Models {
		total += m.Requests
	}
	if total != 2 {
		t.Fatalf("detail models total = %d, want 2 (breakdown stays all-time)", total)
	}
}
