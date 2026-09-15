package proxy

// End-to-end coverage of the usage telemetry recorded on the forwarding relay:
// cache breakdowns survive the relay byte-for-byte untouched while landing on
// the metrics.Event, and failover never double-counts them.

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		Input: ptr(100), Output: ptr(50),
		CacheReadInputTokens:     &[]int64{80}[0],
		CacheCreationInputTokens: &[]int64{10}[0],
	}, p, 10)
	c.addRound(usageCounts{Input: ptr(40), Output: ptr(20)}, p, 10)
	c.addRound(usageCounts{
		Input: ptr(60), Output: ptr(30),
		CacheReadInputTokens:  &[]int64{50}[0],
		ReasoningOutputTokens: &[]int64{12}[0],
	}, p, 10)

	// Multi-round aggregate: IN/OUT are the SUM across rounds (100+40+60,
	// 50+20+30) — never the last round, never an average — and ModelRounds
	// carries the real round count.
	if c.inputTokens != 200 || c.outputTokens != 100 {
		t.Fatalf("totals = %d/%d, want 200/100 (summed across rounds)", c.inputTokens, c.outputTokens)
	}
	if c.rounds != 3 {
		t.Fatalf("rounds = %d, want 3", c.rounds)
	}
	if c.cacheReadTokens != 130 || c.cacheCreationTokens != 10 {
		t.Fatalf("cache sums = %d/%d, want 130/10", c.cacheReadTokens, c.cacheCreationTokens)
	}
	if c.reasoningTokens != 12 {
		t.Fatalf("reasoning = %d, want 12 (only the reporting round)", c.reasoningTokens)
	}
	u := c.usage()
	if u == nil {
		t.Fatal("usage must be published when any round reported telemetry")
	}
	// Canonical totals ride along: non-nil exactly when the sum is positive.
	if u.InputTokens == nil || *u.InputTokens != 200 || u.OutputTokens == nil || *u.OutputTokens != 100 {
		t.Fatalf("canonical totals = %v/%v, want 200/100", u.InputTokens, u.OutputTokens)
	}
	if *u.CacheReadInputTokens != 130 || *u.CacheCreationInputTokens != 10 {
		t.Fatalf("published cache = %v/%v, want 130/10", *u.CacheReadInputTokens, *u.CacheCreationInputTokens)
	}
	if *u.ReasoningOutputTokens != 12 {
		t.Fatalf("published reasoning = %v, want 12", *u.ReasoningOutputTokens)
	}

	// No telemetry anywhere: nothing to publish.
	var none forwardLocalConsumption
	none.addRound(usageCounts{}, p, 5)
	if none.usage() != nil {
		t.Fatal("usage must be nil when no round reported anything")
	}
}

func TestForwardLocalConsumptionFailedRoundKeepsReportedUsage(t *testing.T) {
	p := config.UpstreamProvider{ID: "p", Name: "n"}

	// A round the upstream processed and then failed WITH telemetry is real
	// consumption: it joins the totals and the round count.
	var withUsage forwardLocalConsumption
	withUsage.addRound(usageCounts{Input: ptr(100), Output: ptr(50)}, p, 10)
	withUsage.addFailedRoundIfKnown(usageCounts{
		Input: ptr(200), Output: ptr(80),
		CacheReadInputTokens: &[]int64{150}[0],
	}, p, 20)
	if withUsage.inputTokens != 300 || withUsage.outputTokens != 130 || withUsage.rounds != 2 {
		t.Fatalf("failed round dropped: %d/%d rounds=%d, want 300/130 rounds=2", withUsage.inputTokens, withUsage.outputTokens, withUsage.rounds)
	}
	if withUsage.cacheReadTokens != 150 {
		t.Fatalf("failed round cache = %d, want 150", withUsage.cacheReadTokens)
	}

	// A failure without telemetry invents nothing.
	var quiet forwardLocalConsumption
	quiet.addRound(usageCounts{Input: ptr(100), Output: ptr(50)}, p, 10)
	quiet.addFailedRoundIfKnown(usageCounts{}, p, 20)
	if quiet.inputTokens != 100 || quiet.outputTokens != 50 || quiet.rounds != 1 {
		t.Fatalf("silent failure fabricated consumption: %+v", quiet)
	}
	if quiet.cacheReported || quiet.reasoningReported {
		t.Fatal("silent failure must not gain a cache/reasoning opinion")
	}
}

// --- error-body usage (error != zero usage) ---------------------------------

func TestForwardNon2xxErrorBodyWithUsageRecordsUsage(t *testing.T) {
	metrics.Reset()

	// A single target, so the 503 is surfaced (and recorded) rather than
	// retried. The upstream processed the prompt before rejecting it and says
	// so in the error body — that spend is real and must be recorded.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream capacity"},"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":800}}}`))
	}))
	defer upstream.Close()
	setupPricedForwardRoute(t, upstream.URL, "err-usage", 0, 0)

	rec := forwardRequest(t, "", "err-usage")
	if rec.Code == 200 {
		t.Fatalf("status = %d, want the classified failure", rec.Code)
	}

	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	e := evs[0]
	if e.Ok || e.Status != 503 {
		t.Fatalf("event = status %d ok %v, want 503/false", e.Status, e.Ok)
	}
	if e.Usage == nil {
		t.Fatal("usage reported by the failed upstream must not be discarded")
	}
	if e.Usage.InputTokens == nil || *e.Usage.InputTokens != 1000 || e.InputTokens != 1000 {
		t.Fatalf("input = flat %d canonical %v, want 1000/1000", e.InputTokens, e.Usage.InputTokens)
	}
	if e.Usage.OutputTokens == nil || *e.Usage.OutputTokens != 200 {
		t.Fatalf("output canonical = %v, want 200", e.Usage.OutputTokens)
	}
	if e.Usage.CacheReadInputTokens == nil || *e.Usage.CacheReadInputTokens != 800 {
		t.Fatalf("cache read = %v, want 800", e.Usage.CacheReadInputTokens)
	}

	// Contract: token counters represent actual provider consumption, whatever
	// the outcome — the failed attempt's tokens count too.
	p := providerStat(t, "up-1")
	if p.InputTokens != 1000 || p.OutputTokens != 200 || p.CacheReadInputTokens != 800 {
		t.Fatalf("provider tokens = %+v, want 1000/200 with cache 800", p)
	}
	if p.CacheObservedRequests != 1 {
		t.Fatalf("cache observed = %d, want 1", p.CacheObservedRequests)
	}
}

func TestForwardFailoverFailedAttemptKeepsReportedUsage(t *testing.T) {
	metrics.Reset()

	// Attempt 1 fails WITH usage; attempt 2 succeeds with its own. Per-attempt
	// Event semantics stay: each provider's event carries exactly its own
	// telemetry, nothing summed across the retry.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"unavailable","usage":{"prompt_tokens":400,"completion_tokens":50}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10}}`))
	}))
	defer backup.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	// Distinct priorities make the try-order deterministic (equal priorities
	// load-balance the first pick — see config.orderTargets).
	p1 := config.UpstreamProvider{ID: "usage-fail-primary", Name: "primary", BaseURL: primary.URL, Enabled: true}
	p2 := config.UpstreamProvider{ID: "usage-fail-backup", Name: "backup", BaseURL: backup.URL, Enabled: true}
	route := config.ModelRoute{
		ID: "r1", Model: "usage-fail-model", Enabled: true,
		Targets: []config.RouteTarget{
			{UpstreamID: "usage-fail-primary", Enabled: true, Priority: 0, Weight: 1},
			{UpstreamID: "usage-fail-backup", Enabled: true, Priority: 1, Weight: 1},
		},
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{p1, p2}, []config.ModelRoute{route}); err != nil {
		t.Fatal(err)
	}

	rec := forwardRequest(t, "", "usage-fail-model")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 via backup", rec.Code)
	}

	evs, total := metrics.Events(metrics.EventFilter{})
	if total != 2 {
		t.Fatalf("events = %d, want 2 (one per attempt)", total)
	}
	var failedEv, okEv *metrics.Event
	for i := range evs {
		switch {
		case evs[i].ProviderID == "usage-fail-primary":
			failedEv = &evs[i]
		case evs[i].ProviderID == "usage-fail-backup":
			okEv = &evs[i]
		}
	}
	if failedEv == nil || failedEv.Ok {
		t.Fatalf("failed attempt event missing: %+v", evs)
	}
	if failedEv.Usage == nil || failedEv.Usage.InputTokens == nil || *failedEv.Usage.InputTokens != 400 {
		t.Fatalf("failed attempt usage = %+v, want canonical input 400", failedEv.Usage)
	}
	if okEv == nil || okEv.Usage == nil || okEv.Usage.InputTokens == nil || *okEv.Usage.InputTokens != 100 {
		t.Fatalf("serving attempt usage = %+v, want canonical input 100", okEv.Usage)
	}
	// Nothing crossed over: each provider's counters hold exactly its own spend.
	fp := providerStat(t, "usage-fail-primary")
	bp := providerStat(t, "usage-fail-backup")
	if fp.InputTokens != 400 || fp.OutputTokens != 50 {
		t.Fatalf("primary tokens = %d/%d, want 400/50", fp.InputTokens, fp.OutputTokens)
	}
	if bp.InputTokens != 100 || bp.OutputTokens != 10 {
		t.Fatalf("backup tokens = %d/%d, want 100/10", bp.InputTokens, bp.OutputTokens)
	}
}

// --- partial streams keep the usage that already arrived --------------------

func TestForwardTruncatedStreamKeepsPartialUsage(t *testing.T) {
	metrics.Reset()

	// The upstream delivers message_start (input + cache) and then the
	// connection ends with no terminal frame. The event must be a failure, and
	// its IN figure must stay known while OUT stays UNKNOWN — this is the exact
	// shape the tri-state IN/OUT cells exist for.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2679,"cache_read_input_tokens":2400}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial ans"}}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(stream))
		// Clean close, no message_delta / message_stop: a truncated turn.
	}))
	defer upstream.Close()
	setupPricedForwardRoute(t, upstream.URL, "claude-truncated", 0, 0)

	rec := forwardStreamRequestClaude(t, "claude-truncated")
	if got := rec.Body.String(); !strings.HasPrefix(got, "event: message_start") {
		t.Fatalf("client did not receive the partial stream: %q", got)
	}

	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	e := evs[0]
	if e.Ok {
		t.Fatal("a stream with no terminal frame must not record as success")
	}
	if e.Usage == nil {
		t.Fatal("usage observed before the truncation must survive")
	}
	if e.Usage.InputTokens == nil || *e.Usage.InputTokens != 2679+2400 || e.InputTokens != 2679+2400 {
		t.Fatalf("input = flat %d canonical %v, want %d", e.InputTokens, e.Usage.InputTokens, 2679+2400)
	}
	if e.Usage.OutputTokens != nil || e.OutputTokens != 0 {
		t.Fatalf("output = flat %d canonical %v, want unknown (0/nil)", e.OutputTokens, e.Usage.OutputTokens)
	}
	if e.Usage.CacheReadInputTokens == nil || *e.Usage.CacheReadInputTokens != 2400 {
		t.Fatalf("cache read = %v, want 2400", e.Usage.CacheReadInputTokens)
	}
}

// --- compressed responses (the transport decompresses transparently) --------

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestForwardGzipNonStreamUsageParsed(t *testing.T) {
	metrics.Reset()

	// A gzip upstream response is decompressed by the transport itself (the
	// relay never advertises the client's Accept-Encoding, so Go adds gzip and
	// clears Content-Encoding on the response). The usage parse therefore sees
	// plain JSON — the old "compressed bodies lose usage" limitation does not
	// exist on this path — and the client still receives the decompressed body
	// byte-for-byte.
	plain := `{"id":"chatcmpl-gz","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":2000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":1500}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("transport should auto-negotiate gzip, got %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		_, _ = w.Write(gzipBytes(t, []byte(plain)))
	}))
	defer upstream.Close()
	setupPricedForwardRoute(t, upstream.URL, "gpt-gzip", 0, 0)

	rec := forwardRequest(t, "", "gpt-gzip")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != plain {
		t.Fatalf("client body must be the decompressed JSON, got %q", rec.Body.String())
	}

	p := providerStat(t, "up-1")
	if p.InputTokens != 2000 || p.CacheReadInputTokens != 1500 {
		t.Fatalf("gzip usage lost: input %d cache %d, want 2000/1500", p.InputTokens, p.CacheReadInputTokens)
	}
	if want := 75.0; p.CacheHitRate < want-0.01 || p.CacheHitRate > want+0.01 {
		t.Fatalf("cache hit rate = %v, want %v", p.CacheHitRate, want)
	}
}

func TestForwardGzipStreamUsageParsed(t *testing.T) {
	metrics.Reset()

	// Same for streams: a gzipped SSE relay arrives decompressed, the scanner
	// sees the plain frames, and the client receives the plain event stream.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":500,"cache_read_input_tokens":300,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		_, _ = w.Write(gzipBytes(t, []byte(stream)))
	}))
	defer upstream.Close()
	setupPricedForwardRoute(t, upstream.URL, "claude-gzip", 0, 0)

	rec := forwardStreamRequestClaude(t, "claude-gzip")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != stream {
		t.Fatalf("client must receive the plain stream byte-for-byte")
	}

	evs, _ := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if len(evs) != 1 || evs[0].Usage == nil {
		t.Fatalf("gzip stream usage lost: %+v", evs)
	}
	if evs[0].InputTokens != 800 || evs[0].OutputTokens != 42 {
		t.Fatalf("totals = %d/%d, want 800/42", evs[0].InputTokens, evs[0].OutputTokens)
	}
	if evs[0].Usage.CacheReadInputTokens == nil || *evs[0].Usage.CacheReadInputTokens != 300 {
		t.Fatalf("cache read = %v, want 300", evs[0].Usage.CacheReadInputTokens)
	}
}

// --- OpenAI Responses relay end to end ----------------------------------------

func TestForwardResponsesOversizedCompletedKeepsUsage(t *testing.T) {
	metrics.Reset()

	// The terminal response.completed frame carries the whole generated output
	// beside its usage; with a long answer it is far larger than the observer's
	// scan buffer. The relay must deliver it verbatim, capture the usage from
	// inside it, AND record the stream as complete — the Responses API ends
	// with the envelope events, not [DONE], so the old terminal detection
	// mislabeled every such relay as truncated.
	stream := `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		responsesCompletedFrame(100*1024)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(stream))
	}))
	defer upstream.Close()
	setupPricedForwardRoute(t, upstream.URL, "gpt-responses-big", 0, 0)

	rec := httptest.NewRecorder()
	body := `{"model":"gpt-responses-big","input":"hi","stream":true}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(body), "gpt-responses-big", true, "/responses", false, "") {
		t.Fatal("expected route to match and forward")
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The client receives the oversized frame whole — the observer never
	// touches the relayed bytes.
	if !strings.Contains(rec.Body.String(), `"input_tokens":2000`) {
		t.Fatal("client must receive the full response.completed frame verbatim")
	}

	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	e := evs[0]
	if !e.Ok {
		t.Fatalf("a completed Responses stream must not record as truncated/failure: %+v", e)
	}
	if e.Usage == nil {
		t.Fatal("usage inside the oversized frame was lost")
	}
	if e.Usage.InputTokens == nil || *e.Usage.InputTokens != 2000 || e.Usage.OutputTokens == nil || *e.Usage.OutputTokens != 300 {
		t.Fatalf("canonical totals = %v/%v, want 2000/300", e.Usage.InputTokens, e.Usage.OutputTokens)
	}
	if e.Usage.CacheReadInputTokens == nil || *e.Usage.CacheReadInputTokens != 1500 {
		t.Fatalf("cache read = %v, want 1500", e.Usage.CacheReadInputTokens)
	}
	if e.Usage.ReasoningOutputTokens == nil || *e.Usage.ReasoningOutputTokens != 250 {
		t.Fatalf("reasoning = %v, want 250", e.Usage.ReasoningOutputTokens)
	}
	if e.Usage.Protocol != "responses" {
		t.Fatalf("protocol = %q, want responses", e.Usage.Protocol)
	}
	if e.InputTokens != 2000 || e.OutputTokens != 300 {
		t.Fatalf("flat totals = %d/%d, want 2000/300", e.InputTokens, e.OutputTokens)
	}
}
