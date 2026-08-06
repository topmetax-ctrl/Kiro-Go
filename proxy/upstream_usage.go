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

// scanLine inspects one complete SSE line for a usage object.
func (s *usageScanner) scanLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return
	}
	// Only "data:" frames carry JSON payloads; "event:" / ":" comments do not.
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(prefix):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	// Cheap pre-filter: skip the JSON decode for the vast majority of frames
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

// Counts returns the usage seen so far, after flushing any trailing partial line
// that arrived without a final newline.
func (s *usageScanner) Counts() usageCounts {
	if len(s.partial) > 0 {
		s.scanLine(s.partial)
		s.partial = s.partial[:0]
	}
	return s.counts
}
