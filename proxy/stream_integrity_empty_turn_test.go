package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// ============================================================================
// The empty-turn silent failure, end to end.
//
// These run through the real event-stream parser and the real retry helper
// rather than calling classifyStreamIntegrity directly, because the bug lived in
// the interaction between two layers: reasoning frames set sawOutput in
// parseEventStreamTracked (so errEmptyKiroStream never fires), and the classifier
// used to accept any non-empty stopReason before it ever looked at the content
// count. Testing the classifier alone would have missed it, and did.
//
// Production symptom: the turn ends with nothing on screen and no error anywhere.
// ============================================================================

// emptyTurnCallback wires a callback plus its measure/reset pair, mirroring what
// handleClaudeStreaming builds around rawContentBuilder / rawThinkingBuilder.
type emptyTurnCallback struct {
	content  string
	thinking string
	stop     string
	tools    int
}

func (c *emptyTurnCallback) callback() *KiroStreamCallback {
	return &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				c.thinking += text
			} else {
				c.content += text
			}
		},
		OnToolUse:    func(KiroToolUse) { c.tools++ },
		OnStopReason: func(r string) { c.stop = r },
	}
}

func (c *emptyTurnCallback) measure() (int, int, string, bool) {
	return len(c.content), c.tools, c.stop, len(c.thinking) > 0
}

func (c *emptyTurnCallback) reset() {
	c.content, c.thinking, c.stop, c.tools = "", "", "", 0
}

func writeReasoning(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
		"text": text,
	}))
}

func writeAnswer(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": text,
	}))
}

func writeStop(t *testing.T, w http.ResponseWriter, reason string) {
	t.Helper()
	_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
		"stopReason": reason,
	}))
}

// A turn that streams thinking and then ends with stopReason=end_turn, carrying
// no answer and no tool call, must NOT be reported as success. Before the fix
// this returned nil and the client received
// message_delta(stop_reason=end_turn)+message_stop with an empty body.
func TestEmptyEndTurnAfterReasoningIsNotSuccess(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		writeReasoning(t, w, "Let me look at the file first.")
		writeStop(t, w, "end_turn")
	}))
	defer server.Close()
	defer setupIntegrityTestUpstream(t, server)()

	state := &emptyTurnCallback{}
	err := runKiroWithIntegrityRetry(context.Background(), integrityTestAccount(),
		integrityTestPayload(), state.callback(), state.measure, state.reset,
		func() bool { return true })

	if err == nil {
		t.Fatalf("reasoning-only turn with stopReason=end_turn reported success "+
			"(answer=%q tools=%d) — the client would see an empty turn that just ends",
			state.content, state.tools)
	}
	if !isStreamIntegrityError(err) {
		t.Fatalf("got %v, want a stream integrity error so callers rotate without "+
			"blaming a healthy account", err)
	}
	// The retry budget must actually be spent: this shape is recoverable.
	if got := hits.Load(); got != int32(maxSameAccountStreamRetries+1) {
		t.Errorf("upstream hits=%d, want %d", got, maxSameAccountStreamRetries+1)
	}
}

// The recovery case that matters in production: the first attempt dies after
// thinking, the retry answers properly. The caller must end up with the real
// answer and no error.
func TestEmptyEndTurnRecoversOnRetry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			writeReasoning(t, w, "Thinking about it.")
			writeStop(t, w, "end_turn")
			return
		}
		writeReasoning(t, w, "Thinking about it.")
		writeAnswer(t, w, "here is the answer")
		writeStop(t, w, "end_turn")
	}))
	defer server.Close()
	defer setupIntegrityTestUpstream(t, server)()

	state := &emptyTurnCallback{}
	err := runKiroWithIntegrityRetry(context.Background(), integrityTestAccount(),
		integrityTestPayload(), state.callback(), state.measure, state.reset,
		func() bool { return true })

	if err != nil {
		t.Fatalf("retry should have recovered the turn: %v", err)
	}
	if state.content != "here is the answer" {
		t.Fatalf("answer=%q, want the retry's answer with no leftover from the "+
			"discarded attempt", state.content)
	}
}

// A turn with no answer is legitimately complete when the stop reason explains
// the emptiness — thinking can consume the whole output budget. Retrying would
// burn quota reproducing the same result and would replace the accurate
// max_tokens signal with a generic failure, so exactly one request must be sent.
func TestEmptyTurnWithExplanatoryStopReasonIsAccepted(t *testing.T) {
	for _, reason := range []string{"max_tokens", "refusal", "model_context_window_exceeded"} {
		t.Run(reason, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusOK)
				writeReasoning(t, w, "thinking used the whole budget")
				writeStop(t, w, reason)
			}))
			defer server.Close()
			defer setupIntegrityTestUpstream(t, server)()

			state := &emptyTurnCallback{}
			err := runKiroWithIntegrityRetry(context.Background(), integrityTestAccount(),
				integrityTestPayload(), state.callback(), state.measure, state.reset,
				func() bool { return true })

			if err != nil {
				t.Fatalf("stopReason=%s explains an empty turn; got %v", reason, err)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("upstream hits=%d, want 1 (no retry for an explained empty turn)", got)
			}
		})
	}
}

// A tool call with no prose is a real answer. This is the normal agentic shape
// and must never be treated as an empty turn.
func TestToolOnlyTurnIsSuccess(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		writeReasoning(t, w, "I should read the file.")
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "tool_1",
			"name":      "read_file",
			"input":     `{"path":"a.go"}`,
			"stop":      true,
		}))
		writeStop(t, w, "end_turn")
	}))
	defer server.Close()
	defer setupIntegrityTestUpstream(t, server)()

	state := &emptyTurnCallback{}
	err := runKiroWithIntegrityRetry(context.Background(), integrityTestAccount(),
		integrityTestPayload(), state.callback(), state.measure, state.reset,
		func() bool { return true })

	if err != nil {
		t.Fatalf("tool-only turn must be a success: %v", err)
	}
	if state.tools != 1 {
		t.Fatalf("tools=%d, want 1", state.tools)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits=%d, want 1", got)
	}
}
