package proxy

// Token-usage extraction for the upstream-forwarding path.
//
// Forwarded requests are relayed verbatim, so the only place real token counts
// exist is the upstream's own response. This file pulls them out of both
// supported response shapes without changing a single relayed byte:
//
//   - Non-stream: the buffered JSON body carries a "usage" object. Anthropic uses
//     {input_tokens, output_tokens}; OpenAI uses {prompt_tokens, completion_tokens}.
//   - Stream: usage arrives in a late SSE frame — Anthropic's message_delta (and
//     message_start for the input count), OpenAI's final chunk when the caller
//     requested stream_options.include_usage. usageScanner watches the bytes as
//     they pass through the relay and keeps only the last counts it sees.
//
// Both readers are best-effort: a shape we do not recognize yields zero tokens
// rather than an error, because token accounting must never fail a relay.

import (
	"bytes"
	"encoding/json"
	"strings"
)

// usageCounts is the token pair extracted from an upstream response. Zero means
// "not reported", which the metrics layer records as unknown rather than free.
type usageCounts struct {
	Input  int64
	Output int64
}

func (u usageCounts) known() bool { return u.Input > 0 || u.Output > 0 }

// usageEnvelope covers both provider dialects in one decode. Every field is a
// pointer-free int64 because absent keys decode to 0, which is exactly the
// "unknown" sentinel.
type usageEnvelope struct {
	// Anthropic Messages
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// Anthropic prompt caching: these are input tokens the upstream billed
	// separately. Counting them keeps the input total comparable with a
	// non-cached request.
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	// OpenAI chat/completions
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

func (e usageEnvelope) counts() usageCounts {
	in := e.InputTokens + e.CacheCreationInputTokens + e.CacheReadInputTokens
	if in == 0 {
		in = e.PromptTokens
	}
	out := e.OutputTokens
	if out == 0 {
		out = e.CompletionTokens
	}
	return usageCounts{Input: in, Output: out}
}

// usageFromJSONBody extracts token counts from a buffered non-stream response
// body. It looks for a top-level "usage" object, which is where both dialects
// put it. Returns a zero value when the body is not JSON or carries no usage.
func usageFromJSONBody(body []byte) usageCounts {
	if len(body) == 0 {
		return usageCounts{}
	}
	var envelope struct {
		Usage usageEnvelope `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return usageCounts{}
	}
	return envelope.Usage.counts()
}

// maxUsageScanBuffer caps the partial-line buffer held by usageScanner. SSE data
// lines carrying usage are small (a few hundred bytes); a line larger than this
// is content, not accounting, so we drop it rather than grow without bound on a
// pathological upstream that never emits a newline.
const maxUsageScanBuffer = 64 * 1024

// usageScanner extracts token usage from an SSE byte stream as it is relayed.
//
// It never buffers the whole stream: it holds at most one partial line and
// forgets each line once inspected. Write is called with the same slices handed
// to the client, so scanning cannot alter or delay the relay — the caller writes
// to the client first and feeds the scanner after.
//
// Later frames overwrite earlier ones because both dialects report cumulative
// (not incremental) usage: Anthropic's message_start carries input tokens and
// its final message_delta carries the output total; OpenAI sends one usage
// object in the last chunk.
type usageScanner struct {
	partial []byte
	counts  usageCounts

	// sawContent records whether the stream delivered any client-visible answer:
	// assistant text, or a tool call. Thinking/reasoning deltas deliberately do
	// NOT set it — a stream that reasons and then dies before answering is the
	// exact failure this tracking exists to catch, so counting reasoning as
	// content would make that case indistinguishable from a real answer.
	sawContent bool
	// sawTerminal records whether the upstream sent a frame that means "this turn
	// is over": Anthropic's message_stop or a message_delta carrying stop_reason,
	// OpenAI's [DONE] or a non-null finish_reason, or an explicit error event.
	// Its absence at EOF is how a truncated relay is detected.
	sawTerminal bool
}

// Write feeds relayed bytes to the scanner. It always reports success: this is
// an observer, and a parse problem must never surface as a relay error.
func (s *usageScanner) Write(p []byte) (int, error) {
	if len(s.partial)+len(p) > maxUsageScanBuffer {
		// Keep only the tail: usage frames are short, so a boundary split can be
		// recovered from the last bytes, while a giant content line is discarded.
		s.partial = s.partial[:0]
		if len(p) > maxUsageScanBuffer {
			p = p[len(p)-maxUsageScanBuffer:]
		}
	}
	s.partial = append(s.partial, p...)

	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := s.partial[:i]
		s.partial = s.partial[i+1:]
		s.scanLine(line)
	}
}

// scanLine inspects one complete SSE line for a usage object and for the
// content/terminal markers that decide whether the stream finished cleanly.
func (s *usageScanner) scanLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return
	}
	// An SSE "event:" line names the frame type. Anthropic sends the terminal
	// signal as `event: message_stop`, and signals a mid-stream failure as
	// `event: error`; both settle the turn without any "usage" payload, so they
	// must be read here rather than in the JSON branch below.
	if rest, ok := bytes.CutPrefix(line, []byte("event:")); ok {
		switch string(bytes.TrimSpace(rest)) {
		case "message_stop", "error":
			s.sawTerminal = true
		}
		return
	}
	// Only "data:" frames carry JSON payloads; ":" comments do not.
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(prefix):])
	if len(payload) == 0 {
		return
	}
	// OpenAI's stream sentinel: a clean end of turn with no JSON to decode.
	if bytes.Equal(payload, []byte("[DONE]")) {
		s.sawTerminal = true
		return
	}

	s.scanFrameMarkers(payload)

	// Cheap pre-filter: skip the usage decode for the vast majority of frames
	// (content deltas), which never mention usage.
	if !bytes.Contains(payload, []byte("usage")) {
		return
	}

	var frame struct {
		Usage   usageEnvelope `json:"usage"`
		Message struct {
			Usage usageEnvelope `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return
	}

	// Anthropic message_start nests usage under "message"; message_delta and the
	// OpenAI final chunk put it at the top level.
	for _, c := range []usageCounts{frame.Message.Usage.counts(), frame.Usage.counts()} {
		if c.Input > 0 {
			s.counts.Input = c.Input
		}
		if c.Output > 0 {
			s.counts.Output = c.Output
		}
	}
}

// scanFrameMarkers decodes one data frame far enough to tell whether it carried
// client-visible content or ended the turn. It is best-effort in the same way as
// the usage decode: an unrecognized or malformed shape simply sets nothing.
//
// Both dialects are handled in a single decode because the field sets do not
// collide: Anthropic uses type/delta/content_block, OpenAI uses choices[].
func (s *usageScanner) scanFrameMarkers(payload []byte) {
	// Pre-filter: only frames mentioning one of these keys can move either flag,
	// which skips the decode for keep-alive pings and similar noise.
	if !bytes.Contains(payload, []byte("type")) && !bytes.Contains(payload, []byte("choices")) {
		return
	}

	var frame struct {
		Type  string `json:"type"`
		Delta struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return
	}

	switch frame.Type {
	case "message_stop":
		s.sawTerminal = true
	case "error":
		// A mid-stream error frame settles the turn: the client is being told the
		// request failed, which is a complete (if unhappy) outcome, not a silent
		// truncation.
		s.sawTerminal = true
	case "message_delta":
		// stop_reason on a message_delta is Anthropic's real end-of-turn signal.
		if strings.TrimSpace(frame.Delta.StopReason) != "" {
			s.sawTerminal = true
		}
	case "content_block_start":
		// A tool call is client-visible output even though it carries no text.
		if frame.ContentBlock.Type == "tool_use" {
			s.sawContent = true
		}
	case "content_block_delta":
		// text_delta is an answer; thinking_delta and signature_delta are not.
		// input_json_delta streams a tool call's arguments, which counts.
		switch frame.Delta.Type {
		case "text_delta":
			if frame.Delta.Text != "" {
				s.sawContent = true
			}
		case "input_json_delta":
			s.sawContent = true
		}
	}

	for i := range frame.Choices {
		c := &frame.Choices[i]
		if strings.TrimSpace(c.FinishReason) != "" {
			s.sawTerminal = true
		}
		if c.Delta.Content != "" || len(c.Delta.ToolCalls) > 0 {
			s.sawContent = true
		}
	}
}

// Counts returns the usage seen so far, after flushing any trailing partial line
// that arrived without a final newline.
func (s *usageScanner) Counts() usageCounts {
	s.flushPartial()
	return s.counts
}

// Truncated reports whether the relayed stream ended without any terminal frame.
//
// This is the forwarding path's equivalent of classifyStreamIntegrity on the
// Kiro pool path (proxy/account_failover.go): a provider that closes the
// connection cleanly after streaming reasoning — but before the answer — yields
// io.EOF, which is otherwise indistinguishable from a finished turn. The relay
// would then be recorded as a 200 success while the client sees a stream that
// simply stops, with no error to explain it.
//
// Deliberately conservative: only the absence of a terminal marker counts. A
// turn that produced no content but did terminate (a refusal, an empty answer,
// an error frame) is left alone, so this can only fire on a genuinely
// unterminated stream. hadContent distinguishes the two shapes for the log/metric
// without changing the verdict.
func (s *usageScanner) Truncated() bool {
	s.flushPartial()
	return !s.sawTerminal
}

// hadContent reports whether any client-visible answer (text or tool call) was
// relayed. Used only to describe a truncation, never to decide one.
func (s *usageScanner) hadContent() bool {
	s.flushPartial()
	return s.sawContent
}

// flushPartial scans a trailing line that arrived without a final newline. It is
// idempotent, so the several accessors that need it can each call it.
func (s *usageScanner) flushPartial() {
	if len(s.partial) > 0 {
		line := s.partial
		s.partial = s.partial[:0]
		s.scanLine(line)
	}
}
