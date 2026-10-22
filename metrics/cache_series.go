package metrics

import (
	"sort"
	"time"
)

// DefaultBigWriteTokens is the cache-write size above which a single request is
// counted as a large write. It is a reporting threshold, not a limit: nothing
// is blocked or changed at this size.
const DefaultBigWriteTokens = 100_000

// topWritesKept bounds the largest-writes list so the response size does not
// depend on traffic.
const topWritesKept = 10

// maxCacheWindow caps the window so a careless from/to cannot ask for millions
// of buckets; the ring holds far less history than this anyway.
const maxCacheWindow = 30 * 24 * time.Hour

// CacheBucket is the cache accounting of the eligible events in one time
// bucket. Every token figure is a plain sum over those events.
//
// Input is the request's total input (cache reads and writes included, as the
// gateway normalises it), so Read + Write + Uncached == Input for each event
// whose parts do not exceed the total.
type CacheBucket struct {
	StartMs  int64 `json:"startMs"`
	Requests int64 `json:"requests"`
	Input    int64 `json:"input"`
	Read     int64 `json:"read"`
	Write    int64 `json:"write"`
	// Write5m and Write1h are the upstream's own split of Write by TTL tier.
	// WriteUntiered is the part of Write for which no split was reported; it is
	// never guessed into a tier.
	Write5m       int64 `json:"write5m"`
	Write1h       int64 `json:"write1h"`
	WriteUntiered int64 `json:"writeUntiered"`
	Uncached      int64 `json:"uncached"`
	// ReadInput is the input total of the events that reported a cache-read
	// figure, the denominator of a token-weighted reuse ratio (Read / ReadInput).
	// An event that reported only a write never dilutes it.
	ReadInput int64 `json:"readInput"`
	BigWrites int64 `json:"bigWrites"`
	MaxWrite  int64 `json:"maxWrite"`
}

func (b *CacheBucket) add(o CacheBucket) {
	b.Requests += o.Requests
	b.Input += o.Input
	b.Read += o.Read
	b.Write += o.Write
	b.Write5m += o.Write5m
	b.Write1h += o.Write1h
	b.WriteUntiered += o.WriteUntiered
	b.Uncached += o.Uncached
	b.ReadInput += o.ReadInput
	b.BigWrites += o.BigWrites
	if o.MaxWrite > b.MaxWrite {
		b.MaxWrite = o.MaxWrite
	}
}

// CacheWrite is one request that wrote to the upstream cache, listed so an
// operator can open the exact request behind a spike.
type CacheWrite struct {
	TimeMs       int64  `json:"time"`
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName,omitempty"`
	Model        string `json:"model,omitempty"`
	RequestID    string `json:"requestId,omitempty"`
	Input        int64  `json:"input"`
	Read         int64  `json:"read"`
	Write        int64  `json:"write"`
	// Write5m and Write1h are nil when the upstream sent no tier split.
	Write5m *int64 `json:"write5m,omitempty"`
	Write1h *int64 `json:"write1h,omitempty"`
}

// CacheExcluded counts events inside the window that the series does not
// describe, by reason, so the UI can say what the figures leave out.
type CacheExcluded struct {
	// Pool: Kiro account-pool traffic. Its cache figures are synthesised for the
	// client, not reported by an upstream, so they never describe real caching.
	Pool int64 `json:"pool"`
	// Estimated: usage the gateway estimated rather than the upstream reported.
	Estimated int64 `json:"estimated"`
	// NotReported: successful requests whose upstream gave no cache figure, or
	// no input total to relate it to.
	NotReported int64 `json:"notReported"`
}

// CacheSeriesResult is the response of CacheSeries.
type CacheSeriesResult struct {
	FromMs   int64 `json:"from"`
	ToMs     int64 `json:"to"`
	BucketMs int64 `json:"bucketMs"`
	// CoverageFromMs is where the event log actually starts (see
	// RetentionStat.EventLogSinceMs): a window that begins earlier is only
	// partly covered, whatever the traffic was.
	CoverageFromMs int64         `json:"coverageFrom"`
	BigWriteMin    int64         `json:"bigWriteMin"`
	Buckets        []CacheBucket `json:"buckets"`
	Totals         CacheBucket   `json:"totals"`
	TopWrites      []CacheWrite  `json:"topWrites"`
	Eligible       int64         `json:"eligible"`
	Excluded       CacheExcluded `json:"excluded"`
}

// cacheBucketWidth picks a bucket width that keeps a window to at most a few
// hundred buckets.
func cacheBucketWidth(spanMs int64) int64 {
	switch {
	case spanMs <= time.Hour.Milliseconds():
		return time.Minute.Milliseconds()
	case spanMs <= 6*time.Hour.Milliseconds():
		return 5 * time.Minute.Milliseconds()
	case spanMs <= 24*time.Hour.Milliseconds():
		return 15 * time.Minute.Milliseconds()
	default:
		return time.Hour.Milliseconds()
	}
}

// cacheEventParts reads the cache accounting of one event. ok is false when the
// event cannot be described: the upstream reported no cache figure, or no input
// total to relate it to. Missing telemetry is never turned into a zero.
func cacheEventParts(ev Event) (b CacheBucket, readReported bool, ok bool) {
	u := ev.Usage
	if u == nil || u.InputTokens == nil || *u.InputTokens <= 0 {
		return b, false, false
	}
	if u.CacheReadInputTokens == nil && u.CacheCreationInputTokens == nil {
		return b, false, false
	}
	input := *u.InputTokens
	var read, write int64
	if u.CacheReadInputTokens != nil && *u.CacheReadInputTokens > 0 {
		read = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil && *u.CacheCreationInputTokens > 0 {
		write = *u.CacheCreationInputTokens
	}
	var w5, w1 int64
	if u.CacheCreation5mTokens != nil && *u.CacheCreation5mTokens > 0 {
		w5 = *u.CacheCreation5mTokens
	}
	if u.CacheCreation1hTokens != nil && *u.CacheCreation1hTokens > 0 {
		w1 = *u.CacheCreation1hTokens
	}
	untiered := write - w5 - w1
	if untiered < 0 {
		// A split that exceeds the reported total is inconsistent upstream data;
		// keep the tiers as reported and claim no untiered remainder.
		untiered = 0
	}
	uncached := input - read - write
	if uncached < 0 {
		uncached = 0
	}
	b = CacheBucket{
		Requests: 1, Input: input, Read: read, Write: write,
		Write5m: w5, Write1h: w1, WriteUntiered: untiered, Uncached: uncached,
		MaxWrite: write,
	}
	if u.CacheReadInputTokens != nil {
		b.ReadInput = input
	}
	return b, u.CacheReadInputTokens != nil, true
}

// CacheSeries aggregates the cache accounting of the events in the retained
// event log between fromMs and toMs, optionally for one provider. It reads the
// event ring, not the persisted rollups, because only events carry the 5m/1h
// split; the result is therefore bounded by the ring and empty before a restart
// (see CoverageFromMs).
//
// Eligible events are those whose upstream reported a cache figure and an input
// total, excluding the Kiro pool and estimated usage. bigWriteMin <= 0 uses
// DefaultBigWriteTokens.
func CacheSeries(fromMs, toMs int64, providerID string, bigWriteMin int64) CacheSeriesResult {
	if bigWriteMin <= 0 {
		bigWriteMin = DefaultBigWriteTokens
	}
	if toMs < fromMs {
		toMs = fromMs
	}
	if toMs-fromMs > maxCacheWindow.Milliseconds() {
		fromMs = toMs - maxCacheWindow.Milliseconds()
	}
	width := cacheBucketWidth(toMs - fromMs)
	first := fromMs / width * width
	n := int((toMs/width*width-first)/width) + 1
	res := CacheSeriesResult{
		FromMs: fromMs, ToMs: toMs, BucketMs: width, BigWriteMin: bigWriteMin,
		Buckets:   make([]CacheBucket, n),
		TopWrites: []CacheWrite{},
	}
	for i := range res.Buckets {
		res.Buckets[i].StartMs = first + int64(i)*width
	}

	s.mu.Lock()
	all := s.snapshotEventsLocked()
	res.CoverageFromMs = s.sinceMs
	if s.full {
		res.CoverageFromMs = ringOldestLocked()
	}
	s.mu.Unlock()

	var writes []CacheWrite
	for _, ev := range all {
		if ev.TimeMs < fromMs || ev.TimeMs > toMs {
			continue
		}
		if providerID != "" && ev.ProviderID != providerID {
			continue
		}
		if ev.ProviderID == KiroPoolID {
			res.Excluded.Pool++
			continue
		}
		if ev.Usage != nil && ev.Usage.Source == UsageSourceEstimated {
			res.Excluded.Estimated++
			continue
		}
		parts, _, ok := cacheEventParts(ev)
		if !ok {
			// Only a request that succeeded was expected to report; a failure with
			// no usage is not a reporting gap.
			if ev.Ok {
				res.Excluded.NotReported++
			}
			continue
		}
		if parts.Write >= bigWriteMin {
			parts.BigWrites = 1
		}
		res.Eligible++
		res.Buckets[int((ev.TimeMs/width*width-first)/width)].add(parts)
		res.Totals.add(parts)
		if parts.Write > 0 {
			w := CacheWrite{
				TimeMs: ev.TimeMs, ProviderID: ev.ProviderID, ProviderName: ev.ProviderName,
				Model: ev.TargetModel, RequestID: ev.RequestID,
				Input: parts.Input, Read: parts.Read, Write: parts.Write,
				Write5m: ev.Usage.CacheCreation5mTokens, Write1h: ev.Usage.CacheCreation1hTokens,
			}
			if w.Model == "" {
				w.Model = ev.ClientModel
			}
			writes = append(writes, w)
		}
	}
	sort.SliceStable(writes, func(i, j int) bool {
		if writes[i].Write != writes[j].Write {
			return writes[i].Write > writes[j].Write
		}
		return writes[i].TimeMs > writes[j].TimeMs
	})
	if len(writes) > topWritesKept {
		writes = writes[:topWritesKept]
	}
	if writes != nil {
		res.TopWrites = writes
	}
	return res
}

// ringOldestLocked is the time of the oldest retained event, 0 when empty;
// s.mu must be held. Callers may stamp TimeMs themselves, so ring order is not a
// promise of time order: it scans for the true minimum, bounded by the ring
// capacity.
func ringOldestLocked() int64 {
	size := s.next
	if s.full {
		size = EventRingCapacity
	}
	var oldest int64
	for i := 0; i < size; i++ {
		if t := s.ring[i].TimeMs; oldest == 0 || t < oldest {
			oldest = t
		}
	}
	return oldest
}
