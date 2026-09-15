package proxy

// End-to-end coverage of the usage telemetry recorded on the forwarding relay:
// cache breakdowns survive the relay byte-for-byte untouched while landing on
// the metrics.Event, and failover never double-counts them.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

func forwardStreamRequestClaude(t *testing.T, clientModel string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"model":"` + clientModel + `","messages":[],"stream":true}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(body), clientModel, true, "/messages", true, "") {
		t.Fatal("expected route to match and forward")
	}
	return rec
}

func TestForwardStreamRecordsCacheTelemetryVerbatim(t *testing.T) {
	metrics.Reset()

	// Anthropic caching stream per the official examples: message_start carries
	// the input + cache figures, message_delta the final output total.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2679,"cache_creation_input_tokens":0,"cache_read_input_tokens":2400,"output_tokens":3}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":89}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(stream))
	}))
	defer upstream.Close()
	setupPricedForwardRoute(t, upstream.URL, "claude-cache", 3, 15)

	rec := forwardStreamRequestClaude(t, "claude-cache")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != stream {
		t.Fatalf("relayed stream differs from upstream — the scanner must stay an observer")
	}

	// The event must carry the canonical total (input + cache read), the cache
	// breakdown, and upstream as the usage source.
	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	ev := evs[0]
	if ev.InputTokens != 2679+2400 {
		t.Fatalf("input = %d, want 5079 (input + cache_read)", ev.InputTokens)
	}
	if ev.OutputTokens != 89 {
		t.Fatalf("output = %d, want 89", ev.OutputTokens)
	}
	if ev.Usage == nil {
		t.Fatal("usage breakdown missing from event")
	}
	if ev.Usage.CacheReadInputTokens == nil || *ev.Usage.CacheReadInputTokens != 2400 {
		t.Fatalf("cache read = %v, want 2400", ev.Usage.CacheReadInputTokens)
	}
	if ev.Usage.Source != metrics.UsageSourceUpstream {
		t.Fatalf("source = %q, want upstream", ev.Usage.Source)
	}
	if ev.Usage.Protocol != "anthropic" {
		t.Fatalf("protocol = %q, want anthropic (from the /messages path)", ev.Usage.Protocol)
	}

	// The provider aggregate must weight the cache ratio over exactly the
	// observable population: one event, 2400/5079.
	p := providerStat(t, "up-1")
	if p.CacheObservedRequests != 1 {
		t.Fatalf("cache observed = %d, want 1", p.CacheObservedRequests)
	}
	if want := 2400.0 * 100 / 5079.0; p.CacheHitRate < want-0.01 || p.CacheHitRate > want+0.01 {
		t.Fatalf("cache hit rate = %v, want ~%v", p.CacheHitRate, want)
	}
}

func TestForwardNonStreamRecordsOpenAICachedTokens(t *testing.T) {
	metrics.Reset()

	// OpenAI chat shape: cached_tokens is inside prompt_tokens.
	respBody := `{"id":"chatcmpl-1","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":850}}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(respBody))
	}))
	defer upstream.Close()
	setupPricedForwardRoute(t, upstream.URL, "gpt-cached", 0, 0)

	rec := forwardRequest(t, "", "gpt-cached")
	if rec.Code != 200 || rec.Body.String() != respBody {
		t.Fatalf("relay altered the response: %d", rec.Code)
	}

	p := providerStat(t, "up-1")
	if p.InputTokens != 1000 {
		t.Fatalf("input = %d, want 1000 (cached tokens must not be added)", p.InputTokens)
	}
	if p.CacheReadInputTokens != 850 {
		t.Fatalf("cache read = %d, want 850", p.CacheReadInputTokens)
	}
	if want := 85.0; p.CacheHitRate < want-0.01 || p.CacheHitRate > want+0.01 {
		t.Fatalf("cache hit rate = %v, want %v", p.CacheHitRate, want)
	}
}

func TestForwardNoUsageTelemetryStaysUnknown(t *testing.T) {
	metrics.Reset()

	// A provider reporting no usage at all must not gain a cache opinion.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	setupPricedForwardRoute(t, upstream.URL, "gpt-quiet", 0, 0)

	forwardRequest(t, "", "gpt-quiet")

	p := providerStat(t, "up-1")
	if p.CacheHitRate != -1 || p.CacheObservedRequests != 0 {
		t.Fatalf("cache = rate %v observed %d, want -1/0 (no telemetry is unknown)", p.CacheHitRate, p.CacheObservedRequests)
	}
	if p.InputTokens != 0 {
		t.Fatalf("input = %d, want 0", p.InputTokens)
	}
}

func TestForwardFailoverRecordsEachAttemptOnce(t *testing.T) {
	metrics.Reset()

	var hits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":90}}}`))
	}))
	defer backup.Close()

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	// Unique provider ids and DISTINCT priorities: the metrics store is a
	// package-level singleton (shared ids inherit foreign fail streaks), and
	// equal priorities form one weighted tier — a load-balanced pick, not a
	// failover chain (see config.orderTargets). Priority 1 makes the backup a
	// true fallback so the walk order is deterministic.
	p1 := config.UpstreamProvider{ID: "failover-primary", Name: "primary", BaseURL: primary.URL, Enabled: true}
	p2 := config.UpstreamProvider{ID: "failover-backup", Name: "backup", BaseURL: backup.URL, Enabled: true}
	route := config.ModelRoute{
		ID: "r1", Model: "fail-model", Enabled: true,
		Targets: []config.RouteTarget{
			{UpstreamID: "failover-primary", Enabled: true, Priority: 0, Weight: 1},
			{UpstreamID: "failover-backup", Enabled: true, Priority: 1, Weight: 1},
		},
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{p1, p2}, []config.ModelRoute{route}); err != nil {
		t.Fatal(err)
	}

	rec := forwardRequest(t, "", "fail-model")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 via backup", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("primary hits = %d, want 1", hits)
	}

	// Two events: one 503 on p1, one 200 on p2 — usage counted once, on the
	// attempt that actually served.
	evs, total := metrics.Events(metrics.EventFilter{})
	if total != 2 {
		t.Fatalf("events = %d, want 2 (one per attempt)", total)
	}
	var okEv *metrics.Event
	for i := range evs {
		if evs[i].Ok {
			okEv = &evs[i]
		}
	}
	if okEv == nil || okEv.Usage == nil || okEv.Usage.CacheReadInputTokens == nil || *okEv.Usage.CacheReadInputTokens != 90 {
		t.Fatalf("serving attempt usage = %+v, want cached 90", okEv)
	}

	b := providerStat(t, "failover-backup")
	if b.InputTokens != 100 || b.CacheReadInputTokens != 90 {
		t.Fatalf("backup tokens = %d/%d, want 100/90", b.InputTokens, b.CacheReadInputTokens)
	}
	p := providerStat(t, "failover-primary")
	if p.InputTokens != 0 || p.CacheObservedRequests != 0 {
		t.Fatalf("failed attempt must not contribute usage: %+v", p)
	}
}

func TestForwardLocalConsumptionCacheSumsPerField(t *testing.T) {
	p := config.UpstreamProvider{ID: "p", Name: "n"}
	var c forwardLocalConsumption

	// Round 1 reports cache; round 2 reports nothing; round 3 reports cache and
	// reasoning. The partial reporter must not zero the aggregate.
	c.addRound(usageCounts{
		Input: 100, Output: 50,
		CacheReadInputTokens:     &[]int64{80}[0],
		CacheCreationInputTokens: &[]int64{10}[0],
	}, p, 10)
	c.addRound(usageCounts{Input: 40, Output: 20}, p, 10)
	c.addRound(usageCounts{
		Input: 60, Output: 30,
		CacheReadInputTokens:  &[]int64{50}[0],
		ReasoningOutputTokens: &[]int64{12}[0],
	}, p, 10)

	if c.cacheReadTokens != 130 || c.cacheCreationTokens != 10 {
		t.Fatalf("cache sums = %d/%d, want 130/10", c.cacheReadTokens, c.cacheCreationTokens)
	}
	if c.reasoningTokens != 12 {
		t.Fatalf("reasoning = %d, want 12 (only the reporting round)", c.reasoningTokens)
	}
	u := c.usage()
	if u == nil {
		t.Fatal("usage must be published when any round reported a breakdown")
	}
	if *u.CacheReadInputTokens != 130 || *u.CacheCreationInputTokens != 10 {
		t.Fatalf("published cache = %v/%v, want 130/10", *u.CacheReadInputTokens, *u.CacheCreationInputTokens)
	}
	if *u.ReasoningOutputTokens != 12 {
		t.Fatalf("published reasoning = %v, want 12", *u.ReasoningOutputTokens)
	}

	// No breakdown anywhere: the flat totals stay, the nested object stays nil.
	var none forwardLocalConsumption
	none.addRound(usageCounts{Input: 5, Output: 5}, p, 5)
	if none.usage() != nil {
		t.Fatal("usage must be nil when no round reported a breakdown")
	}
}
