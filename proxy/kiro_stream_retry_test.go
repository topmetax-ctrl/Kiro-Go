package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kiro-go/config"
)

// Regression coverage for the empty-stream failure mode: a 200 response whose
// event stream reaches EOF without a single content or tool frame used to be
// reported as success, so the handler synthesised stop_reason "end_turn" over an
// empty body and the client rendered a finished turn with nothing in it.
//
// Three separate contracts are pinned here:
//  1. the parser reports an output-less stream as errEmptyKiroStream,
//  2. CallKiroAPIContext retries such a stream once, because nothing was
//     delivered to the caller yet,
//  3. it does NOT retry once an output callback has run, because that would
//     concatenate two partial answers.

// ---- parser level -------------------------------------------------------

func TestParseEventStreamEmptyStreamIsError(t *testing.T) {
	cases := []struct {
		name   string
		frames [][]byte
	}{
		{
			// The upstream accepted the request and closed the stream immediately.
			name:   "no frames at all",
			frames: nil,
		},
		{
			// Bookkeeping frames only: billing and context usage arrived, the answer
			// did not. This is the shape that produced the empty turns.
			name: "metadata frames only",
			frames: [][]byte{
				awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
				awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
			},
		},
		{
			// An empty content string is not output.
			name: "empty content string",
			frames: [][]byte{
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": ""}),
			},
		},
		{
			name: "empty reasoning text",
			frames: [][]byte{
				awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": ""}),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var texts []string
			var completed bool
			emitted, err := parseEventStreamTracked(joinFrames(tc.frames), &KiroStreamCallback{
				OnText:     func(text string, _ bool) { texts = append(texts, text) },
				OnComplete: func(_, _ int) { completed = true },
			})
			if !errors.Is(err, errEmptyKiroStream) {
				t.Fatalf("expected errEmptyKiroStream, got %v", err)
			}
			if emitted {
				t.Fatalf("emitted must stay false when no output was delivered")
			}
			if len(texts) != 0 {
				t.Fatalf("expected no text callbacks, got %#v", texts)
			}
			// OnComplete is the handler's cue to finalise usage for a turn that
			// happened. An empty stream must not reach it.
			if completed {
				t.Fatalf("OnComplete must not run for an output-less stream")
			}
		})
	}
}

func TestParseEventStreamContentMakesStreamSuccessful(t *testing.T) {
	// The mirror of the case above: one real content frame is enough to make the
	// stream a completed turn, metadata frames or not.
	emitted, err := parseEventStreamTracked(joinFrames([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"}),
	}), &KiroStreamCallback{OnText: func(string, bool) {}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !emitted {
		t.Fatalf("expected emitted=true after OnText ran")
	}
}

func TestParseEventStreamToolUseOnlyIsSuccessful(t *testing.T) {
	// A turn that is nothing but a tool call carries no text, and must not be
	// mistaken for an empty stream.
	emitted, err := parseEventStreamTracked(joinFrames([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_1",
			"name":      "readFile",
			"input":     `{"path":"main.go"}`,
			"stop":      true,
		}),
	}), &KiroStreamCallback{OnToolUse: func(KiroToolUse) {}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !emitted {
		t.Fatalf("expected emitted=true after OnToolUse ran")
	}
}

func TestParseEventStreamIncompleteToolInputIsError(t *testing.T) {
	// The buffered tool JSON is cut off mid-object. Delivering that as an empty
	// input map would invoke the tool with {} instead of its real arguments, which
	// the client cannot distinguish from a genuine no-arg call.
	truncated := joinFrames([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_1",
			"name":      "readFile",
			"input":     `{"path":"ma`,
		}),
	})

	var toolUses []KiroToolUse
	_, err := parseEventStreamTracked(truncated, &KiroStreamCallback{
		OnToolUse: func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
	})
	if !errors.Is(err, errIncompleteKiroToolInput) {
		t.Fatalf("expected errIncompleteKiroToolInput, got %v", err)
	}
	if len(toolUses) != 0 {
		t.Fatalf("a truncated tool block must not be delivered, got %#v", toolUses)
	}
}

func TestParseEventStreamIncompleteToolInputIsErrorWithoutCallback(t *testing.T) {
	// Detection must not depend on the caller having registered OnToolUse: the
	// stream is broken either way, and a caller with no tool handler still needs
	// the failure so it can retry rather than report a finished turn.
	_, err := parseEventStreamTracked(joinFrames([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_1",
			"name":      "readFile",
			"input":     `{"path":"ma`,
		}),
	}), &KiroStreamCallback{})
	if !errors.Is(err, errIncompleteKiroToolInput) {
		t.Fatalf("expected errIncompleteKiroToolInput, got %v", err)
	}
}

// ---- retry classification ----------------------------------------------

func TestIsRetryableStreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"empty stream", errEmptyKiroStream, true},
		{"incomplete tool input", errIncompleteKiroToolInput, true},
		{"unexpected eof", errors.New("unexpected EOF"), true},
		// The caller's own deadline, not an upstream blip. Retrying it burns the
		// remaining budget and writes into a connection that is already gone.
		{"context canceled", context.Canceled, false},
		{"context canceled wrapped", &net.OpError{Op: "read", Err: context.Canceled}, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"net timeout", &net.OpError{Op: "read", Err: &timeoutError{}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableStreamError(tc.err); got != tc.want {
				t.Fatalf("isRetryableStreamError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// timeoutError is a minimal net.Error that reports itself as a timeout.
type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

// ---- endpoint level ----------------------------------------------------

func TestCallKiroAPIRetriesStreamThatDeliveredNothing(t *testing.T) {
	var served int
	up := kiroStreamUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		if served == 1 {
			// A 200 whose stream ends without a single frame.
			w.WriteHeader(http.StatusOK)
			return
		}
		writeContentFrame(t, w, "recovered answer")
	})

	var texts []string
	err := CallKiroAPIContext(context.Background(), streamRetryAccount(), streamRetryPayload(), &KiroStreamCallback{
		OnText: func(text string, _ bool) { texts = append(texts, text) },
	})
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if *up.attempts != 2 {
		t.Fatalf("expected 2 upstream attempts, got %d", *up.attempts)
	}
	if *up.waits != 1 {
		t.Fatalf("expected 1 backoff before the retry, got %d", *up.waits)
	}
	// Only the surviving attempt's output reaches the caller: the failed attempt
	// delivered nothing, so there is nothing to concatenate.
	if len(texts) != 1 || texts[0] != "recovered answer" {
		t.Fatalf("unexpected delivered text: %#v", texts)
	}
}

func TestCallKiroAPIStopsAfterMaxStreamAttempts(t *testing.T) {
	up := kiroStreamUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	err := CallKiroAPIContext(context.Background(), streamRetryAccount(), streamRetryPayload(), &KiroStreamCallback{
		OnText: func(string, bool) {},
	})
	if !errors.Is(err, errEmptyKiroStream) {
		t.Fatalf("expected errEmptyKiroStream to surface, got %v", err)
	}
	if *up.attempts != maxStreamAttemptsPerEndpoint {
		t.Fatalf("expected %d upstream attempts, got %d", maxStreamAttemptsPerEndpoint, *up.attempts)
	}
	// The retry budget is bounded: with one endpoint and no attempt left there is
	// nothing to wait for, so the last failure must not sleep before giving up.
	if *up.waits != 1 {
		t.Fatalf("expected exactly 1 backoff across %d attempts, got %d", maxStreamAttemptsPerEndpoint, *up.waits)
	}
}

func TestCallKiroAPIDoesNotRetryAfterOutputWasDelivered(t *testing.T) {
	up := kiroStreamUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		// Real output first, then the stream dies mid-prelude.
		writeContentFrame(t, w, "partial answer")
		_, _ = w.Write([]byte{0, 0, 0, 42, 0, 0})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	var texts []string
	err := CallKiroAPIContext(context.Background(), streamRetryAccount(), streamRetryPayload(), &KiroStreamCallback{
		OnText: func(text string, _ bool) { texts = append(texts, text) },
	})
	if err == nil {
		t.Fatalf("expected the truncated stream to surface an error")
	}
	// Retrying here would replay the whole prompt and append a second partial
	// answer to the one the caller already received.
	if *up.attempts != 1 {
		t.Fatalf("expected no retry after output was delivered, got %d attempts", *up.attempts)
	}
	if *up.waits != 0 {
		t.Fatalf("expected no backoff, got %d", *up.waits)
	}
	if len(texts) != 1 || texts[0] != "partial answer" {
		t.Fatalf("unexpected delivered text: %#v", texts)
	}
}

func TestCallKiroAPIDoesNotRetryCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up := kiroStreamUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		// Headers only, then hold the stream open and let the client walk away.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		cancel()
		time.Sleep(200 * time.Millisecond)
	})

	err := CallKiroAPIContext(ctx, streamRetryAccount(), streamRetryPayload(), &KiroStreamCallback{
		OnText: func(string, bool) {},
	})
	if err == nil {
		t.Fatalf("expected an error for the cancelled request")
	}
	// The client is gone; the retry budget must not be spent on it.
	if *up.waits != 0 {
		t.Fatalf("expected no backoff for a cancelled context, got %d", *up.waits)
	}
}

// ---- helpers ------------------------------------------------------------

// joinFrames concatenates raw event-stream frames into a single reader. A nil
// slice yields an immediately-EOF body, i.e. a 200 that carried no frames.
func joinFrames(frames [][]byte) io.Reader {
	return bytes.NewReader(bytes.Join(frames, nil))
}

type streamRetryHarness struct {
	attempts *int
	waits    *int
}

// kiroStreamUpstream points CallKiroAPIContext at a single fake endpoint and
// counts both upstream attempts and retry backoffs. Package globals
// (kiroEndpoints, kiroHttpStore, streamRetryWait) and the config singleton are
// swapped, so these tests must not run in parallel.
func kiroStreamUpstream(t *testing.T, upstream http.HandlerFunc) streamRetryHarness {
	t.Helper()

	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	// One endpoint only, so an attempt count above 1 can only mean a retry of the
	// same endpoint rather than a fallback to the next one.
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		upstream(w, r)
	}))
	t.Cleanup(server.Close)

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}})
	t.Cleanup(func() { kiroHttpStore.Store(oldClient) })

	waits := 0
	oldWait := streamRetryWait
	streamRetryWait = func(time.Duration) { waits++ }
	t.Cleanup(func() { streamRetryWait = oldWait })

	return streamRetryHarness{attempts: &attempts, waits: &waits}
}

func streamRetryAccount() *config.Account {
	// No ProfileArn and no region on the account: regionalizeURL stays a no-op so
	// the request keeps hitting the test server's host.
	return &config.Account{
		ID:          "stream-retry",
		Enabled:     true,
		AccessToken: "token-stream-retry",
	}
}

func streamRetryPayload() *KiroPayload {
	p := &KiroPayload{}
	// A payload-level ARN keeps CallKiroAPIContext from attempting live profile
	// resolution for the account.
	p.ProfileArn = "arn:aws:codewhisperer:us-east-1:000000000000:profile/TEST"
	p.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}
	return p
}
