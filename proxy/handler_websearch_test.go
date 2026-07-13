package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
	"kiro-go/search"
)

// fakeConversationRunner returns a scripted KiroRunResult, so the handler can be
// exercised without a live Kiro backend or provider.
type fakeConversationRunner struct {
	result KiroRunResult
	err    error
	calls  int
}

func (f *fakeConversationRunner) Run(ctx context.Context, account *config.Account, original *KiroPayload, policy WebSearchPolicy) (KiroRunResult, error) {
	f.calls++
	if f.err != nil {
		return KiroRunResult{}, f.err
	}
	return f.result, nil
}

// newWebSearchTestHandler builds a Handler with one enabled account and the
// given runner injected.
func newWebSearchTestHandler(t *testing.T, runner ConversationRunner) *Handler {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "acct",
		Enabled:     true,
		AccessToken: "token",
		ProfileArn:  "arn:aws:codewhisperer:profile/acct",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{
		pool:               p,
		promptCache:        newPromptCacheTracker(defaultPromptCacheTTL),
		conversationRunner: runner,
	}
}

func TestNonStreamRunnerFinalHasNoInternalToolUseAndSources(t *testing.T) {
	runner := &fakeConversationRunner{
		result: KiroRunResult{
			FinalRound: KiroRoundResult{
				VisibleContent: "Go 1.26 is the latest stable release.",
			},
			TotalInputTokens:  1200,
			TotalOutputTokens: 80,
			TotalCredits:      0.05,
			SearchCalls:       1,
			SearchRounds:      1,
			Sources: []SearchSource{
				{Title: "Go Downloads", URL: "https://go.dev/dl/"},
			},
		},
	}
	h := newWebSearchTestHandler(t, runner)

	payload := basePayload()
	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(context.Background(), rec, payload, "claude-sonnet-4.5", false,
		claudeThinkingResponseOptions{}, 1, nil, "", true, testPolicy())

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if runner.calls != 1 {
		t.Fatalf("expected runner to be called once, got %d", runner.calls)
	}

	var resp ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// No internal web_search tool_use may leak to the client.
	for _, block := range resp.Content {
		if block.Type == "tool_use" && isWebSearchToolName(block.Name) {
			t.Fatalf("internal web_search tool_use leaked to client: %#v", block)
		}
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("expected stop_reason end_turn, got %q", resp.StopReason)
	}
	// Sources appended deterministically.
	var text string
	for _, block := range resp.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	if !strings.Contains(text, "Sources:") || !strings.Contains(text, "https://go.dev/dl/") {
		t.Fatalf("expected appended sources, got %q", text)
	}
	// Aggregate usage from the run (input is overwritten by context-based estimate
	// only when ContextUsagePct>0, which it is not here, so the run total stands).
	if resp.Usage.InputTokens != 1200 {
		t.Fatalf("expected aggregate input tokens 1200, got %d", resp.Usage.InputTokens)
	}
}

func TestNonStreamRunnerProviderErrorDoesNotFailAccount(t *testing.T) {
	runner := &fakeConversationRunner{
		err: &search.ProviderError{Kind: search.ErrAuth, StatusCode: 401, Err: errStub("unauthorized")},
	}
	h := newWebSearchTestHandler(t, runner)

	payload := basePayload()
	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(context.Background(), rec, payload, "claude-sonnet-4.5", false,
		claudeThinkingResponseOptions{}, 1, nil, "", true, testPolicy())

	// A provider auth error is surfaced as an error, but the account must not be
	// excluded/failed (single account here; the runner is called exactly once).
	if runner.calls != 1 {
		t.Fatalf("expected exactly one runner call (no account retry), got %d", runner.calls)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 for provider auth error, got 200")
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

// parseSSEEvents splits a raw SSE stream into (event, dataJSON) pairs.
func parseSSEEvents(t *testing.T, raw string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev.event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev.event != "" {
			out = append(out, ev)
		}
	}
	return out
}

type sseEvent struct {
	event string
	data  string
}

func TestStreamRunnerFinalReplayNoInternalToolUse(t *testing.T) {
	runner := &fakeConversationRunner{
		result: KiroRunResult{
			FinalRound: KiroRoundResult{
				VisibleContent: "Go 1.26 is the latest stable release.",
				Events: []KiroRoundEvent{
					{Kind: RoundEventText, Text: "Go 1.26 is the latest stable release."},
				},
			},
			TotalInputTokens:  1200,
			TotalOutputTokens: 80,
			TotalCredits:      0.05,
			Sources:           []SearchSource{{Title: "Go Downloads", URL: "https://go.dev/dl/"}},
		},
	}
	h := newWebSearchTestHandler(t, runner)

	rec := httptest.NewRecorder()
	h.handleClaudeStream(context.Background(), rec, basePayload(), "claude-sonnet-4.5", false,
		claudeThinkingResponseOptions{}, 1, nil, "", true, testPolicy())

	events := parseSSEEvents(t, rec.Body.String())
	if len(events) == 0 {
		t.Fatalf("no SSE events; body=%q", rec.Body.String())
	}

	// message_start exactly once, and first.
	startCount, stopCount := 0, 0
	deltaStop := ""
	var text strings.Builder
	sawToolUse := false
	for _, ev := range events {
		switch ev.event {
		case "message_start":
			startCount++
		case "message_stop":
			stopCount++
		case "message_delta":
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(ev.data), &m); err == nil {
				if d, ok := m["delta"].(map[string]interface{}); ok {
					if sr, ok := d["stop_reason"].(string); ok {
						deltaStop = sr
					}
				}
			}
		case "content_block_start":
			var m map[string]interface{}
			_ = json.Unmarshal([]byte(ev.data), &m)
			if cb, ok := m["content_block"].(map[string]interface{}); ok {
				if cb["type"] == "tool_use" {
					sawToolUse = true
				}
			}
		case "content_block_delta":
			var m map[string]interface{}
			_ = json.Unmarshal([]byte(ev.data), &m)
			if d, ok := m["delta"].(map[string]interface{}); ok {
				if d["type"] == "text_delta" {
					if s, ok := d["text"].(string); ok {
						text.WriteString(s)
					}
				}
			}
		}
	}

	if startCount != 1 {
		t.Fatalf("expected exactly one message_start, got %d", startCount)
	}
	if stopCount != 1 {
		t.Fatalf("expected exactly one message_stop, got %d", stopCount)
	}
	if sawToolUse {
		t.Fatalf("internal web_search tool_use leaked into the stream")
	}
	if deltaStop != "end_turn" {
		t.Fatalf("expected stop_reason end_turn, got %q", deltaStop)
	}
	got := text.String()
	if !strings.Contains(got, "latest stable release") {
		t.Fatalf("final text not replayed, got %q", got)
	}
	if !strings.Contains(got, "Sources:") || !strings.Contains(got, "https://go.dev/dl/") {
		t.Fatalf("expected appended sources in stream text, got %q", got)
	}
}

func TestStreamRunnerErrorBeforeStreamStartsSendsError(t *testing.T) {
	runner := &fakeConversationRunner{
		err: &search.ProviderError{Kind: search.ErrAuth, StatusCode: 401, Err: errStub("unauthorized")},
	}
	h := newWebSearchTestHandler(t, runner)

	rec := httptest.NewRecorder()
	h.handleClaudeStream(context.Background(), rec, basePayload(), "claude-sonnet-4.5", false,
		claudeThinkingResponseOptions{}, 1, nil, "", true, testPolicy())

	if runner.calls != 1 {
		t.Fatalf("expected exactly one runner call (no account retry on provider error), got %d", runner.calls)
	}
	// Provider auth error before any streaming → a JSON error body, not SSE.
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 for provider auth error before stream start")
	}
}
