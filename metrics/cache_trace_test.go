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

// The cache-miss analysis reads the ring a day after the fact, so it has to keep
// the newest EventRingCapacity events and drop only older ones.
func TestRingKeepsTheNewestCapacityEvents(t *testing.T) {
	Reset()
	defer Reset()
	extra := 25
	for i := 1; i <= EventRingCapacity+extra; i++ {
		Record(Event{TimeMs: int64(i), ClientModel: "m", ProviderID: "p", Status: 200, Ok: true})
	}
	got, total := Events(EventFilter{})
	if total != EventRingCapacity || len(got) != EventRingCapacity {
		t.Fatalf("kept %d/%d events, want %d", len(got), total, EventRingCapacity)
	}
	if got[0].TimeMs != int64(EventRingCapacity+extra) || got[len(got)-1].TimeMs != int64(extra+1) {
		t.Fatalf("newest/oldest = %d/%d, want %d/%d", got[0].TimeMs, got[len(got)-1].TimeMs, EventRingCapacity+extra, extra+1)
	}
}

// A trace that names no session and carries no signals must serialize as it did
// before they existed: an export read by an older consumer sees no new keys.
func TestTraceWithoutSessionFieldsKeepsItsShape(t *testing.T) {
	raw, err := json.Marshal(Event{TimeMs: 1, Cache: &CacheTrace{Conv: "aaaaaaaaaaaa", Msgs: 2, Policy: "off"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"sess"`, `"parent"`, `"signals"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("unused field %s appears in %s", key, raw)
		}
	}
}

// An export written before the signals existed still loads, and its trace has
// none: absent from the data is nil here, never a made-up "absent".
func TestEventFromAnOlderExportStillLoads(t *testing.T) {
	var ev Event
	old := `{"time":1,"status":200,"cache":{"conv":"aaaaaaaaaaaa","msgs":3,"prefix":["3:bbbbbbbbbbbb"],"marks":["m2"],"policy":"off"}}`
	if err := json.Unmarshal([]byte(old), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Cache == nil || ev.Cache.Msgs != 3 || ev.Cache.Signals != nil || ev.Cache.Sess != "" || ev.Cache.Parent != "" {
		t.Fatalf("older event loaded as %+v", ev.Cache)
	}
}

func TestTraceSignalsRoundTrip(t *testing.T) {
	want := &CacheTrace{
		Conv: "aaaaaaaaaaaa", Msgs: 3, Sess: "bbbbbbbbbbbbbbbbbbbbbbbb", Parent: "cccccccccccccccccccccccc",
		Signals: &CacheSignals{
			UserID: "json", ParentSession: SignalPresent, SubagentMarker: SignalAbsent, AgentHeader: SignalNotObservable,
			Probe: SignalAbsent, TitleHelper: SignalAbsent, ExtendedTTLBeta: SignalPresent,
		},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got CacheTrace
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Sess != want.Sess || got.Parent != want.Parent || got.Signals == nil || *got.Signals != *want.Signals {
		t.Fatalf("round trip lost data: %s", raw)
	}
	// Not-observable and absent are different things on the wire.
	if !strings.Contains(string(raw), `"agentHeader":"not-observable"`) || !strings.Contains(string(raw), `"subagentMarker":"absent"`) {
		t.Fatalf("tri-state not spelled out: %s", raw)
	}
}
