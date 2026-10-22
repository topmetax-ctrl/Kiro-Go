package metrics

import (
	"encoding/json"
	"strings"
	"testing"
)

// An event that carries none of the cache-observability fields must serialize
// exactly as it did before they existed, so exports and the SSE stream stay
// byte-compatible for consumers that know nothing about them.
func TestEventWithoutCacheFieldsKeepsItsShape(t *testing.T) {
	in, out := int64(10), int64(2)
	raw, err := json.Marshal(Event{
		TimeMs: 1, ClientModel: "m", Status: 200, Ok: true,
		Usage: &EventUsage{InputTokens: &in, OutputTokens: &out, Source: UsageSourceUpstream},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"cache"`, `"upstreamModel"`, `"cacheCreation5mTokens"`, `"cacheCreation1hTokens"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("unused field %s appears in %s", key, raw)
		}
	}
}

func TestEventCacheFieldsRoundTrip(t *testing.T) {
	five, hour := int64(0), int64(1200)
	want := Event{
		TimeMs:        2,
		UpstreamModel: "claude-sonnet-5-5",
		Usage:         &EventUsage{CacheCreation5mTokens: &five, CacheCreation1hTokens: &hour},
		Cache: &CacheTrace{
			Conv: "aaaaaaaaaaaa", Tools: "bbbbbbbbbbbb", System: "cccccccccccc", Settings: "dddddddddddd",
			Msgs: 3, Prefix: []string{"2:eeeeeeeeeeee", "3:ffffffffffff"}, Marks: []string{"m2:1h"}, Policy: "off",
		},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Event
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.UpstreamModel != want.UpstreamModel || got.Cache == nil || got.Cache.Msgs != 3 ||
		strings.Join(got.Cache.Prefix, ",") != "2:eeeeeeeeeeee,3:ffffffffffff" || got.Cache.Policy != "off" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	// An explicit zero tier survives: it means "reported none", not "unknown".
	if got.Usage.CacheCreation5mTokens == nil || *got.Usage.CacheCreation5mTokens != 0 {
		t.Fatalf("explicit zero lost: %+v", got.Usage)
	}
}

// The ring stores events by value; a recorded trace must come back intact and
// must not change the aggregate counters.
func TestRecordedTraceDoesNotAffectCounters(t *testing.T) {
	Reset()
	tr := &CacheTrace{Conv: "aaaaaaaaaaaa", Msgs: 1}
	Record(Event{ProviderID: "p", ProviderName: "P", Ok: true, Status: 200, InputTokens: 5, Cache: tr})
	Record(Event{ProviderID: "p", ProviderName: "P", Ok: true, Status: 200, InputTokens: 5})
	evs, total := Events(EventFilter{ProviderID: "p"})
	if total != 2 || len(evs) != 2 {
		t.Fatalf("events = %d/%d", total, len(evs))
	}
	var traced int
	for _, e := range evs {
		if e.Cache != nil {
			traced++
			if e.Cache.Conv != "aaaaaaaaaaaa" {
				t.Fatalf("trace changed in the ring: %+v", e.Cache)
			}
		}
	}
	if traced != 1 {
		t.Fatalf("traced events = %d, want 1", traced)
	}
}
