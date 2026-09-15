package proxy

// Terminal-outcome semantics for the forwarding scanner and relay.
//
// Terminal is not success. A stream can settle three different ways over an
// HTTP 200 transport — completed, failed, incomplete — and the metrics must
// tell them apart: a provider's own response.failed must not record as a
// success (it would reset the failure streak a cooldown depends on), and an
// incomplete-by-limit must not cooldown a provider that merely enforced its
// configured cap.

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

// scanSSE feeds a whole SSE transcript through the scanner like the relay does.
func scanSSE(t *testing.T, sse string) *usageScanner {
	t.Helper()
	s := &usageScanner{}
	if _, err := s.Write([]byte(sse)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return s
}

// responsesFailedFrame / responsesIncompleteFrame mirror the official envelope
// shapes: the error/incomplete details and the usage both live inside the
// "response" object.
const responsesFailedFrame = `event: response.failed` + "\n" +
	`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"server_error","message":` +
	`"the model generated a disallowed marker: upstream-internal-detail"},"usage":{"input_tokens":100,"output_tokens":20}}}` + "\n\n"

const responsesIncompleteFrame = `event: response.incomplete` + "\n" +
	`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete",` +
	`"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":50,"output_tokens":90}}}` + "\n\n"

// A. completed: the normal terminal, recorded as a completed outcome.
func TestScannerResponsesCompletedTerminalKind(t *testing.T) {
	s := scanSSE(t, responsesCompletedFrame(0))
	if got := s.Terminal(); got != terminalCompleted {
		t.Errorf("Terminal() = %v, want completed", got)
	}
	if s.Truncated() {
		t.Error("completed stream flagged as truncated")
	}
	if got := s.Counts(); tot(got.Input) != 2000 || tot(got.Output) != 300 {
		t.Errorf("usage = %v/%v, want 2000/300", tot(got.Input), tot(got.Output))
	}
}

// B. failed: terminal, NOT truncated, NOT the same outcome as completed, usage
// retained, and the upstream's own error type is captured — but never its
// message.
func TestScannerResponsesFailedTerminalKind(t *testing.T) {
	s := scanSSE(t, responsesFailedFrame)
	if got := s.Terminal(); got != terminalFailed {
		t.Fatalf("Terminal() = %v, want failed", got)
	}
	if s.Truncated() {
		t.Error("failed terminal misread as truncation")
	}
	if got := s.Counts(); tot(got.Input) != 100 || tot(got.Output) != 20 {
		t.Errorf("usage = %v/%v, want 100/20 (failed frames still report real spend)", tot(got.Input), tot(got.Output))
	}
	if got := s.ErrCode(); got != "server_error" {
		t.Errorf("ErrCode() = %q, want server_error", got)
	}
}

// B (data-only): some relays strip event: lines; the data frame alone must
// still classify.
func TestScannerResponsesFailedDataFrameOnly(t *testing.T) {
	dataOnly := strings.Replace(responsesFailedFrame, "event: response.failed\n", "", 1)
	s := scanSSE(t, dataOnly)
	if got := s.Terminal(); got != terminalFailed {
		t.Errorf("Terminal() = %v, want failed", got)
	}
	if got := s.ErrCode(); got != "server_error" {
		t.Errorf("ErrCode() = %q, want server_error", got)
	}
}

// C. incomplete: a limit ended the turn. Terminal, not truncated, usage
// retained, and — critically — NOT the failed kind, so it can never trip
// provider-failure accounting downstream.
func TestScannerResponsesIncompleteTerminalKind(t *testing.T) {
	s := scanSSE(t, responsesIncompleteFrame)
	if got := s.Terminal(); got != terminalIncomplete {
		t.Fatalf("Terminal() = %v, want incomplete", got)
	}
	if s.Truncated() {
		t.Error("incomplete terminal misread as truncation")
	}
	if got := s.Counts(); tot(got.Input) != 50 || tot(got.Output) != 90 {
		t.Errorf("usage = %v/%v, want 50/90", tot(got.Input), tot(got.Output))
	}
}

// The chat dialects express the same three outcomes through stop/finish
// reasons: max_tokens/length/content_filter are the limit flavors.
func TestScannerLimitReasonsAreIncomplete(t *testing.T) {
	tests := map[string]struct {
		sse  string
		want streamTerminalKind
	}{
		"anthropic max_tokens": {
			sse:  `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":99}}` + "\n\n",
			want: terminalIncomplete,
		},
		"anthropic end_turn": {
			sse:  `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n",
			want: terminalCompleted,
		},
		"openai length": {
			sse:  `data: {"choices":[{"delta":{},"finish_reason":"length"}]}` + "\n\n",
			want: terminalIncomplete,
		},
		"openai content_filter": {
			sse:  `data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}` + "\n\n",
			want: terminalIncomplete,
		},
		"openai stop": {
			sse:  `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			want: terminalCompleted,
		},
		"anthropic error frame": {
			sse:  "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n",
			want: terminalFailed,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := scanSSE(t, tc.sse).Terminal(); got != tc.want {
				t.Errorf("Terminal() = %v, want %v", got, tc.want)
			}
		})
	}
}

// An oversized DATA-ONLY terminal frame (no event: line — some relays strip
// them) must still settle the turn. The giant line bypasses scanFrameMarkers
// entirely, so the filter's head probe is what carries the signal; without it
// a legitimate giant response.completed would be recorded as a truncation.
func TestScannerOversizedDataOnlyTerminalFrame(t *testing.T) {
	big := `data: {"type":"response.completed","response":{"id":"resp_1","output":[{"content":[{"text":"` +
		strings.Repeat("x", 100*1024) +
		`"}]}],"usage":{"input_tokens":2000,"output_tokens":300},"status":"completed"}}` + "\n\n"
	s := scanSSE(t, big)
	if got := s.Terminal(); got != terminalCompleted {
		t.Fatalf("Terminal() = %v, want completed (head probe missed the type)", got)
	}
	if s.Truncated() {
		t.Error("giant data-only completed frame misread as truncation")
	}
	if got := s.Counts(); tot(got.Input) != 2000 || tot(got.Output) != 300 {
		t.Errorf("usage = %v/%v, want 2000/300", tot(got.Input), tot(got.Output))
	}

	bigFailed := strings.Replace(big, `"type":"response.completed"`, `"type":"response.failed"`, 1)
	bigFailed = strings.Replace(bigFailed, `"status":"completed"`, `"status":"failed","error":{"type":"server_error"}`, 1)
	if got := scanSSE(t, bigFailed).Terminal(); got != terminalFailed {
		t.Errorf("giant data-only failed frame: Terminal() = %v, want failed", got)
	}
}

// A terminal frame's kind is decided by the FIRST terminal seen; later frames
// do not overwrite it.
func TestScannerTerminalKindFirstWins(t *testing.T) {
	s := scanSSE(t, responsesFailedFrame+`data: {"type":"message_stop"}`+"\n\n")
	if got := s.Terminal(); got != terminalFailed {
		t.Errorf("Terminal() = %v, want failed (first terminal wins)", got)
	}
}

// ====================================================================
// Relay-level outcomes: what actually lands in the metrics and on the
// client's wire.
// ====================================================================

// forwardStreamOnce drives one streaming forward request against a route
// served by the given upstream URLs (each at ascending priority).
func forwardStreamOnce(t *testing.T, clientModel string) *httptest.ResponseRecorder {
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

// response.failed over HTTP 200 must NOT record as a success: it downgrades
// the metric, bumps the provider failure streak, and — because the response is
// already committed — never retries on the backup.
func TestForwardResponsesFailedNotSuccessNoRetry(t *testing.T) {
	metrics.Reset()
	resetConnectionHealthForTest()
	var primaryHits, backupHits int32

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
			responsesFailedFrame))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		_, _ = w.Write([]byte(`{"id":"from-backup"}`))
	}))
	defer backup.Close()

	setupMultiTargetRoute(t, "m-failed", primary.URL, backup.URL)
	rec := forwardStreamOnce(t, "m-failed")

	if primaryHits != 1 || backupHits != 0 {
		t.Fatalf("hits primary=%d backup=%d, want 1/0 — no retry may happen after a committed stream", primaryHits, backupHits)
	}
	// The client sees the rewritten public error frame, never the upstream's
	// own message or internals.
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Error("client must receive a terminal error frame for a failed stream")
	}
	if strings.Contains(body, "upstream-internal-detail") {
		t.Error("upstream error message leaked to the client")
	}

	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-a"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	e := evs[0]
	if e.Ok {
		t.Errorf("response.failed recorded as success: %+v", e)
	}
	if e.StreamOutcome != "failed" {
		t.Errorf("StreamOutcome = %q, want failed", e.StreamOutcome)
	}
	if e.Status != 200 {
		t.Errorf("Status = %d, want 200 (the wire status; outcome carries the truth)", e.Status)
	}
	if e.Usage == nil || e.Usage.InputTokens == nil || *e.Usage.InputTokens != 100 || e.Usage.OutputTokens == nil || *e.Usage.OutputTokens != 20 {
		t.Fatalf("usage on failed stream = %+v, want 100/20", e.Usage)
	}
	if e.ErrorMsg == "" || !strings.Contains(e.ErrorMsg, "server_error") {
		t.Errorf("ErrorMsg = %q, want the upstream error code", e.ErrorMsg)
	}
	if streak, _, ok := metrics.ProviderFailureStreak("up-a"); !ok || streak != 1 {
		t.Errorf("provider failure streak = %d (ok=%v), want 1 — a provider-reported failure must count", streak, ok)
	}
}

// response.incomplete (max_output_tokens) over HTTP 200 stays a success: the
// provider enforced its own limit, which must never poison cooldown, but the
// outcome must still be distinguishable from completed.
func TestForwardResponsesIncompleteStaysHealthy(t *testing.T) {
	metrics.Reset()
	resetConnectionHealthForTest()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"cut off\"}}\n\n" +
			responsesIncompleteFrame))
	}))
	defer upstream.Close()

	// One provider, one route — the point is the recorded outcome, not routing.
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	up := config.UpstreamProvider{ID: "up-1", Name: "inc-upstream", BaseURL: upstream.URL, ApiKey: "s", Enabled: true}
	route := config.ModelRoute{ID: "route-1", Model: "m-inc", UpstreamID: up.ID, Enabled: true}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{up}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	rec := forwardStreamOnce(t, "m-inc")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	e := evs[0]
	if !e.Ok {
		t.Errorf("incomplete(max_output_tokens) recorded as failure: %+v", e)
	}
	if e.StreamOutcome != "incomplete" {
		t.Errorf("StreamOutcome = %q, want incomplete", e.StreamOutcome)
	}
	if e.Usage == nil || e.Usage.InputTokens == nil || *e.Usage.InputTokens != 50 || e.Usage.OutputTokens == nil || *e.Usage.OutputTokens != 90 {
		t.Fatalf("usage on incomplete stream = %+v, want 50/90", e.Usage)
	}
	if streak, _, ok := metrics.ProviderFailureStreak("up-1"); ok && streak != 0 {
		t.Errorf("provider failure streak = %d, want 0 — a limit outcome must not count against health", streak)
	}
}

// A completed stream labels its outcome, keeping completed/failed/incomplete
// distinguishable end to end.
func TestForwardStreamOutcomeCompleted(t *testing.T) {
	metrics.Reset()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(responsesCompletedFrame(0)))
	}))
	defer upstream.Close()

	setupPricedForwardRoute(t, upstream.URL, "m-ok", 0, 0)
	rec := forwardStreamOnce(t, "m-ok")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	if e := evs[0]; !e.Ok || e.StreamOutcome != "completed" {
		t.Errorf("Ok=%v StreamOutcome=%q, want true/completed", e.Ok, e.StreamOutcome)
	}
}

// A rewritten error frame on a Responses stream must surface in the Responses
// error shape, not the chat-completions one — otherwise Responses clients
// cannot parse the failure at all.
func TestForwardResponsesErrorRewrittenInResponsesDialect(t *testing.T) {
	metrics.Reset()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(responsesFailedFrame))
	}))
	defer upstream.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	up := config.UpstreamProvider{ID: "up-1", Name: "resp-err", BaseURL: upstream.URL, ApiKey: "s", Enabled: true}
	route := config.ModelRoute{ID: "route-1", Model: "m-resp-err", UpstreamID: up.ID, Enabled: true}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{up}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	rec := httptest.NewRecorder()
	reqBody := `{"model":"m-resp-err","input":"hi","stream":true}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(reqBody), "m-resp-err", true, "/responses", false, "") {
		t.Fatal("expected route to match and forward")
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (failure is protocol-level on a 200 stream)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"response.failed"`) || !strings.Contains(body, `"status":"failed"`) {
		t.Errorf("Responses stream error must be rewritten in the Responses dialect, got: %s", body)
	}
	if strings.Contains(body, "upstream-internal-detail") {
		t.Error("upstream error message leaked to the client")
	}
}

// An Anthropic mid-stream error frame over HTTP 200 (overloaded) must likewise
// record as a failure, not a success.
func TestForwardAnthropicMidStreamErrorNotSuccess(t *testing.T) {
	metrics.Reset()
	resetConnectionHealthForTest()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	}))
	defer upstream.Close()

	setupPricedForwardRoute(t, upstream.URL, "m-overload", 0, 0)
	if rec := forwardStreamOnce(t, "m-overload"); rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 {
		t.Fatalf("events = %d, want 1", total)
	}
	e := evs[0]
	if e.Ok {
		t.Errorf("mid-stream overloaded_error recorded as success: %+v", e)
	}
	if e.StreamOutcome != "failed" || !strings.Contains(e.ErrorMsg, "overloaded_error") {
		t.Errorf("StreamOutcome=%q ErrorMsg=%q, want failed/overloaded_error", e.StreamOutcome, e.ErrorMsg)
	}
	if streak, _, ok := metrics.ProviderFailureStreak("up-1"); !ok || streak != 1 {
		t.Errorf("provider failure streak = %d (ok=%v), want 1", streak, ok)
	}
}
