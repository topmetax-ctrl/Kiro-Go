package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/metrics"
)

// The end-to-end regression for the production symptom: the upstream streams
// thinking, then closes cleanly with no answer and no terminal frame. The client
// must be told, and the provider must not be credited with a success.
func TestForwardTruncatedThinkingStreamSurfacesError(t *testing.T) {
	metrics.Reset()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		// A thinking block streams in full...
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Cogitating...\"}}\n\n"))
		f.Flush()
		// ...and then the handler returns: a clean close, no message_stop.
	}))
	defer upstream.Close()

	setupForwardRoute(t, upstream.URL, "claude-trunc", "")
	rec := forwardStreamRequest(t, "claude-trunc")

	out := rec.Body.String()
	// The relayed thinking bytes must still reach the client untouched.
	if !strings.Contains(out, "thinking_delta") {
		t.Error("relayed thinking bytes were lost")
	}
	// And an error frame must be appended so the client stops waiting silently.
	if !strings.Contains(out, "event: error") {
		t.Errorf("no SSE error frame appended; client would hang silently.\ngot: %q", out)
	}
	if !strings.Contains(out, "truncated response") {
		t.Errorf("error frame lacks a diagnosable reason.\ngot: %q", out)
	}

	// The provider must be charged a failure, not a 200 success — otherwise this
	// whole class of outage stays invisible on the dashboard.
	ov := metrics.Overall()
	if ov.Success != 0 {
		t.Errorf("truncated relay recorded as success (%d)", ov.Success)
	}
	if ov.Failed != 1 {
		t.Errorf("expected 1 recorded failure, got %d", ov.Failed)
	}
}

// The complementary guard: a healthy stream must be untouched — no injected error
// frame, and a clean success on the metric.
func TestForwardCompleteStreamUnaffected(t *testing.T) {
	metrics.Reset()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		f.Flush()
	}))
	defer upstream.Close()

	setupForwardRoute(t, upstream.URL, "claude-ok", "")
	rec := forwardStreamRequest(t, "claude-ok")

	out := rec.Body.String()
	if strings.Contains(out, "event: error") {
		t.Errorf("healthy stream got a spurious error frame.\ngot: %q", out)
	}
	ov := metrics.Overall()
	if ov.Failed != 0 {
		t.Errorf("healthy stream recorded %d failures", ov.Failed)
	}
	if ov.Success != 1 {
		t.Errorf("expected 1 success, got %d", ov.Success)
	}
}

// A client that disconnects mid-stream leaves the relay short of a terminal
// frame, but that is the client's doing. It must stay a 499 cancellation, not be
// reclassified as an upstream truncation (which would blame a healthy provider).
func TestForwardClientCancelNotReportedAsTruncation(t *testing.T) {
	metrics.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"))
		f.Flush()
		// The client goes away mid-stream; keep sending so the relay's write or
		// read observes the cancellation.
		cancel()
		for i := 0; i < 200; i++ {
			if _, err := w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"more\"}}\n\n")); err != nil {
				return
			}
			f.Flush()
		}
	}))
	defer upstream.Close()

	setupForwardRoute(t, upstream.URL, "claude-cancel", "")

	body := `{"model":"claude-cancel","messages":[],"stream":true}`
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(body), "claude-cancel", true, "/messages", true, "") {
		t.Fatal("expected route to match and forward")
	}

	if strings.Contains(rec.Body.String(), "truncated response") {
		t.Error("client cancellation misreported to the client as an upstream truncation")
	}
	// A cancellation is attributed to the client (499), so it must not land in the
	// provider's failure count.
	if ov := metrics.Overall(); ov.Failed != 0 {
		t.Errorf("client cancel counted as %d provider failure(s)", ov.Failed)
	}
}
