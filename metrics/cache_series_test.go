package metrics

import (
	"testing"
	"time"
)

func i64(n int64) *int64 { return &n }

// cacheEv builds a successful upstream-reported event. read/write nil mean the
// upstream did not report that figure.
func cacheEv(provider string, at int64, input int64, read, write *int64) Event {
	return Event{
		TimeMs: at, ProviderID: provider, ProviderName: "P-" + provider, ClientModel: "client-m", TargetModel: "target-m",
		RequestID: "req", Ok: true, Status: 200,
		Usage: &EventUsage{InputTokens: i64(input), CacheReadInputTokens: read, CacheCreationInputTokens: write, Source: UsageSourceUpstream},
	}
}

const minute = int64(60_000)

func TestCacheSeriesArithmeticAndTiers(t *testing.T) {
	reset(t)
	base := int64(10_000) * minute // aligned to every bucket width
	// 303.3k read, 3.4k written to the 1h tier, 2 uncached: the common warm turn.
	hit := cacheEv("p1", base+5_000, 306_702, i64(303_300), i64(3_400))
	hit.Usage.CacheCreation5mTokens, hit.Usage.CacheCreation1hTokens = i64(0), i64(3_400)
	// A write whose upstream gave no tier split: the whole write is untiered, never guessed.
	untiered := cacheEv("p1", base+6_000, 1_000, i64(0), i64(100))
	// Tiers that exceed the reported total are inconsistent: keep them, claim no remainder.
	odd := cacheEv("p1", base+7_000, 5_000, i64(0), i64(100))
	odd.Usage.CacheCreation5mTokens, odd.Usage.CacheCreation1hTokens = i64(80), i64(80)
	// Read and write larger than the input total: uncached clamps at zero.
	over := cacheEv("p1", base+8_000, 100, i64(90), i64(40))
	for _, e := range []Event{hit, untiered, odd, over} {
		Record(e)
	}

	r := CacheSeries(base, base+30*minute, nil, 0)
	tot := r.Totals
	if r.Eligible != 4 || tot.Requests != 4 {
		t.Fatalf("eligible = %d / %d, want 4", r.Eligible, tot.Requests)
	}
	if tot.Read != 303_300+90 || tot.Write != 3_400+100+100+40 {
		t.Fatalf("read/write = %d/%d", tot.Read, tot.Write)
	}
	if tot.Write5m != 80 || tot.Write1h != 3_400+80 {
		t.Fatalf("tiers = %d/%d", tot.Write5m, tot.Write1h)
	}
	// untiered: hit 0, untiered 100, odd clamps to 0, over 40
	if tot.WriteUntiered != 140 {
		t.Fatalf("untiered = %d, want 140", tot.WriteUntiered)
	}
	// uncached: hit 2, untiered 900, odd 4_900, over clamps to 0
	if tot.Uncached != 2+900+4_900 {
		t.Fatalf("uncached = %d", tot.Uncached)
	}
	if tot.MaxWrite != 3_400 {
		t.Fatalf("maxWrite = %d", tot.MaxWrite)
	}
}

func TestCacheSeriesEligibilityAndExclusions(t *testing.T) {
	reset(t)
	at := int64(5_000) * minute
	Record(cacheEv("p1", at, 1_000, i64(900), nil))     // read-only report: eligible
	Record(cacheEv("p1", at+1, 1_000, nil, i64(500)))   // write-only report: eligible, not in the reuse denominator
	Record(cacheEv("p1", at+2, 1_000, nil, nil))        // ok, no cache figure: not reported
	noInput := cacheEv("p1", at+3, 1_000, i64(10), nil) // cache figure but no input total: not reported
	noInput.Usage.InputTokens = nil
	Record(noInput)
	failed := cacheEv("p1", at+4, 1_000, nil, nil) // failure with no usage: silent, not a gap
	failed.Ok, failed.Usage = false, nil
	Record(failed)
	est := cacheEv("p1", at+5, 1_000, i64(900), nil)
	est.Usage.Source = UsageSourceEstimated
	Record(est)
	pool := cacheEv(KiroPoolID, at+6, 1_000, i64(900), i64(50)) // synthesised cache: never counted
	Record(pool)
	noUsage := cacheEv("p1", at+7, 1_000, nil, nil)
	noUsage.Usage = nil
	Record(noUsage)                                             // ok with no usage at all: not reported
	Record(cacheEv("p1", at-1, 1_000, i64(900), nil))           // just before the window
	Record(cacheEv("p1", at+10*minute+1, 1_000, i64(900), nil)) // just after

	r := CacheSeries(at, at+10*minute, nil, 0)
	if r.Eligible != 2 {
		t.Fatalf("eligible = %d, want 2", r.Eligible)
	}
	if r.Excluded.Pool != 1 || r.Excluded.Estimated != 1 || r.Excluded.NotReported != 3 {
		t.Fatalf("excluded = %+v, want pool 1, estimated 1, notReported 3", r.Excluded)
	}
	if r.Totals.ReadInput != 1_000 || r.Totals.Read != 900 {
		t.Fatalf("reuse ratio inputs = %d/%d: a write-only report must not enter the denominator", r.Totals.Read, r.Totals.ReadInput)
	}
	// Provider filter keeps the pool out and other providers out.
	Record(cacheEv("p2", at+20, 2_000, i64(100), nil))
	if got := CacheSeries(at, at+10*minute, []string{"p2"}, 0); got.Eligible != 1 || got.Excluded.Pool != 0 {
		t.Fatalf("p2 filter: %+v", got)
	}
	if got := CacheSeries(at, at+10*minute, []string{"p1", "p2"}, 0); got.Eligible != 3 {
		t.Fatalf("p1+p2 filter: eligible %d, want 3", got.Eligible)
	}
}

func TestCacheSeriesBucketsAndBounds(t *testing.T) {
	reset(t)
	from := int64(20_000) * minute
	Record(cacheEv("p1", from, 100, i64(50), nil))           // first instant of the window
	Record(cacheEv("p1", from+minute-1, 100, i64(50), nil))  // last instant of bucket 0
	Record(cacheEv("p1", from+minute, 100, i64(50), nil))    // first instant of bucket 1
	Record(cacheEv("p1", from+60*minute, 100, i64(50), nil)) // exactly toMs: included

	r := CacheSeries(from, from+60*minute, nil, 0)
	if r.BucketMs != minute {
		t.Fatalf("bucket = %d, want one minute for a 1h window", r.BucketMs)
	}
	if len(r.Buckets) != 61 || r.Buckets[0].StartMs != from {
		t.Fatalf("buckets = %d starting %d", len(r.Buckets), r.Buckets[0].StartMs)
	}
	if r.Buckets[0].Requests != 2 || r.Buckets[1].Requests != 1 || r.Buckets[60].Requests != 1 {
		t.Fatalf("assignment wrong: %d %d %d", r.Buckets[0].Requests, r.Buckets[1].Requests, r.Buckets[60].Requests)
	}
	// Dense: quiet buckets exist and are zero rather than missing.
	if r.Buckets[30].StartMs != from+30*minute || r.Buckets[30].Requests != 0 {
		t.Fatalf("quiet bucket: %+v", r.Buckets[30])
	}
	for _, c := range []struct {
		span time.Duration
		want int64
	}{{time.Hour, minute}, {6 * time.Hour, 5 * minute}, {24 * time.Hour, 15 * minute}, {72 * time.Hour, 60 * minute}} {
		if got := cacheBucketWidth(c.span.Milliseconds()); got != c.want {
			t.Fatalf("width(%v) = %d, want %d", c.span, got, c.want)
		}
	}
	// A careless window is clamped instead of allocating millions of buckets.
	huge := CacheSeries(1, 1+10*365*24*60*minute, nil, 0)
	if len(huge.Buckets) > 800 {
		t.Fatalf("unclamped window produced %d buckets", len(huge.Buckets))
	}
}

func TestCacheSeriesBigWritesAndTopWrites(t *testing.T) {
	reset(t)
	at := int64(30_000) * minute
	for i := int64(0); i < 12; i++ {
		e := cacheEv("p1", at+i*1_000, 500_000, i64(0), i64(10_000*(i+1)))
		e.RequestID = "r" + string(rune('a'+i))
		Record(e)
	}
	tie := cacheEv("p1", at+99_000, 500_000, i64(0), i64(120_000)) // ties the largest, but is newer
	tie.RequestID = "tie"
	Record(tie)
	r := CacheSeries(at, at+10*minute, nil, 0)
	if len(r.TopWrites) != topWritesKept {
		t.Fatalf("top writes = %d, want %d", len(r.TopWrites), topWritesKept)
	}
	if r.TopWrites[0].RequestID != "tie" || r.TopWrites[0].Write != 120_000 || r.TopWrites[1].Write != 120_000 {
		t.Fatalf("order/tie: %+v %+v", r.TopWrites[0], r.TopWrites[1])
	}
	for i := 1; i < len(r.TopWrites); i++ {
		if r.TopWrites[i].Write > r.TopWrites[i-1].Write {
			t.Fatalf("not descending at %d", i)
		}
	}
	// Default threshold is 100k: writes of 100k, 110k, 120k and the tie.
	if r.Totals.BigWrites != 4 || r.BigWriteMin != DefaultBigWriteTokens {
		t.Fatalf("bigWrites = %d (min %d), want 4", r.Totals.BigWrites, r.BigWriteMin)
	}
	if got := CacheSeries(at, at+10*minute, nil, 50_000).Totals.BigWrites; got != 9 {
		t.Fatalf("bigWrites at 50k = %d, want 9", got)
	}
	// A window with nothing in it still returns empty lists, not nulls.
	empty := CacheSeries(at+time.Hour.Milliseconds(), at+2*time.Hour.Milliseconds(), nil, 0)
	if empty.TopWrites == nil || empty.Buckets == nil || empty.Eligible != 0 {
		t.Fatalf("empty window: %+v", empty)
	}
}

func TestCacheSeriesCoverage(t *testing.T) {
	reset(t)
	now := nowMs()
	if got := CacheSeries(now-minute, now, nil, 0).CoverageFromMs; got != Retention().MemorySinceMs {
		t.Fatalf("coverage = %d, want memorySince while the ring has room", got)
	}
	for i := 0; i < EventRingCapacity+5; i++ {
		e := cacheEv("p1", 1_000+int64(i), 100, i64(1), nil)
		Record(e)
	}
	if got := CacheSeries(0, 1_000_000, nil, 0).CoverageFromMs; got != 1_005 {
		t.Fatalf("coverage after wrap = %d, want 1005 (oldest survivor)", got)
	}
}

func TestEventFilterMinCacheWrite(t *testing.T) {
	reset(t)
	at := nowMs()
	Record(cacheEv("p1", at, 1_000, i64(0), i64(5_000)))
	Record(cacheEv("p1", at+1, 1_000, i64(0), i64(50)))
	Record(cacheEv("p1", at+2, 1_000, i64(900), nil)) // no write reported
	noUsage := cacheEv("p1", at+3, 1_000, nil, nil)
	noUsage.Usage = nil
	Record(noUsage)
	got, total := Events(EventFilter{MinCacheWrite: 1_000})
	if total != 1 || len(got) != 1 || *got[0].Usage.CacheCreationInputTokens != 5_000 {
		t.Fatalf("minCacheWrite 1000: total %d %+v", total, got)
	}
	if _, total := Events(EventFilter{}); total != 4 {
		t.Fatalf("no filter: %d, want 4", total)
	}
}
