package proxy

// Cache observability on the forwarding relay: the TTL tier split read from the
// upstream's usage, the model it named, and the request-side digests recorded
// beside them. None of it may change a relayed byte.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

func ptrEq(p *int64, want int64) bool { return p != nil && *p == want }

func TestUsageSplitsCacheCreationByTTL(t *testing.T) {
	body := `{"model":"claude-sonnet-5-5","usage":{"input_tokens":6,"cache_creation_input_tokens":1500,` +
		`"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":500,"ephemeral_1h_input_tokens":1000},"output_tokens":9}}`
	u := usageFromJSONBody([]byte(body))
	if !ptrEq(u.CacheCreation5mTokens, 500) || !ptrEq(u.CacheCreation1hTokens, 1000) {
		t.Fatalf("split = %v / %v, want 500 / 1000", u.CacheCreation5mTokens, u.CacheCreation1hTokens)
	}
	if !ptrEq(u.CacheCreationInputTokens, 1500) || u.Model != "claude-sonnet-5-5" {
		t.Fatalf("total/model = %v / %q", u.CacheCreationInputTokens, u.Model)
	}
}

func TestUsageSplitKeepsPresenceSemantics(t *testing.T) {
	// No breakdown at all: the tier is unknown, never derived from the total.
	none := usageFromJSONBody([]byte(`{"usage":{"input_tokens":6,"cache_creation_input_tokens":1500,"cache_read_input_tokens":0,"output_tokens":9}}`))
	if none.CacheCreation5mTokens != nil || none.CacheCreation1hTokens != nil {
		t.Fatalf("absent breakdown must stay nil, got %v / %v", none.CacheCreation5mTokens, none.CacheCreation1hTokens)
	}
	// An explicit zero is an answer.
	zero := usageFromJSONBody([]byte(`{"usage":{"input_tokens":6,"cache_creation_input_tokens":0,"cache_read_input_tokens":900,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":9}}`))
	if !ptrEq(zero.CacheCreation5mTokens, 0) || !ptrEq(zero.CacheCreation1hTokens, 0) {
		t.Fatalf("explicit zeros must be kept, got %v / %v", zero.CacheCreation5mTokens, zero.CacheCreation1hTokens)
	}
	// One member only: the other stays unknown.
	one := usageFromJSONBody([]byte(`{"usage":{"input_tokens":6,"cache_creation_input_tokens":40,"cache_read_input_tokens":0,` +
		`"cache_creation":{"ephemeral_1h_input_tokens":40},"output_tokens":9}}`))
	if one.CacheCreation5mTokens != nil || !ptrEq(one.CacheCreation1hTokens, 40) {
		t.Fatalf("one-sided breakdown: %v / %v", one.CacheCreation5mTokens, one.CacheCreation1hTokens)
	}
	// Other dialects carry no such split.
	oa := usageFromJSONBody([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`))
	if oa.CacheCreation5mTokens != nil || oa.CacheCreation1hTokens != nil || oa.Model != "" {
		t.Fatalf("OpenAI shape grew a TTL split or model: %+v", oa)
	}
}

func TestStreamUsageMergesTTLSplitLastWins(t *testing.T) {
	s := &usageScanner{}
	feed := func(line string) { _, _ = s.Write([]byte(line + "\n")) }
	feed(`data: {"type":"message_start","message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":3,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0},"output_tokens":1}}}`)
	feed(``)
	feed(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)
	// A later cumulative frame is authoritative per field.
	feed(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":3,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":100},"output_tokens":50}}`)
	feed(``)
	c := s.Counts()
	if !ptrEq(c.CacheCreation5mTokens, 0) || !ptrEq(c.CacheCreation1hTokens, 100) {
		t.Fatalf("split = %v / %v, want 0 / 100", c.CacheCreation5mTokens, c.CacheCreation1hTokens)
	}
	if c.Model != "claude-sonnet-5-5" {
		t.Fatalf("model = %q", c.Model)
	}
}

func TestUpstreamModelIsCapped(t *testing.T) {
	long := strings.Repeat("m", 500)
	u := usageFromJSONBody([]byte(`{"model":"` + long + `","usage":{"input_tokens":1,"output_tokens":1}}`))
	if len(u.Model) != maxUpstreamModelLen {
		t.Fatalf("model length = %d, want %d", len(u.Model), maxUpstreamModelLen)
	}
}

func TestTokenOptPolicyLabel(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.TokenOptimizationConfig
		want string
	}{
		{"zero value", config.TokenOptimizationConfig{}, "off"},
		{"off with settings saved", config.TokenOptimizationConfig{Mode: "off", Caveman: "caveman"}, "off"},
		{"observe", config.TokenOptimizationConfig{Mode: "observe", InputCompression: "headroom"}, "observe"},
		{"on, nothing selected", config.TokenOptimizationConfig{Mode: "on", Caveman: "off", Ponytail: "off"}, "on"},
		{"on, caveman only", config.TokenOptimizationConfig{Mode: "on", Caveman: "caveman", Ponytail: "off"}, "on+caveman"},
		{"on, everything", config.TokenOptimizationConfig{
			Mode: "on", InputCompression: "headroom", Headroom: config.HeadroomConfig{Mode: "cache"},
			Caveman: "ultracave", Ponytail: "full",
		}, "on+headroom-cache+ultracave+ponytail-full"},
	}
	for _, c := range cases {
		if got := tokenOptPolicyLabel(c.cfg); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// claudeBody is a three-message Messages request with a breakpoint on the last
// block, the shape Claude Code sends.
const claudeBody = `{"model":"claude-trace","stream":%STREAM%,"system":[{"type":"text","text":"sys"}],` +
	`"tools":[{"name":"Bash","description":"run"}],"thinking":{"type":"enabled","budget_tokens":1000},` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"a"}]},` +
	`{"role":"assistant","content":[{"type":"text","text":"b"}]},` +
	`{"role":"user","content":[{"type":"text","text":"c","cache_control":{"type":"ephemeral"}}]}]}`

func setupTraceRoute(t *testing.T, upstreamURL, extraConfig string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if extraConfig != "" {
		if err := os.WriteFile(path, []byte(extraConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.Init(path); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	up := config.UpstreamProvider{ID: "up-1", Name: "trace-upstream", BaseURL: upstreamURL, ApiKey: "s", Enabled: true}
	route := config.ModelRoute{ID: "route-1", Model: "claude-trace", UpstreamID: up.ID, Enabled: true}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{up}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
}

func forwardClaude(t *testing.T, h *Handler, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.Replace(claudeBody, "%STREAM%", map[bool]string{true: "true", false: "false"}[stream], 1)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	if !h.tryForwardUpstream(r, rec, []byte(body), "claude-trace", stream, "/messages", true, "") {
		t.Fatal("expected route to match and forward")
	}
	return rec
}

const traceStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":1200,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":1000},"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestForwardRecordsTTLSplitModelAndTrace(t *testing.T) {
	metrics.Reset()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(traceStream))
	}))
	defer upstream.Close()
	setupTraceRoute(t, upstream.URL, "")

	rec := forwardClaude(t, &Handler{}, true)
	if rec.Code != 200 || rec.Body.String() != traceStream {
		t.Fatalf("relay altered the stream (status %d)", rec.Code)
	}
	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	ev := evs[0]
	if ev.Usage == nil || !ptrEq(ev.Usage.CacheCreation5mTokens, 200) || !ptrEq(ev.Usage.CacheCreation1hTokens, 1000) {
		t.Fatalf("usage split not recorded: %+v", ev.Usage)
	}
	if ev.UpstreamModel != "claude-sonnet-5-5" {
		t.Fatalf("upstream model = %q", ev.UpstreamModel)
	}
	c := ev.Cache
	if c == nil {
		t.Fatal("cache trace missing")
	}
	if c.Msgs != 3 || c.Policy != "off" || c.Conv == "" || c.Tools == "" || c.System == "" || c.Settings == "" || len(c.Prefix) != 3 {
		t.Fatalf("trace incomplete: %+v", c)
	}
	if strings.Join(c.Marks, ",") != "m2" {
		t.Fatalf("marks = %v, want [m2]", c.Marks)
	}
	// The event serializes the trace without any prompt text.
	raw, _ := json.Marshal(ev)
	for _, leak := range []string{`"sys"`, `"Bash"`, `"text":"a"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("event leaks request content %s: %s", leak, raw)
		}
	}
}

func TestForwardTraceSameBodySameDigests(t *testing.T) {
	metrics.Reset()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":900,"output_tokens":3}}`))
	}))
	defer upstream.Close()
	setupTraceRoute(t, upstream.URL, "")

	h := &Handler{}
	forwardClaude(t, h, false)
	forwardClaude(t, h, false)
	evs, _ := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if len(evs) != 2 || evs[0].Cache == nil || evs[1].Cache == nil {
		t.Fatalf("want two traced events, got %d", len(evs))
	}
	a, _ := json.Marshal(evs[0].Cache)
	b, _ := json.Marshal(evs[1].Cache)
	if string(a) != string(b) {
		t.Fatalf("identical requests must give identical traces:\n%s\n%s", a, b)
	}
	// A response with a cache read but no creation breakdown keeps the tier unknown.
	if evs[0].Usage == nil || evs[0].Usage.CacheCreation5mTokens != nil || evs[0].Usage.CacheCreation1hTokens != nil {
		t.Fatalf("no cache_creation object was sent; tiers must stay unknown: %+v", evs[0].Usage)
	}
}

func TestForwardTraceDisabledLeavesEventsUnchanged(t *testing.T) {
	metrics.Reset()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(traceStream))
	}))
	defer upstream.Close()
	setupTraceRoute(t, upstream.URL, `{"cacheTrace":false}`)

	if config.CacheTraceEnabled() {
		t.Fatal("cacheTrace:false was not honored")
	}
	rec := forwardClaude(t, &Handler{}, true)
	if rec.Body.String() != traceStream {
		t.Fatal("relay altered the stream")
	}
	evs, _ := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if len(evs) != 1 || evs[0].Cache != nil {
		t.Fatalf("disabled tracing must record no trace: %+v", evs)
	}
	raw, _ := json.Marshal(evs[0])
	if strings.Contains(string(raw), `"cache":`) {
		t.Fatalf("disabled tracing left a cache key on the event: %s", raw)
	}
	// Response-side telemetry is not part of the switch.
	if evs[0].Usage == nil || !ptrEq(evs[0].Usage.CacheCreation1hTokens, 1000) {
		t.Fatalf("TTL split must be recorded regardless: %+v", evs[0].Usage)
	}
}

func TestForwardTraceCoversFailuresAndSkipsOtherDialects(t *testing.T) {
	metrics.Reset()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"boom"}}`))
	}))
	defer upstream.Close()
	setupTraceRoute(t, upstream.URL, "")

	forwardClaude(t, &Handler{}, false)
	evs, _ := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if len(evs) == 0 || evs[0].Cache == nil || evs[0].Cache.Msgs != 3 {
		t.Fatalf("a failed request must still carry its trace: %+v", evs)
	}

	// A route that is not Claude-shaped has no prompt cache semantics to trace.
	h := &Handler{}
	if got := h.startCacheTrace(nil, []byte(claudeBody), false, "")(); got != nil {
		t.Fatalf("non-Claude route traced: %+v", got)
	}
}

func TestStartCacheTraceIsSafeConcurrently(t *testing.T) {
	setupTraceRoute(t, "http://127.0.0.1:1", "")
	h := &Handler{}
	body := []byte(strings.Replace(claudeBody, "%STREAM%", "false", 1))
	var wg sync.WaitGroup
	results := make([]*metrics.CacheTrace, 24)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			read := h.startCacheTrace(nil, body, true, "")
			results[i] = read()
			if read() != results[i] {
				t.Errorf("reader must be idempotent")
			}
		}()
	}
	wg.Wait()
	want, _ := json.Marshal(results[0])
	for i, r := range results {
		if r == nil {
			t.Fatalf("result %d is nil", i)
		}
		if got, _ := json.Marshal(r); string(got) != string(want) {
			t.Fatalf("result %d differs", i)
		}
	}
}
