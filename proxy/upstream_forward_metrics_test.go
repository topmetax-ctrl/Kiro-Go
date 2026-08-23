package proxy

// End-to-end checks that the forwarding relay records real token usage and
// per-provider metrics without altering a single relayed byte.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"kiro-go/config"
	"kiro-go/metrics"
)

// setupPricedForwardRoute mirrors setupForwardRoute but attaches per-1M-token
// prices so the cost attribution can be asserted.
func setupPricedForwardRoute(t *testing.T, upstreamURL, clientModel string, inPrice, outPrice float64) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	up := config.UpstreamProvider{
		ID: "up-1", Name: "priced-upstream", BaseURL: upstreamURL, ApiKey: "s", Enabled: true,
		PriceInPerM: inPrice, PriceOutPerM: outPrice,
	}
	route := config.ModelRoute{
		ID: "route-1", Model: clientModel, UpstreamID: up.ID, Enabled: true,
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{up}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
}

// providerStat returns the recorded stats for one provider id, failing the test
// when it is absent. Metrics live in a package-level singleton shared by every
// test in this package, and Reset deliberately retains known provider names, so
// assertions must select their provider rather than assume a single entry.
func providerStat(t *testing.T, id string) metrics.ProviderStat {
	t.Helper()
	for _, p := range metrics.ProviderStats() {
		if p.ProviderID == id {
			return p
		}
	}
	t.Fatalf("provider %q not found in %+v", id, metrics.ProviderStats())
	return metrics.ProviderStat{}
}
func forwardStreamRequest(t *testing.T, clientModel string) *httptest.ResponseRecorder {
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

func TestForwardStreamRecordsTokensAndRelaysVerbatim(t *testing.T) {
	metrics.Reset()

	// A COMPLETE Anthropic turn: the delta carries its "type" (as real frames do)
	// and message_delta carries stop_reason, which is Anthropic's end-of-turn
	// signal. Both matter — without them usageScanner.Truncated() reports a
	// truncated stream and the relay appends an SSE error frame, so the
	// byte-identical assertion below would be testing a failure path instead of
	// the happy one.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":500,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello world"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":250}}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(stream))
	}))
	defer upstream.Close()

	// $3/1M in, $15/1M out — the usual Sonnet-shaped numbers.
	setupPricedForwardRoute(t, upstream.URL, "claude-forward", 3, 15)

	rec := forwardStreamRequest(t, "claude-forward")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// The relay must be byte-identical: the usage scanner is an observer only.
	if got := rec.Body.String(); got != stream {
		t.Fatalf("relayed body differs from upstream.\n got: %q\nwant: %q", got, stream)
	}

	p := providerStat(t, "up-1")
	if p.InputTokens != 500 || p.OutputTokens != 250 {
		t.Fatalf("tokens = %d/%d, want 500/250", p.InputTokens, p.OutputTokens)
	}
	if p.Streamed != 1 {
		t.Fatalf("streamed = %d, want 1", p.Streamed)
	}
	// 500/1e6*3 + 250/1e6*15 = 0.0015 + 0.00375
	if want := 0.00525; p.CostUSD < want-1e-9 || p.CostUSD > want+1e-9 {
		t.Fatalf("cost = %v, want %v", p.CostUSD, want)
	}
	if p.InFlight != 0 {
		t.Fatalf("inFlight = %d, want 0 after completion", p.InFlight)
	}
}

func TestForwardNonStreamRecordsParsedTokens(t *testing.T) {
	metrics.Reset()

	respBody := `{"id":"chatcmpl-1","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":80,"completion_tokens":20}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(respBody))
	}))
	defer upstream.Close()

	setupPricedForwardRoute(t, upstream.URL, "gpt-forward", 0, 0)
	key, err := config.AddApiKey(config.ApiKeyEntry{Name: "k", Key: "sk-x", Enabled: true, TokenLimit: 100000})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	rec := forwardRequest(t, key.ID, "gpt-forward")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != respBody {
		t.Fatalf("relayed body differs.\n got: %q\nwant: %q", got, respBody)
	}

	p := providerStat(t, "up-1")
	if p.InputTokens != 80 || p.OutputTokens != 20 {
		t.Fatalf("token stats = %+v, want 80/20", p)
	}
	// An unpriced provider must attribute no cost rather than a fake zero-price.
	if p.CostUSD != 0 {
		t.Fatalf("cost = %v, want 0 for an unpriced provider", p.CostUSD)
	}

	// Real parsed usage must also drive the API key quota, replacing the old
	// one-request-unit fallback.
	if got := config.FindApiKeyByValue("sk-x"); got == nil || got.TokensUsed != 100 {
		t.Fatalf("key tokens = %v, want 100", got)
	}
}

func TestForwardRecordsFailureStatusPerProvider(t *testing.T) {
	metrics.Reset()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer upstream.Close()

	setupPricedForwardRoute(t, upstream.URL, "gpt-forward", 0, 0)
	rec := forwardRequest(t, "", "gpt-forward")
	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429 relayed", rec.Code)
	}

	d, ok := metrics.ProviderDetailFor("up-1", 60)
	if !ok {
		t.Fatal("provider detail missing")
	}
	if d.Failed != 1 || d.FailStreak != 1 {
		t.Fatalf("failure not recorded: %+v", d.ProviderStat)
	}
	var found bool
	for _, st := range d.Statuses {
		if st.Status == 429 && st.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("status 429 not in histogram: %+v", d.Statuses)
	}
	if len(d.Models) != 1 || d.Models[0].Model != "gpt-forward" {
		t.Fatalf("model breakdown = %+v", d.Models)
	}
}

func TestForwardRecordsUpstreamErrorMessage(t *testing.T) {
	metrics.Reset()

	// The relay of an error body succeeds, so without explicitly capturing the
	// upstream's text the recorded event would carry a bare status code and the
	// admin panel would show a reason-less error row.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"daily quota exhausted"}}`))
	}))
	defer upstream.Close()

	setupPricedForwardRoute(t, upstream.URL, "gpt-forward", 0, 0)
	rec := forwardRequest(t, "", "gpt-forward")

	got := rec.Body.String()
	if strings.Contains(got, "daily quota exhausted") {
		t.Fatalf("client received raw upstream text: %q", got)
	}
	if rec.Code != 429 {
		t.Fatalf("status=%d", rec.Code)
	}

	d, ok := metrics.ProviderDetailFor("up-1", 60)
	if !ok {
		t.Fatal("provider detail missing")
	}
	if len(d.RecentErrs) != 1 {
		t.Fatalf("recent errors = %+v, want 1", d.RecentErrs)
	}
	if got := d.RecentErrs[0].Message; !strings.Contains(got, "daily quota exhausted") {
		t.Fatalf("admin error message = %q, want the upstream diagnostic", got)
	}
}

func TestUpstreamErrorSummaryShapes(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"anthropic object", `{"error":{"type":"overloaded_error","message":"server busy"}}`, "overloaded_error: server busy"},
		{"message only", `{"error":{"message":"bad key"}}`, "bad key"},
		{"error as string", `{"error":"insufficient credit"}`, "insufficient credit"},
		{"top-level message", `{"message":"gateway timeout"}`, "gateway timeout"},
		{"plain text", "upstream exploded", "upstream exploded"},
		{"empty", "   ", ""},
		{"html page", "<html><body>502</body></html>", "<html><body>502</body></html>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := upstreamErrorSummary([]byte(c.body)); got != c.want {
				t.Fatalf("summary = %q, want %q", got, c.want)
			}
		})
	}
}

func TestUpstreamErrorSummaryIsBounded(t *testing.T) {
	// A chatty upstream must not park an unbounded string in the metrics ring.
	long := strings.Repeat("é", 5000) // multi-byte, to catch byte-vs-rune slicing
	got := upstreamErrorSummary([]byte(`{"error":{"message":"` + long + `"}}`))
	if r := []rune(got); len(r) > 301 {
		t.Fatalf("summary kept %d runes, want <= 301", len(r))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncation not marked: %q", got[len(got)-10:])
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a multi-byte rune")
	}
}

// canceledWriter is a ResponseWriter whose Write fails partway through, standing
// in for a client that hangs up mid-stream. It pairs with a canceled request
// context, which is what actually distinguishes a disconnect from an upstream
// read error.
//
// The first Write succeeds — so the relay is committed to this target and cannot
// fail over — and every later one fails. It also cancels the request context and
// closes gone, which the test's upstream waits on before sending the rest of the
// stream. That handshake is what makes the disconnect deterministic: the relay
// reads with a 16KB buffer, so 20 small SSE frames written back-to-back are
// frequently delivered as ONE read, producing one Write and then a clean EOF. In
// that case the relay delivered every byte successfully and there is no error to
// attribute to anyone, so the outcome is a legitimate success and the test's
// premise never occurs. Making the upstream keep sending AFTER the client is gone
// guarantees the second Write that the disconnect path is about.
type canceledWriter struct {
	*httptest.ResponseRecorder
	cancel  context.CancelFunc
	gone    chan struct{}
	written int
}

func (c *canceledWriter) Write(b []byte) (int, error) {
	if c.written > 0 {
		return 0, errors.New("connection reset by peer")
	}
	c.written += len(b)
	n, err := c.ResponseRecorder.Write(b)
	// The client is gone as of this byte. Cancel first, then release the upstream:
	// the ordering means the frames it sends next are guaranteed to meet a failing
	// Write and an already-canceled context.
	c.cancel()
	close(c.gone)
	return n, err
}

func TestForwardClientDisconnectIsNotBlamedOnUpstream(t *testing.T) {
	metrics.Reset()

	// gone is closed by the writer once the client has "hung up".
	gone := make(chan struct{})

	// The upstream answers 200, sends an opening frame, then waits for the client
	// to disappear before sending more. Without that wait the whole stream can
	// arrive in a single read and the relay finishes cleanly — a success, not the
	// mid-relay disconnect this test is about.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {}\n\n"))
		flush()

		select {
		case <-gone:
		case <-time.After(5 * time.Second):
			return // never leak this handler if the writer is never called
		}

		// The client is gone; these frames are what the relay fails to deliver.
		for i := 0; i < 20; i++ {
			if _, err := w.Write([]byte("event: content_block_delta\ndata: {}\n\n")); err != nil {
				return
			}
			flush()
		}
	}))
	defer upstream.Close()

	setupPricedForwardRoute(t, upstream.URL, "claude-forward", 3, 15)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := `{"model":"claude-forward","messages":[],"stream":true}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(ctx, apiKeyContextKey{}, ""))
	w := &canceledWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, gone: gone}

	h := &Handler{}
	if !h.tryForwardUpstream(r, w, []byte(body), "claude-forward", true, "/messages", true, "") {
		t.Fatal("expected route to match and forward")
	}

	d, ok := metrics.ProviderDetailFor("up-1", 60)
	if !ok {
		t.Fatal("provider detail missing")
	}
	// The upstream did nothing wrong: the disconnect is its own category and must
	// leave reliability untouched.
	if d.Canceled != 1 {
		t.Fatalf("canceled = %d, want 1", d.Canceled)
	}
	if d.Failed != 0 {
		t.Fatalf("failed = %d, want 0 (client disconnect is not an upstream failure)", d.Failed)
	}
	if d.FailStreak != 0 || !d.Healthy {
		t.Fatalf("streak=%d healthy=%v, want 0/true", d.FailStreak, d.Healthy)
	}
	// A "200 error" row would be nonsense to an admin reading the panel.
	if len(d.RecentErrs) != 0 {
		t.Fatalf("recent errors = %+v, want none", d.RecentErrs)
	}
	for _, st := range d.Statuses {
		if st.Status == 200 {
			t.Fatalf("cancellation recorded under upstream status 200: %+v", d.Statuses)
		}
	}
}
