package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// Phase 0 contract/golden tests. These lock the observable behaviour of the
// account-retry + streaming loops that the Handler exposes for all three public
// protocols (/v1/messages, /v1/chat/completions, /v1/responses) BEFORE any
// refactor (typed errors, ModelCache extraction, ChatExecutor) touches them.
//
// The invariants pinned here are the ones a refactor could silently break:
//   - the SSE event *sequence* per protocol,
//   - retry onto the next account BEFORE the first client-visible byte,
//   - NO retry AFTER the first byte (and the protocol-specific error surface),
//   - the exhaustion tail (503 when no account was ever usable, 500 otherwise).
//
// They deliberately assert structure (event names, attempt counts, status
// codes), not byte-for-byte equality: ids/timestamps are non-deterministic and
// are not part of the contract.

// goldenUpstream wires a Handler to a fake Kiro event-stream server. The
// returned *int is incremented once per upstream attempt so tests can prove
// whether the retry loop re-selected an account. Package globals (kiroEndpoints,
// kiroHttpStore) and the config singleton are swapped, so these tests must not
// run in parallel.
func goldenUpstream(t *testing.T, accounts []config.Account, upstream http.HandlerFunc) (*Handler, *int) {
	t.Helper()

	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, a := range accounts {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("add account %s: %v", a.ID, err)
		}
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
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
	kiroHttpStore.Store(&http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}})
	t.Cleanup(func() { kiroHttpStore.Store(oldClient) })

	p := accountpool.GetPool()
	p.Reload()

	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}
	return h, &attempts
}

func goldenAccounts(ids ...string) []config.Account {
	accts := make([]config.Account, 0, len(ids))
	for _, id := range ids {
		accts = append(accts, config.Account{
			ID:          id,
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		})
	}
	return accts
}

func goldenPayload() *KiroPayload {
	p := &KiroPayload{}
	p.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}
	return p
}

// longContent exceeds every per-protocol streaming buffer threshold (Claude >20
// runes, OpenAI >50 runes), so the happy-path tests deterministically flush at
// least one text delta and mark the stream started.
const longContent = "This is a deliberately long assistant answer used so the streaming buffer flushes at least one visible delta."

// writeContentFrame emits a single valid assistantResponseEvent frame and flushes.
func writeContentFrame(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": content,
	}))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// eventNames returns the ordered list of SSE event names in a raw stream.
func eventNames(evs []sseEvent) []string {
	names := make([]string, 0, len(evs))
	for _, e := range evs {
		names = append(names, e.event)
	}
	return names
}

func containsInOrder(seq, want []string) bool {
	i := 0
	for _, s := range seq {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	return i == len(want)
}

// ---- Claude /v1/messages ------------------------------------------------

func TestClaudeStreamByteShapeGolden(t *testing.T) {
	h, attempts := goldenUpstream(t, goldenAccounts("a1"), func(w http.ResponseWriter, r *http.Request) {
		writeContentFrame(t, w, longContent)
	})

	rec := httptest.NewRecorder()
	h.handleClaudeStream(context.Background(), rec, goldenPayload(), "claude-sonnet-4.5", false,
		claudeThinkingResponseOptions{}, 1, nil, "", false, WebSearchPolicy{})

	if *attempts != 1 {
		t.Fatalf("expected 1 upstream attempt, got %d", *attempts)
	}
	evs := parseSSEEvents(t, rec.Body.String())
	if !containsInOrder(eventNames(evs), []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}) {
		t.Fatalf("Claude SSE sequence broken, got %v", eventNames(evs))
	}
	// message_start / message_stop appear exactly once.
	starts, stops := 0, 0
	for _, e := range evs {
		switch e.event {
		case "message_start":
			starts++
		case "message_stop":
			stops++
		}
	}
	if starts != 1 || stops != 1 {
		t.Fatalf("expected exactly one message_start/message_stop, got %d/%d", starts, stops)
	}
}

func TestClaudeStreamNoRetryAfterFirstByte(t *testing.T) {
	// Two accounts are available; the first emits a real content frame (marking
	// the stream started) then truncates mid-frame so parseEventStream fails.
	// The handler must NOT re-select account #2 after bytes were flushed.
	h, attempts := goldenUpstream(t, goldenAccounts("a1", "a2"), func(w http.ResponseWriter, r *http.Request) {
		writeContentFrame(t, w, longContent)
		// A partial prelude (2 of 12 bytes) forces io.ErrUnexpectedEOF downstream.
		_, _ = w.Write([]byte{0x00, 0x00})
	})

	rec := httptest.NewRecorder()
	h.handleClaudeStream(context.Background(), rec, goldenPayload(), "claude-sonnet-4.5", false,
		claudeThinkingResponseOptions{}, 1, nil, "", false, WebSearchPolicy{})

	if *attempts != 1 {
		t.Fatalf("retry-after-first-byte occurred: expected 1 attempt, got %d", *attempts)
	}
	evs := parseSSEEvents(t, rec.Body.String())
	sawStart, sawError := false, false
	for _, e := range evs {
		switch e.event {
		case "message_start":
			sawStart = true
		case "error":
			sawError = true
		}
	}
	if !sawStart {
		t.Fatalf("expected message_start before the mid-stream failure, got %v", eventNames(evs))
	}
	if !sawError {
		t.Fatalf("Claude must emit an error SSE on mid-stream failure, got %v", eventNames(evs))
	}
}

// ---- OpenAI /v1/chat/completions ---------------------------------------

func TestOpenAIStreamByteShapeGolden(t *testing.T) {
	h, attempts := goldenUpstream(t, goldenAccounts("a1"), func(w http.ResponseWriter, r *http.Request) {
		writeContentFrame(t, w, longContent)
	})

	rec := httptest.NewRecorder()
	h.handleOpenAIStream(rec, goldenPayload(), "gpt-4", false, 1, "")

	if *attempts != 1 {
		t.Fatalf("expected 1 upstream attempt, got %d", *attempts)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("expected chat.completion.chunk objects, got %q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("expected stream to terminate with data: [DONE], got tail %q", body[max(0, len(body)-40):])
	}
}

func TestOpenAIStreamRetryBeforeFirstByte(t *testing.T) {
	call := 0
	h, attempts := goldenUpstream(t, goldenAccounts("a1", "a2"), func(w http.ResponseWriter, r *http.Request) {
		call++
		if call == 1 {
			http.Error(w, "temporary upstream failure", http.StatusInternalServerError)
			return
		}
		writeContentFrame(t, w, longContent)
	})

	rec := httptest.NewRecorder()
	h.handleOpenAIStream(rec, goldenPayload(), "gpt-4", false, 1, "")

	if *attempts != 2 {
		t.Fatalf("expected retry onto second account (2 attempts), got %d", *attempts)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("expected a successful chunk after retry, got %q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("expected [DONE] after successful retry")
	}
}

func TestOpenAIStreamNoRetryAfterFirstByte(t *testing.T) {
	h, attempts := goldenUpstream(t, goldenAccounts("a1", "a2"), func(w http.ResponseWriter, r *http.Request) {
		writeContentFrame(t, w, longContent)
		_, _ = w.Write([]byte{0x00, 0x00})
	})

	rec := httptest.NewRecorder()
	h.handleOpenAIStream(rec, goldenPayload(), "gpt-4", false, 1, "")

	if *attempts != 1 {
		t.Fatalf("retry-after-first-byte occurred: expected 1 attempt, got %d", *attempts)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("expected a chunk to have been flushed before the failure, got %q", body)
	}
	// OpenAI's contract on a post-first-byte failure is to go silent: no error
	// object, no [DONE]. This is the behaviour the ChatExecutor refactor must keep.
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("OpenAI stream must not emit [DONE] after a mid-stream failure, got %q", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("OpenAI stream stays silent on mid-stream failure (no error object), got %q", body)
	}
}

func TestOpenAINonStreamExhaustionTail(t *testing.T) {
	t.Run("no_accounts_available_503", func(t *testing.T) {
		h, _ := goldenUpstream(t, nil, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("upstream must not be called when no account is available")
		})
		rec := httptest.NewRecorder()
		h.handleOpenAINonStream(rec, goldenPayload(), "gpt-4", false, 1, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 when no account was ever usable, got %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("all_accounts_fail_500", func(t *testing.T) {
		h, attempts := goldenUpstream(t, goldenAccounts("a1"), func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "temporary upstream failure", http.StatusInternalServerError)
		})
		rec := httptest.NewRecorder()
		h.handleOpenAINonStream(rec, goldenPayload(), "gpt-4", false, 1, "")
		if *attempts < 1 {
			t.Fatalf("expected the single account to be attempted at least once")
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 when accounts existed but all failed, got %d", rec.Code)
		}
	})
}

// ---- OpenAI Responses /v1/responses ------------------------------------

func TestResponsesStreamByteShapeGolden(t *testing.T) {
	h, attempts := goldenUpstream(t, goldenAccounts("a1"), func(w http.ResponseWriter, r *http.Request) {
		writeContentFrame(t, w, longContent)
	})

	rec := httptest.NewRecorder()
	h.handleResponsesStream(rec, goldenPayload(), "claude-sonnet-4.5", false, 1, "", "", "resp_test",
		&ResponsesRequest{Model: "claude-sonnet-4.5"}, nil, false)

	if *attempts != 1 {
		t.Fatalf("expected 1 upstream attempt, got %d", *attempts)
	}
	evs := parseSSEEvents(t, rec.Body.String())
	if !containsInOrder(eventNames(evs), []string{"response.created", "response.output_text.delta", "response.completed"}) {
		t.Fatalf("Responses SSE sequence broken, got %v", eventNames(evs))
	}
	if !strings.HasSuffix(strings.TrimSpace(rec.Body.String()), "data: [DONE]") {
		t.Fatalf("expected Responses stream to terminate with data: [DONE]")
	}
}

func TestResponsesStreamNoRetryAfterFirstByte(t *testing.T) {
	h, attempts := goldenUpstream(t, goldenAccounts("a1", "a2"), func(w http.ResponseWriter, r *http.Request) {
		writeContentFrame(t, w, longContent)
		_, _ = w.Write([]byte{0x00, 0x00})
	})

	rec := httptest.NewRecorder()
	h.handleResponsesStream(rec, goldenPayload(), "claude-sonnet-4.5", false, 1, "", "", "resp_test",
		&ResponsesRequest{Model: "claude-sonnet-4.5"}, nil, false)

	if *attempts != 1 {
		t.Fatalf("retry-after-first-byte occurred: expected 1 attempt, got %d", *attempts)
	}
	evs := parseSSEEvents(t, rec.Body.String())
	sawCreated, sawFailed := false, false
	for _, e := range evs {
		switch e.event {
		case "response.created":
			sawCreated = true
		case "response.failed":
			sawFailed = true
		}
	}
	if !sawCreated {
		t.Fatalf("expected response.created before the mid-stream failure, got %v", eventNames(evs))
	}
	if !sawFailed {
		t.Fatalf("Responses must emit response.failed on mid-stream failure, got %v", eventNames(evs))
	}
}
