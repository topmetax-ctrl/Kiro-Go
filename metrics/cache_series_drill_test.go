package metrics

import "testing"

// ResetProvider rebuilds the ring and may leave it with room, but the events the
// ring overwrote earlier do not come back: the log must still start at the
// oldest survivor, or the missing history would read as zero requests.
func TestLogStartSurvivesResetProvider(t *testing.T) {
	reset(t)
	for i := 0; i < EventRingCapacity+10; i++ {
		Record(cacheEv("p1", 1_000+int64(i), 100, i64(1), nil))
	}
	Record(cacheEv("p2", 50_000, 100, i64(1), nil))
	if !ResetProvider("p2") {
		t.Fatal("p2 should reset")
	}
	if got := CacheSeries(0, 1_000_000, nil, 0).CoverageFromMs; got < 1_010 {
		t.Fatalf("log start = %d after ResetProvider, want the oldest survivor (about 1010), not process start", got)
	}
	if got := Retention().EventLogSinceMs; got < 1_010 {
		t.Fatalf("eventLogSince = %d after ResetProvider", got)
	}
	// A full Reset forgets the loss: nothing was ever dropped from the new log.
	Reset()
	r := Retention()
	if r.EventLogSinceMs != r.MemorySinceMs {
		t.Fatalf("after Reset: eventLogSince %d, memorySince %d", r.EventLogSinceMs, r.MemorySinceMs)
	}
}

// The event filters select exactly the population a cache bar was drawn from, so
// a drill-down shows the requests the chart counted and nothing else.
func TestEventFilterCacheEligibleMatchesTheChart(t *testing.T) {
	reset(t)
	at := int64(70_000) * minute
	counted := cacheEv("p1", at, 1_000, i64(900), nil)
	writer := cacheEv("p2", at+1, 1_000, i64(0), i64(500))
	est := cacheEv("p1", at+2, 1_000, i64(900), nil)
	est.Usage.Source = UsageSourceEstimated
	pool := cacheEv(KiroPoolID, at+3, 1_000, i64(900), i64(500))
	noInput := cacheEv("p1", at+4, 1_000, i64(10), i64(500))
	noInput.Usage.InputTokens = nil
	noFigure := cacheEv("p1", at+5, 1_000, nil, nil)
	failed := cacheEv("p1", at+6, 1_000, nil, nil)
	failed.Ok, failed.Usage = false, nil
	for _, e := range []Event{counted, writer, est, pool, noInput, noFigure, failed} {
		Record(e)
	}
	series := CacheSeries(at, at+10*minute, nil, 0)
	got, total := Events(EventFilter{CacheEligible: true})
	if int64(total) != series.Eligible || total != 2 {
		t.Fatalf("eligible filter = %d, chart = %d; want both 2 (%+v)", total, series.Eligible, got)
	}
	// minCacheWrite uses the same population: the pool's write and the event
	// without an input total are not in the chart, so they are not in the list.
	if _, total := Events(EventFilter{MinCacheWrite: 1}); total != 1 {
		t.Fatalf("minCacheWrite=1 matched %d events, want only the one the chart counted", total)
	}
	if _, total := Events(EventFilter{CacheEligible: true, ProviderIDs: []string{"p1", "p2"}}); total != 2 {
		t.Fatalf("p1+p2: %d", total)
	}
	if _, total := Events(EventFilter{CacheEligible: true, ProviderIDs: []string{"p2"}}); total != 1 {
		t.Fatalf("p2 only: %d", total)
	}
}

// A write-only report has no read figure: the listed write must not claim a 0 read.
func TestTopWriteReadIsNilWhenNotReported(t *testing.T) {
	reset(t)
	at := int64(80_000) * minute
	Record(cacheEv("p1", at, 200_000, nil, i64(150_000)))
	Record(cacheEv("p1", at+1, 200_000, i64(0), i64(120_000)))
	r := CacheSeries(at, at+minute, nil, 0)
	if len(r.TopWrites) != 2 {
		t.Fatalf("top writes: %+v", r.TopWrites)
	}
	if r.TopWrites[0].Read != nil {
		t.Fatalf("write-only report listed a read of %d", *r.TopWrites[0].Read)
	}
	if r.TopWrites[1].Read == nil || *r.TopWrites[1].Read != 0 {
		t.Fatalf("an explicit zero read must stay 0: %+v", r.TopWrites[1].Read)
	}
}
