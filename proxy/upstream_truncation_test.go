package proxy

import (
	"strings"
	"testing"
)

// scanAll writes an SSE transcript through the scanner the way the relay does,
// in one chunk. Chunk-boundary behavior is covered separately.
func scanAll(t *testing.T, sse string) *usageScanner {
	t.Helper()
	s := &usageScanner{}
	if _, err := s.Write([]byte(sse)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return s
}

// A complete Anthropic turn must not be flagged. This is the guard against the
// change turning healthy traffic into errors.
func TestScannerCompleteAnthropicStreamNotTruncated(t *testing.T) {
	s := scanAll(t, strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))

	if s.Truncated() {
		t.Error("complete stream flagged as truncated")
	}
	if !s.hadContent() {
		t.Error("text_delta did not register as content")
	}
	if got := s.Counts(); got.Input != 10 || got.Output != 5 {
		t.Errorf("usage = %+v, want input 10 output 5", got)
	}
}

// The production symptom: thinking streams in full, then the turn dies before
// any answer. Transport-clean, so only the missing terminal frame reveals it.
func TestScannerThinkingOnlyStreamIsTruncated(t *testing.T) {
	s := scanAll(t, strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Cogitating..."}}`,
		``,
	}, "\n"))

	if !s.Truncated() {
		t.Error("thinking-only stream not flagged as truncated")
	}
	if s.hadContent() {
		t.Error("thinking_delta must not count as client-visible content")
	}
}

// Text streamed but the terminal frame never arrived: a partial answer, which is
// still a truncation.
func TestScannerTextWithoutTerminalIsTruncated(t *testing.T) {
	s := scanAll(t, strings.Join([]string{
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial ans"}}`,
		``,
	}, "\n"))

	if !s.Truncated() {
		t.Error("unterminated text stream not flagged")
	}
	if !s.hadContent() {
		t.Error("text_delta should register as content")
	}
}

// A tool call with a stop_reason is a complete turn even though it has no text.
func TestScannerToolUseTurnIsComplete(t *testing.T) {
	s := scanAll(t, strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Read"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
	}, "\n"))

	if s.Truncated() {
		t.Error("tool_use turn flagged as truncated")
	}
	if !s.hadContent() {
		t.Error("tool_use block should register as content")
	}
}

// An upstream error frame settles the turn: the client was told it failed, which
// is a complete outcome and must not be double-reported as a truncation.
func TestScannerErrorFrameCountsAsTerminal(t *testing.T) {
	for name, sse := range map[string]string{
		"named event": "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n",
		"data only":   "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if scanAll(t, sse).Truncated() {
				t.Error("error frame not treated as terminal")
			}
		})
	}
}

func TestScannerOpenAIStreamTerminals(t *testing.T) {
	tests := map[string]struct {
		sse       string
		truncated bool
		content   bool
	}{
		"finish_reason ends turn": {
			sse:       "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
			truncated: false,
			content:   true,
		},
		"[DONE] ends turn": {
			sse:       "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			truncated: false,
			content:   true,
		},
		"content then silence": {
			sse:       "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
			truncated: true,
			content:   true,
		},
		"reasoning then silence": {
			sse:       "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"},\"finish_reason\":null}]}\n\n",
			truncated: true,
			content:   false,
		},
		"tool call counts as content": {
			sse:       "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\"}]},\"finish_reason\":null}]}\n\n",
			truncated: true,
			content:   true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := scanAll(t, tc.sse)
			if got := s.Truncated(); got != tc.truncated {
				t.Errorf("Truncated() = %v, want %v", got, tc.truncated)
			}
			if got := s.hadContent(); got != tc.content {
				t.Errorf("hadContent() = %v, want %v", got, tc.content)
			}
		})
	}
}

// The relay reads 16KB at a time, so frames are split at arbitrary offsets. A
// terminal frame cut across two writes must still be seen — otherwise a healthy
// stream is reported as truncated.
func TestScannerTerminalAcrossChunkBoundary(t *testing.T) {
	full := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"
	for split := 1; split < len(full); split++ {
		s := &usageScanner{}
		if _, err := s.Write([]byte(full[:split])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := s.Write([]byte(full[split:])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if s.Truncated() {
			t.Fatalf("split at %d: terminal frame missed", split)
		}
	}
}

// A terminal frame arriving without a trailing newline must still be seen; the
// scanner only inspects the partial buffer when an accessor forces a flush.
func TestScannerTerminalWithoutTrailingNewline(t *testing.T) {
	s := scanAll(t, "data: {\"type\":\"message_stop\"}")
	if s.Truncated() {
		t.Error("terminal frame without trailing newline missed")
	}
}

// The accessors each flush the partial buffer, so calling them in any order (and
// repeatedly, as the relay does) must not change the verdict.
func TestScannerAccessorsIdempotent(t *testing.T) {
	s := scanAll(t, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}")
	first := s.Truncated()
	if !first {
		t.Fatal("expected truncated")
	}
	if !s.hadContent() {
		t.Error("hadContent lost after Truncated flushed the buffer")
	}
	if s.Truncated() != first {
		t.Error("Truncated() changed on second call")
	}
	if s.Counts().Input != 0 {
		t.Error("unexpected usage")
	}
}

// An empty stream (headers, then immediate EOF) is truncated with no content.
func TestScannerEmptyStreamIsTruncated(t *testing.T) {
	s := &usageScanner{}
	if !s.Truncated() {
		t.Error("empty stream not flagged as truncated")
	}
	if s.hadContent() {
		t.Error("empty stream reported content")
	}
}

// Keep-alive comments and blank lines must not be mistaken for frames.
func TestScannerIgnoresCommentsAndPings(t *testing.T) {
	s := scanAll(t, ": keep-alive\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\n")
	if !s.Truncated() {
		t.Error("ping-only stream should still be truncated")
	}
	if s.hadContent() {
		t.Error("ping registered as content")
	}
}

// CRLF line endings are legal SSE and appear in the wild behind some proxies.
func TestScannerHandlesCRLFTerminal(t *testing.T) {
	s := scanAll(t, "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n")
	if s.Truncated() {
		t.Error("CRLF terminal frame missed")
	}
}
