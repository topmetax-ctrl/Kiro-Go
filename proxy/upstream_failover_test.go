package proxy

// Failover behavior of the multi-target forwarding path.
//
// The invariant under test throughout: a target is only abandoned for the next
// one when NOTHING has been written to the client. Once the first byte is out,
// the request belongs to that target whatever happens next.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

// setupMultiTargetRoute wires one route over the given upstream URLs, each as a
// its own provider at ascending Priority (so index order == try order).
func setupMultiTargetRoute(t *testing.T, clientModel string, urls ...string) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var ups []config.UpstreamProvider
	var targets []config.RouteTarget
	for i, u := range urls {
		id := "up-" + string(rune('a'+i))
		ups = append(ups, config.UpstreamProvider{
			ID: id, Name: "provider-" + string(rune('a'+i)), BaseURL: u, ApiKey: "s", Enabled: true,
		})
		targets = append(targets, config.RouteTarget{
			UpstreamID: id, Priority: i, Weight: 1, Enabled: true,
		})
	}
	route := config.ModelRoute{ID: "route-1", Model: clientModel, Targets: targets, Enabled: true}
	if err := config.UpdateUpstreamConfig(ups, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
}

func forwardOnce(t *testing.T, clientModel string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"model":"` + clientModel + `","messages":[],"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(body), clientModel, stream, "/messages", true, "") {
		t.Fatal("expected route to match and forward")
	}
	return rec
}

// A 5xx on the preferred provider must be retried on the backup, and the client
// must see only the backup's successful body — no trace of the failed attempt.
func TestForwardFailsOverOn5xx(t *testing.T) {
	metrics.Reset()
	var primaryHits, backupHits int32

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok-from-backup"}`))
	}))
	defer backup.Close()

	setupMultiTargetRoute(t, "m", primary.URL, backup.URL)
	rec := forwardOnce(t, "m", false)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"id":"ok-from-backup"}` {
		t.Fatalf("client body = %q; the failed attempt must not reach the client", got)
	}
	if primaryHits != 1 || backupHits != 1 {
		t.Fatalf("hits: primary=%d backup=%d, want 1/1", primaryHits, backupHits)
	}
}

// 429 is provider-specific (that provider's quota), so it must fail over.
func TestForwardFailsOverOn429(t *testing.T) {
	metrics.Reset()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"backup"}`))
	}))
	defer backup.Close()

	setupMultiTargetRoute(t, "m", primary.URL, backup.URL)
	if rec := forwardOnce(t, "m", false); rec.Code != 200 {
		t.Fatalf("status = %d, want 200 via failover", rec.Code)
	}
}

// A dead TCP endpoint is the classic failover case.
func TestForwardFailsOverOnConnectionError(t *testing.T) {
	metrics.Reset()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"backup"}`))
	}))
	defer backup.Close()

	// Port 1 refuses connections.
	setupMultiTargetRoute(t, "m", "http://127.0.0.1:1/dead", backup.URL)
	rec := forwardOnce(t, "m", false)
	if rec.Code != 200 || rec.Body.String() != `{"id":"backup"}` {
		t.Fatalf("status=%d body=%q, want backup to serve", rec.Code, rec.Body.String())
	}
}

// 401/403/400 mean the request or credential is wrong, not that the provider is
// unhealthy. Retrying would multiply the same error and hide the real cause, so
// the first such response is surfaced immediately — as a public error, never
// the upstream body, and never as a client-auth 401.
func TestForwardDoesNotFailOverOnAuthError(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			metrics.Reset()
			var backupHits int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			}))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&backupHits, 1)
				_, _ = w.Write([]byte(`{"id":"backup"}`))
			}))
			defer backup.Close()

			setupMultiTargetRoute(t, "m", primary.URL, backup.URL)
			rec := forwardOnce(t, "m", false)

			if backupHits != 0 {
				t.Errorf("backup was tried %d times; %d must not fail over", backupHits, status)
			}
			if strings.Contains(rec.Body.String(), "nope") {
				t.Errorf("raw upstream body leaked: %s", rec.Body.String())
			}
			if rec.Code == http.StatusUnauthorized && status != 0 {
				t.Errorf("upstream %d must not become client 401", status)
			}
			if rec.Code == 200 {
				t.Errorf("unexpected success")
			}
		})
	}
}

// THE critical invariant: once the stream has begun, a mid-stream upstream
// failure cannot be retried elsewhere — the client already holds a partial
// response and a second attempt would concatenate two answers.
func TestForwardDoesNotRetryAfterStreamStarted(t *testing.T) {
	metrics.Reset()
	var backupHits int32

	// Sends valid SSE, then hangs up mid-stream.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"partial\":1}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Panicking in a test server handler drops the connection, simulating a
		// truncated upstream stream.
		panic(http.ErrAbortHandler)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"backup\":1}\n\n"))
	}))
	defer backup.Close()

	setupMultiTargetRoute(t, "m", primary.URL, backup.URL)
	rec := forwardOnce(t, "m", true)

	if backupHits != 0 {
		t.Fatalf("backup tried %d times after bytes were already written to the client", backupHits)
	}
	if strings.Contains(rec.Body.String(), "backup") {
		t.Fatalf("client body contains two responses spliced together: %q", rec.Body.String())
	}
}

// When the LAST target also fails with a retryable status, the client gets one
// public error (the last attempt's mapping). Raw upstream text stays internal.
func TestForwardLastTargetSendsPublicError(t *testing.T) {
	metrics.Reset()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"first down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"second exhausted"}}`))
	}))
	defer backup.Close()

	setupMultiTargetRoute(t, "m", primary.URL, backup.URL)
	rec := forwardOnce(t, "m", false)

	if rec.Code != 429 {
		t.Fatalf("status = %d, want the last target's mapped 429", rec.Code)
	}
	got := rec.Body.String()
	if strings.Contains(got, "second exhausted") || strings.Contains(got, "first down") {
		t.Fatalf("raw upstream leaked: %s", got)
	}
	if !strings.Contains(got, "rate limited") && !strings.Contains(got, "provider_rate_limited") {
		t.Fatalf("expected public rate-limit error, got %s", got)
	}
}

// Each attempt records its own event, so the panel shows the failure and the
// recovery separately rather than hiding the unhealthy provider.
func TestForwardRecordsPerAttemptMetrics(t *testing.T) {
	metrics.Reset()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer backup.Close()

	setupMultiTargetRoute(t, "m", primary.URL, backup.URL)
	forwardOnce(t, "m", false)

	evs, _ := metrics.Events(metrics.EventFilter{Limit: 50})
	var failEv, okEv *metrics.Event
	for i := range evs {
		switch evs[i].ProviderID {
		case "up-a":
			failEv = &evs[i]
		case "up-b":
			okEv = &evs[i]
		}
	}
	if failEv == nil || okEv == nil {
		t.Fatalf("want one event per attempt, got %+v", evs)
	}
	if failEv.Ok || failEv.Attempt != 0 || failEv.Status != 503 {
		t.Errorf("first attempt event wrong: %+v", *failEv)
	}
	if !okEv.Ok || okEv.Attempt != 1 {
		t.Errorf("second attempt event should be ok with Attempt=1: %+v", *okEv)
	}
}

// A route whose providers are all disabled must fall through to the Kiro pool,
// exactly like an unrouted model — this is the loop-safety invariant.
func TestForwardFallsThroughWhenNoTargetEligible(t *testing.T) {
	metrics.Reset()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	up := config.UpstreamProvider{ID: "up-1", Name: "off", BaseURL: "http://127.0.0.1:1", Enabled: false}
	route := config.ModelRoute{
		ID: "r1", Model: "m", Enabled: true,
		Targets: []config.RouteTarget{{UpstreamID: "up-1", Enabled: true}},
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{up}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	body := `{"model":"m","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	rec := httptest.NewRecorder()
	h := &Handler{}
	if h.tryForwardUpstream(r, rec, []byte(body), "m", false, "/messages", true, "") {
		t.Fatal("want false (fall through to Kiro pool) when no target is eligible")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("nothing should be written on fall-through, got %q", rec.Body.String())
	}
}
