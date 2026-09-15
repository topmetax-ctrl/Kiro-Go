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
//
// Canonical semantics (matching OpenTelemetry GenAI conventions):
// Input and Output are TOTALS. CacheRead and CacheCreation are subsets of
// Input; Reasoning is a subset of Output. Per-dialect derivation happens in
// usageEnvelope.normalize, so these invariants hold no matter which provider
// dialect the response spoke.
type usageCounts struct {
	Input  int64
	Output int64
	// Server-side tool uses parsed from usage.server_tool_use (Anthropic only).
	ServerTool upstreamToolUsage

	// The subset figures are pointers: nil means the upstream did not report
	// the figure at all, a non-nil zero means it reported an exact zero.
	// Collapsing either into a bare int64 would make "provider says zero cache"
	// indistinguishable from "provider says nothing", which is the difference
	// between "0 · 0%" and "—" on the activity table.
	CacheReadInputTokens     *int64
	CacheCreationInputTokens *int64
	ReasoningOutputTokens    *int64
	// Protocol names the response dialect the figures were read from. It is
	// detected from the wire shape, never from the provider's display name, so
	// any provider speaking a known dialect is covered without per-name hacks.
	// Empty until the first usage frame is seen.
	Protocol string
}

func (u usageCounts) known() bool { return u.Input > 0 || u.Output > 0 }

// cacheKnown reports whether the upstream reported a cache breakdown at all.
// Only then is a cache-hit ratio meaningful (rather than zero).
func (u usageCounts) cacheKnown() bool {
	return u.CacheReadInputTokens != nil || u.CacheCreationInputTokens != nil
}

// cacheReadOrZero dereferences the cache-read figure for callers that have
// already established a breakdown was reported.
func (u usageCounts) cacheReadOrZero() int64 {
	if u.CacheReadInputTokens == nil {
		return 0
	}
	return *u.CacheReadInputTokens
}

// usageEnvelope covers every provider dialect this relay can receive in one
// decode. Presence-sensitive fields are pointers: absent decodes to nil, an
// explicit zero decodes to a non-nil zero, and that distinction is exactly the
// "0% vs unknown" cache question. Every shape is documented upstream behavior,
// not guesswork:
//
//   - Anthropic Messages: {input_tokens, output_tokens, cache_creation_input_tokens,
//     cache_read_input_tokens, server_tool_use}. Cached tokens are reported OUTSIDE
//     input_tokens; the canonical total is their sum.
//   - OpenAI Chat Completions: {prompt_tokens, completion_tokens,
//     prompt_tokens_details{cached_tokens, cache_write_tokens},
//     completion_tokens_details{reasoning_tokens}}. Details are subsets of the
//     parent totals (cached tokens are already inside prompt_tokens).
//   - OpenAI Responses: {input_tokens, output_tokens,
//     input_tokens_details{cached_tokens, cache_write_tokens},
//     output_tokens_details{reasoning_tokens}} — subsets again.
//   - DeepSeek: {prompt_tokens, completion_tokens, prompt_cache_hit_tokens,
//     prompt_cache_miss_tokens} with prompt_tokens = hit + miss.
//   - Gemini generateContent: {usageMetadata:{promptTokenCount, candidatesTokenCount,
//     cachedContentTokenCount, thoughtsTokenCount}}; cachedContentTokenCount is part
//     of promptTokenCount.
type usageEnvelope struct {
	// Anthropic Messages (and, by name collision, OpenAI Responses) totals.
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	// Anthropic prompt caching: input tokens the upstream billed separately from
	// input_tokens. Their presence is what marks the Anthropic dialect.
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`

	// OpenAI Chat Completions
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`

	// OpenAI Responses details (subsets of input_tokens / output_tokens).
	InputTokensDetails struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`

	// DeepSeek cache extensions
	PromptCacheHitTokens  *int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens *int64 `json:"prompt_cache_miss_tokens"`

	// Gemini generateContent usageMetadata (camelCase, "Count" suffix).
	PromptTokenCount        *int64 `json:"promptTokenCount"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount"`

	// Gemini nests its usage one level deeper than the other dialects.
	UsageMetadata *usageEnvelope `json:"usageMetadata"`

	// Server-side tool usage (Anthropic usage.server_tool_use)
	ServerToolUse struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

// protocol names label the dialects for metrics.Event usage attribution.
const (
	protocolAnthropic = "anthropic"
	protocolOpenAI    = "openai"
	protocolResponses = "responses"
	protocolGemini    = "gemini"
)

// normalize converts one decoded envelope into the canonical usage. The
// dialect branches are separate because the semantics genuinely differ: only
// Anthropic reports cached input outside its input total.
//
// An invariant violation (cache reads exceeding total input) is logged once and
// left unclamped: the raw numbers are the evidence, and a ratio above 100% on
// the activity table is itself the visible symptom of a lying upstream.
func (e usageEnvelope) normalize() usageCounts {
	c := usageCounts{}

	// DeepSeek first: its responses also carry prompt_tokens, and its cache
	// figures are the top-level extension fields.
	switch {
	case e.PromptCacheHitTokens != nil || e.PromptCacheMissTokens != nil:
		c.Input = derefOr(e.PromptTokens, derefOr(e.PromptCacheHitTokens, 0)+derefOr(e.PromptCacheMissTokens, 0))
		c.CacheReadInputTokens = e.PromptCacheHitTokens
		c.Output = derefOr(e.CompletionTokens, 0)
		c.ReasoningOutputTokens = e.CompletionTokensDetails.ReasoningTokens
		c.Protocol = protocolOpenAI

	case e.PromptTokens != nil || e.CompletionTokens != nil:
		// OpenAI Chat Completions: details are subsets, never additions.
		c.Input = derefOr(e.PromptTokens, 0)
		c.Output = derefOr(e.CompletionTokens, 0)
		c.CacheReadInputTokens = e.PromptTokensDetails.CachedTokens
		c.CacheCreationInputTokens = e.PromptTokensDetails.CacheWriteTokens
		c.ReasoningOutputTokens = e.CompletionTokensDetails.ReasoningTokens
		c.Protocol = protocolOpenAI

	case e.CacheReadInputTokens != nil || e.CacheCreationInputTokens != nil:
		// Anthropic Messages: cached input is reported OUTSIDE input_tokens, so
		// the canonical total is the sum. This branch must run before the plain
		// input_tokens fallback below or the totals would lose the cache.
		c.Input = derefOr(e.InputTokens, 0)
		if e.CacheReadInputTokens != nil {
			c.Input += *e.CacheReadInputTokens
		}
		if e.CacheCreationInputTokens != nil {
			c.Input += *e.CacheCreationInputTokens
		}
		c.CacheReadInputTokens = e.CacheReadInputTokens
		c.CacheCreationInputTokens = e.CacheCreationInputTokens
		c.Output = derefOr(e.OutputTokens, 0)
		c.Protocol = protocolAnthropic

	case e.InputTokensDetails.CachedTokens != nil || e.InputTokensDetails.CacheWriteTokens != nil:
		// OpenAI Responses: input_tokens already includes the cached share.
		c.Input = derefOr(e.InputTokens, 0)
		c.CacheReadInputTokens = e.InputTokensDetails.CachedTokens
		c.CacheCreationInputTokens = e.InputTokensDetails.CacheWriteTokens
		c.Output = derefOr(e.OutputTokens, 0)
		c.ReasoningOutputTokens = e.OutputTokensDetails.ReasoningTokens
		c.Protocol = protocolResponses

	case e.InputTokens != nil || e.OutputTokens != nil:
		// Bare input_tokens/output_tokens with no cache detail: either an
		// Anthropic response without caching or an OpenAI Responses one. The
		// totals read identically under both semantics, so no dialect call is
		// made here; the caller resolves the protocol from the request path
		// (usageProtocol) rather than guessing.
		c.Input = derefOr(e.InputTokens, 0)
		c.Output = derefOr(e.OutputTokens, 0)

	case geminiUsagePresent(e):
		// Gemini generateContent: promptTokenCount already includes the cached
		// share, and thoughtsTokenCount is part of the output side.
		c.Input = derefOr(e.PromptTokenCount, 0)
		c.CacheReadInputTokens = e.CachedContentTokenCount
		c.Output = derefOr(e.CandidatesTokenCount, 0)
		c.ReasoningOutputTokens = e.ThoughtsTokenCount
		c.Protocol = protocolGemini

	default:
		// usageMetadata nested one level deeper (non-stream Gemini body).
		if e.UsageMetadata != nil {
			c = e.UsageMetadata.normalize()
		}
	}

	// Anthropic reports server_tool_use; other dialects leave it zero. Applied
	// after the switch so the early default branch keeps it too.
	c.ServerTool = extractServerToolUse(e)
	if e.UsageMetadata != nil && c.ServerTool.WebSearchRequests == 0 {
		c.ServerTool = extractServerToolUse(*e.UsageMetadata)
	}
	return c
}

// geminiUsagePresent reports whether the envelope carries Gemini's camelCase
// usageMetadata fields, which no other dialect uses.
func geminiUsagePresent(e usageEnvelope) bool {
	return e.PromptTokenCount != nil || e.CandidatesTokenCount != nil || e.CachedContentTokenCount != nil
}

func derefOr(p *int64, or int64) int64 {
	if p != nil {
		return *p
	}
	return or
}

// usageProtocol resolves the protocol label for a parsed usage: the
// shape-detected dialect when the response shape was unambiguous, otherwise the
// dialect the request path implied (/messages speaks Anthropic, /responses
// speaks OpenAI Responses, anything else OpenAI Chat). This is a property of
// the wire protocol, never of the provider's display name.
func usageProtocol(u usageCounts, subPath string) string {
	if u.Protocol != "" {
		return u.Protocol
	}
	switch subPath {
	case "/messages":
		return protocolAnthropic
	case "/responses":
		return protocolResponses
	default:
		return protocolOpenAI
	}
}

// usageFromJSONBody extracts token counts from a buffered non-stream response
// body. It looks for a top-level "usage" object, which is where both dialects
// put it. Returns a zero value when the body is not JSON or carries no usage.

// upstreamToolUsage is the server-side tool usage extracted from an upstream
// response. Only the logical use count is observable; the upstream controls
// execution and reports no backend/cache details.
type upstreamToolUsage struct {
	WebSearchRequests int64
}

func (u *upstreamToolUsage) merge(other upstreamToolUsage) {
	if other.WebSearchRequests > 0 {
		u.WebSearchRequests = other.WebSearchRequests
	}
}

// extractServerToolUse extracts server-side tool usage from a full upstream
// usage envelope. Returns zero when absent.
func extractServerToolUse(env usageEnvelope) upstreamToolUsage {
	return upstreamToolUsage{
		WebSearchRequests: env.ServerToolUse.WebSearchRequests,
	}
}

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
	c := envelope.Usage.normalize()
	if c.Protocol == "" {
		// Non-stream Gemini bodies nest usage under "usageMetadata" instead.
		var gem struct {
			UsageMetadata usageEnvelope `json:"usageMetadata"`
		}
		if json.Unmarshal(body, &gem) == nil {
			g := gem.UsageMetadata.normalize()
			if g.Protocol != "" {
				g.Protocol = protocolGemini
				return g
			}
		}
	}
	return c
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
// Frame merge semantics per field: every supported dialect reports usage
// cumulatively on the final authoritative frame (Anthropic's message_start
// carries the input side and its message_delta may carry the full final usage,
// OpenAI sends one usage object in the last chunk, Gemini one usageMetadata at
// the end), so the merge keeps the LAST reported value per field — never a
// sum. Presence-sensitive fields (cache, reasoning) merge per-field: a frame
// reporting only output_tokens leaves an already-seen cache breakdown intact,
// and a later frame reporting an explicit cache zero overwrites an earlier
// nonzero value, because the later frame is the authoritative one.
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
		Response struct {
			Usage usageEnvelope `json:"usage"`
		} `json:"response"`
		// Gemini chunks nest the dialect's fields under usageMetadata; the
		// envelope reuses that field name for the same purpose.
		UsageMetadata *usageEnvelope `json:"usageMetadata"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return
	}

	// Anthropic message_start nests usage under "message"; message_delta and the
	// OpenAI final chunk put it at the top level; OpenAI Responses' terminal
	// response.completed event nests it under "response"; Gemini chunks carry
	// usageMetadata.
	mergeUsage(&s.counts, frame.Message.Usage)
	mergeUsage(&s.counts, frame.Usage)
	mergeUsage(&s.counts, frame.Response.Usage)
	if frame.UsageMetadata != nil {
		mergeUsage(&s.counts, *frame.UsageMetadata)
	}
}

// mergeUsage folds one frame's normalized usage into the running totals.
// Input/Output merge last-wins (cumulative); the pointer fields merge
// last-NON-NIL-wins so a frame reporting only an output total cannot erase a
// cache breakdown an earlier frame carried, while an explicit later zero does
// overwrite.
func mergeUsage(dst *usageCounts, env usageEnvelope) {
	c := env.normalize()
	if c.Input > 0 {
		dst.Input = c.Input
	}
	if c.Output > 0 {
		dst.Output = c.Output
	}
	if c.CacheReadInputTokens != nil {
		dst.CacheReadInputTokens = c.CacheReadInputTokens
	}
	if c.CacheCreationInputTokens != nil {
		dst.CacheCreationInputTokens = c.CacheCreationInputTokens
	}
	if c.ReasoningOutputTokens != nil {
		dst.ReasoningOutputTokens = c.ReasoningOutputTokens
	}
	if c.Protocol != "" {
		dst.Protocol = c.Protocol
	}
	if c.ServerTool.WebSearchRequests > 0 {
		dst.ServerTool.WebSearchRequests = c.ServerTool.WebSearchRequests
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
