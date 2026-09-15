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
//     they pass through the relay and keeps only the last counts it sees. A data
//     line too large for the scan buffer (OpenAI Responses' response.completed
//     carries the whole generated output beside its usage) is streamed through
//     bigLineFilter, which extracts the usage object without buffering the line.
//
// Both readers are best-effort: a shape we do not recognize yields zero tokens
// rather than an error, because token accounting must never fail a relay.

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync/atomic"
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
	// Totals are pointers, same presence encoding as the subsets below: nil
	// means the upstream did not report the figure at all, a non-nil zero means
	// it reported an exact zero. Keeping presence here — rather than collapsing
	// it into a bare int64 — is what lets the activity table show "0" for a
	// reported zero and "—" for unknown. Every supported dialect reports
	// cumulative totals on its authoritative frames, so an explicit zero IS the
	// upstream's answer whenever it says one; placeholder-zero suppression
	// belongs to the stream merge, which sees whole frames, not to this parser.
	Input  *int64
	Output *int64
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

func (u usageCounts) known() bool { return u.Input != nil || u.Output != nil }

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
// Anthropic reports cached input outside its input total, and only Gemini
// reports reasoning tokens OUTSIDE its candidate output count.
//
// Totals keep their wire presence exactly (nil stays nil, an explicit zero
// stays a pointer to zero): placeholder suppression is the stream merge's job.
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
		c.Input = firstNonNil(e.PromptTokens, sumIntPtr(e.PromptCacheHitTokens, e.PromptCacheMissTokens))
		c.CacheReadInputTokens = e.PromptCacheHitTokens
		c.Output = e.CompletionTokens
		c.ReasoningOutputTokens = e.CompletionTokensDetails.ReasoningTokens
		c.Protocol = protocolOpenAI

	case e.PromptTokens != nil || e.CompletionTokens != nil:
		// OpenAI Chat Completions: details are subsets, never additions.
		c.Input = e.PromptTokens
		c.Output = e.CompletionTokens
		c.CacheReadInputTokens = e.PromptTokensDetails.CachedTokens
		c.CacheCreationInputTokens = e.PromptTokensDetails.CacheWriteTokens
		c.ReasoningOutputTokens = e.CompletionTokensDetails.ReasoningTokens
		c.Protocol = protocolOpenAI

	case e.CacheReadInputTokens != nil || e.CacheCreationInputTokens != nil:
		// Anthropic Messages: cached input is reported OUTSIDE input_tokens, so
		// the canonical total is the sum. This branch must run before the plain
		// input_tokens fallback below or the totals would lose the cache.
		c.Input = sumIntPtr(e.InputTokens, e.CacheReadInputTokens, e.CacheCreationInputTokens)
		c.CacheReadInputTokens = e.CacheReadInputTokens
		c.CacheCreationInputTokens = e.CacheCreationInputTokens
		c.Output = e.OutputTokens
		c.Protocol = protocolAnthropic

	case e.InputTokensDetails.CachedTokens != nil || e.InputTokensDetails.CacheWriteTokens != nil:
		// OpenAI Responses: input_tokens already includes the cached share.
		c.Input = e.InputTokens
		c.CacheReadInputTokens = e.InputTokensDetails.CachedTokens
		c.CacheCreationInputTokens = e.InputTokensDetails.CacheWriteTokens
		c.Output = e.OutputTokens
		c.ReasoningOutputTokens = e.OutputTokensDetails.ReasoningTokens
		c.Protocol = protocolResponses

	case e.InputTokens != nil || e.OutputTokens != nil:
		// Bare input_tokens/output_tokens with no cache detail: either an
		// Anthropic response without caching or an OpenAI Responses one. The
		// totals read identically under both semantics, so no dialect call is
		// made here; the caller resolves the protocol from the request path
		// (usageProtocol) rather than guessing.
		c.Input = e.InputTokens
		c.Output = e.OutputTokens

	case geminiUsagePresent(e):
		// Gemini generateContent. Verified against the official reference
		// (ai.google.dev/api/generate-content): totalTokenCount is documented
		// as "prompt + thoughts + response candidates" — three ADDITIVE
		// components — and the thinking guide bills "output tokens AND
		// thinking tokens" separately. thoughtsTokenCount is therefore NOT
		// inside candidatesTokenCount, so the canonical output side is their
		// sum; counting candidates alone would understate output and let
		// reasoning exceed it (a 1000-candidate/4000-thought answer would
		// record reasoning 4000 > output 1000).
		c.Input = e.PromptTokenCount
		c.CacheReadInputTokens = e.CachedContentTokenCount
		c.Output = sumIntPtr(e.CandidatesTokenCount, e.ThoughtsTokenCount)
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
	return e.PromptTokenCount != nil || e.CandidatesTokenCount != nil ||
		e.CachedContentTokenCount != nil || e.ThoughtsTokenCount != nil
}

func derefOr(p *int64, or int64) int64 {
	if p != nil {
		return *p
	}
	return or
}

// firstNonNil returns the first non-nil pointer, or nil when every argument is
// absent. It preserves presence: a reported zero is returned as a reported
// zero, not collapsed into a fallback's zero.
func firstNonNil(ps ...*int64) *int64 {
	for _, p := range ps {
		if p != nil {
			return p
		}
	}
	return nil
}

// sumIntPtr adds the pointed-at values, treating nil as absent rather than
// zero. It returns nil only when every argument is absent — the sum keeps
// presence whenever any component was reported. Used for totals that the wire
// reports as separate additive components (Anthropic's input + cache fields,
// Gemini's candidates + thoughts).
func sumIntPtr(ps ...*int64) *int64 {
	any := false
	sum := int64(0)
	for _, p := range ps {
		if p != nil {
			any = true
			sum += *p
		}
	}
	if !any {
		return nil
	}
	return &sum
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

// streamTerminalKind is the GENERATION outcome the stream itself reported, as
// distinct from transport integrity. Official Responses semantics (and their
// dialect equivalents) end a turn three different ways, and they are not the
// same outcome: response.completed is a full answer, response.failed is the
// upstream reporting its own generation error, and response.incomplete is a
// turn cut short by a documented limit (incomplete_details.reason, e.g.
// max_output_tokens). All three settle the transport — none of them means the
// stream was silently cut — but treating failed the same as completed would
// record a provider's own error as a success, and treating incomplete as an
// outage would cooldown a healthy provider for enforcing its output limit.
//
// The kind never changes what bytes reach the client: it only labels the
// recorded metric and decides whether an upstream-reported failure counts
// against provider health.
type streamTerminalKind uint8

const (
	// terminalNone: no terminal frame was ever seen. The stream ended without
	// the upstream saying why — the only state Truncated() reports.
	terminalNone streamTerminalKind = iota
	terminalCompleted
	terminalFailed
	terminalIncomplete
)

// String renders the kind for metrics.Event.StreamOutcome. The empty string is
// reserved for "no terminal seen / not a stream", so an old event or a
// truncated one reads as unknown rather than as a fabricated outcome.
func (k streamTerminalKind) String() string {
	switch k {
	case terminalCompleted:
		return "completed"
	case terminalFailed:
		return "failed"
	case terminalIncomplete:
		return "incomplete"
	default:
		return ""
	}
}

// terminalFromTypeName maps a wire frame-type name to its terminal kind. It is
// shared by the named-event reader, the data-frame decoder and the oversized
// line probe so all three paths classify identically. Names outside the
// terminal set (message_start, ping, …) return false and change nothing.
func terminalFromTypeName(name string) (streamTerminalKind, bool) {
	switch name {
	case "message_stop", "response.completed":
		return terminalCompleted, true
	case "error", "response.failed":
		return terminalFailed, true
	case "response.incomplete":
		return terminalIncomplete, true
	default:
		return terminalNone, false
	}
}

// limitStopReasons are the Anthropic stop_reason / OpenAI finish_reason values
// that mean "the turn ended because a configured limit was hit". These are the
// chat-completions dialects' equivalent of Responses'
// incomplete_details.reason=max_output_tokens: the provider is healthy, the
// turn simply reached its cap. They map to terminalIncomplete, everything else
// maps to terminalCompleted.
var limitStopReasons = map[string]bool{
	"max_tokens":     true, // Anthropic
	"length":         true, // OpenAI chat
	"content_filter": true, // OpenAI chat (limit-triggered refusal)
}

// Degradation counters for the usage observer. The observer is best-effort by
// contract — it must never fail a relay — which means when it loses usage it
// loses it SILENTLY unless something counts the loss. These are that something:
// low-cardinality, reason-labeled counters in the house style of
// providererr's classifiedTotal (plain atomic counters plus reader functions,
// no new framework, no per-request labels). The reasons are exactly the ways
// the observer can give up on usage it saw evidence of:
type usageObserverReason uint8

const (
	// degradeOversizedUsage: a usage object captured from an oversized line
	// outgrew maxUsageCapture and was abandoned whole.
	degradeOversizedUsage usageObserverReason = iota
	// degradeMalformedUsage: a captured usage object did not decode. The
	// capture is verbatim wire bytes, so this means a hostile or corrupt
	// upstream, never a scanner bug per se.
	degradeMalformedUsage
	// degradeUnsupportedEncoding: the upstream answered with a Content-Encoding
	// the transport does not decode transparently, so the observer sees
	// compressed bytes and can extract nothing.
	degradeUnsupportedEncoding
	usageObserverReasonCount
)

var usageObserverDegrades [usageObserverReasonCount]atomic.Int64

func incUsageObserverDegrade(r usageObserverReason) {
	usageObserverDegrades[r].Add(1)
}

// UsageObserverDegradeTotals returns the lifetime counts of lost-usage events
// by reason, for admin introspection. Index order: oversized usage, malformed
// usage JSON, unsupported content encoding.
func UsageObserverDegradeTotals() [usageObserverReasonCount]int64 {
	var out [usageObserverReasonCount]int64
	for i := range usageObserverDegrades {
		out[i] = usageObserverDegrades[i].Load()
	}
	return out
}

// maxUsageScanBuffer caps the partial-line buffer held by usageScanner. SSE data
// lines carrying usage are small (a few hundred bytes); a line larger than this
// is content-heavy, so instead of growing without bound the scanner hands the
// line to bigLineFilter, which needs no buffering at all.
const maxUsageScanBuffer = 64 * 1024

// maxUsageCapture caps one usage object captured from an oversized line. Real
// usage objects are a few hundred bytes; the cap exists so a hostile or
// pathological frame cannot turn the observer into a buffer. A capture that
// exceeds it is abandoned whole — nothing partial is ever decoded.
const maxUsageCapture = 16 * 1024

// usageScanner extracts token usage from an SSE byte stream as it is relayed.
//
// It never buffers the whole stream: it holds at most one partial line, and a
// line that outgrows that buffer is streamed through bigLineFilter, which
// retains nothing but scanner state and the current usage object. Write is
// called with the same slices handed to the client, so scanning cannot alter or
// delay the relay — the caller writes to the client first and feeds the scanner
// after.
//
// Frame merge semantics per field: every supported dialect reports usage
// cumulatively on its usage-bearing frames (Anthropic's message_delta usage is
// documented cumulative and message_start opens the stream with the input side,
// OpenAI sends one usage object in the last chunk, Responses' response.completed
// carries the full usage, DeepSeek's last pre-[DONE] chunk carries the entire
// request, Gemini reports usageMetadata on the final chunk), so the merge keeps
// the LAST reported value per field — never a sum.
type usageScanner struct {
	partial []byte
	// big carries the state machine for the one line that outgrew partial.
	// While it is active, partial is empty: every byte of the oversized line
	// flows through the filter instead.
	big    bigLineFilter
	counts usageCounts

	// sawContent records whether the stream delivered any client-visible answer:
	// assistant text, or a tool call. Thinking/reasoning deltas deliberately do
	// NOT set it — a stream that reasons and then dies before answering is the
	// exact failure this tracking exists to catch, so counting reasoning as
	// content would make that case indistinguishable from a real answer.
	sawContent bool
	// terminal records which terminal frame — if any — ended the turn, and
	// therefore the generation outcome the upstream itself reported. None means
	// the stream ended without one (the truncated case). terminalFailed and
	// terminalIncomplete are still terminals: the transport ended cleanly and
	// the client was told the outcome, so neither is a silent truncation — but
	// only terminalCompleted records as a success, and terminalIncomplete never
	// counts against provider health (see the kind's doc above).
	terminal streamTerminalKind
	// streamErrCode carries the upstream's own error type/code from a failed
	// terminal frame ("overloaded_error", "server_error", …). Protocol metadata
	// only — bounded, and never the error message, which can carry upstream
	// topology that must not reach the metrics ring.
	streamErrCode string
}

// noteTerminal settles the turn with the given outcome, keeping the first
// terminal seen (a second terminal frame after the first is noise or a
// mid-stream error already accounted for).
func (s *usageScanner) noteTerminal(k streamTerminalKind) {
	if s.terminal == terminalNone && k != terminalNone {
		s.terminal = k
	}
}

// Write feeds relayed bytes to the scanner. It always reports success: this is
// an observer, and a parse problem must never surface as a relay error.
func (s *usageScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if s.big.active {
			// An oversized line is in progress: stream the rest of it through
			// the filter. feed consumes up to and including the line's newline,
			// after which normal buffered scanning resumes.
			p = p[s.big.feed(s, p):]
			continue
		}
		s.partial = append(s.partial, p...)
		p = nil
		for {
			i := bytes.IndexByte(s.partial, '\n')
			if i < 0 {
				break
			}
			s.scanLine(s.partial[:i])
			s.partial = s.partial[i+1:]
		}
		if len(s.partial) > maxUsageScanBuffer {
			// The growing line will not fit in the buffer — and its beginning,
			// which may already contain the usage object, is exactly the part a
			// keep-the-tail policy would throw away. Hand everything seen so
			// far to the streaming filter and feed the remainder of the line
			// the same way as it arrives.
			s.big.begin(s, s.partial)
			s.partial = s.partial[:0]
		}
	}
	return n, nil
}

// bigLineFilter scans a single oversized SSE data line byte-by-byte and
// captures only the JSON objects the accounting needs: values of the keys
// "usage" and "usageMetadata" at object depth 1 or 2 — the exact positions the
// dialects put them (top level for OpenAI Chat, inside "message" for Anthropic
// message_start, inside "response" for OpenAI Responses, top level for Gemini).
// Deeper matches (e.g. per-item usage inside a future output[] entry) are
// ignored, mirroring what the whole-frame decoder reads.
//
// It exists for one wire shape in particular: OpenAI Responses'
// response.completed, whose "response" object carries the ENTIRE generated
// output alongside the authoritative usage — a long answer makes the frame far
// larger than any sane line buffer, and dropping the frame would drop the only
// usage the stream ever reports. Filtering instead of buffering keeps memory
// bounded (never more than one captured usage object) while never losing that
// usage, and string/escape tracking means a "usage" spelled inside response
// text can never trigger a capture.
//
// Captured objects are merged through the same mergeUsage path as whole-frame
// decodes, in the order they close — so the last one wins, exactly as with
// small frames.
type bigLineFilter struct {
	active bool
	// JSON lexical state.
	inString bool
	escaped  bool
	depth    int
	// Key-recognition state: keyBuf holds the last string's content until the
	// following byte decides whether that string was an object key.
	keyBuf     [16]byte
	keyLen     int
	keyLong    bool
	keyIsUsage bool // the closed string was "usage"/"usageMetadata" at depth ≤ 2
	keySeen    bool // a string just closed; the next bytes decide key vs value
	awaitObj   bool // matched key seen with ':'; waiting for the '{' that opens it
	// parentDialect records that the last depth-1 key was "message" or
	// "response" — the only parents whose nested "usage" the whole-frame
	// decoder reads. A depth-2 "usage" under ANY other parent is ignored, so
	// the filter stays byte-consistent with the small-frame decoder: both
	// extract exactly the dialect positions and nothing more.
	parentDialect bool
	// Capture state.
	capturing    bool
	captureDepth int
	overCap      bool
	capture      []byte
}

// begin hands the accumulated head of an oversized line to the filter. The head
// necessarily starts at the line's first byte (partial is always cleared at
// newlines), so lexical state starts clean — and the head may already contain a
// complete usage object, which is merged immediately.
//
// The head also contains the frame's root "type" field — every dialect leads
// its envelope with it — which is probed HERE because a data-only oversized
// terminal frame (some relays strip event: lines) would otherwise never settle
// the turn: the filter streams the line through without buffering it, so
// scanFrameMarkers never sees the frame, and a legitimate giant
// response.completed would be recorded as a truncation. The probe is bounded:
// it stops after the first few root keys, so it costs nothing regardless of
// how long the line grows.
func (f *bigLineFilter) begin(s *usageScanner, head []byte) {
	f.stop()
	f.active = true
	if kind, known := terminalFromTypeName(probeRootType(head)); known {
		s.noteTerminal(kind)
	}
	f.feed(s, head)
}

// probeRootType reads the FIRST few root-level keys of a (possibly huge) JSON
// object and returns the value of the "type" key if it appears among them.
// Responses and Anthropic envelopes lead with "type", so in practice the very
// first key answers; probing a few guards against cosmetic key-order variance.
// Anything malformed, non-object, or without an early "type" returns "" — the
// caller treats that as "no terminal signal", which is the pre-existing
// behavior for unknown shapes.
func probeRootType(head []byte) string {
	// The scanner's oversized-line head still carries the SSE "data:" prefix;
	// the filter-side caller may have already cut it. A json.Decoder skips
	// leading whitespace itself, so only the prefix needs removing.
	if rest, ok := bytes.CutPrefix(head, []byte("data:")); ok {
		head = rest
	}
	dec := json.NewDecoder(bytes.NewReader(head))
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return ""
	}
	for i := 0; i < 8; i++ {
		key, err := dec.Token()
		if err != nil {
			return ""
		}
		if d, ok := key.(json.Delim); ok && d == '}' {
			return ""
		}
		name, ok := key.(string)
		if !ok {
			return ""
		}
		val, err := dec.Token()
		if err != nil {
			return ""
		}
		if name == "type" {
			if s, ok := val.(string); ok {
				return s
			}
			return ""
		}
	}
	return ""
}

// stop deactivates the filter. Usage objects that already closed were merged
// as they closed; anything still open when the line ends is malformed JSON and
// is abandoned whole.
func (f *bigLineFilter) stop() {
	*f = bigLineFilter{}
}

// feed runs one chunk of the oversized line through the state machine, merging
// each usage object as it closes. It consumes up to and including the line's
// terminating newline and then deactivates itself; the return value is the
// number of bytes consumed.
func (f *bigLineFilter) feed(s *usageScanner, p []byte) int {
	for i, b := range p {
		if b == '\n' {
			// JSON strings cannot contain raw newlines, so a raw one really
			// ends the SSE line. Closed captures were already merged.
			f.stop()
			return i + 1
		}
		f.scanByte(b, s)
	}
	return len(p)
}

// scanByte advances the lexical state machine by one byte. While a capture is
// open every byte is recorded too — the capture is a verbatim copy of the
// object's bytes — and the state machine keeps running underneath so a capture
// abandoned for size still knows where its object ends.
func (f *bigLineFilter) scanByte(b byte, s *usageScanner) {
	if f.capturing {
		f.note(b)
	}
	if f.inString {
		switch {
		case f.escaped:
			f.escaped = false
		case b == '\\':
			f.escaped = true
		case b == '"':
			// The string closed; whether it was a key is decided by the next
			// structural byte (a ':' makes it one). The depth at close time is
			// the depth of the object holding the key, so depth ≤ 2 admits the
			// dialect positions (frame root, or one nesting level in) and
			// rejects deep per-item usage the whole-frame decoder also ignores.
			f.inString = false
			f.keySeen = true
			f.keyIsUsage = !f.keyLong && f.depth <= 2 && f.keyMatches()
			if f.depth == 1 {
				// A depth-1 key names the object any depth-2 usage would sit
				// in. Only the dialect parents the decoder reads may arm one —
				// matched case-insensitively for the same reason keyMatches is.
				f.parentDialect = !f.keyLong &&
					(asciiFoldEqual(f.keyBuf[:f.keyLen], "message") ||
						asciiFoldEqual(f.keyBuf[:f.keyLen], "response"))
			}
		default:
			// Record candidate key bytes. Escaped or oversized strings can
			// never match ("usage" is short and unescaped) — keyLong pins the
			// decision for this string.
			if f.keyLen < len(f.keyBuf) {
				f.keyBuf[f.keyLen] = b
				f.keyLen++
			} else {
				f.keyLong = true
			}
		}
		return
	}
	if f.keySeen && (b == ':' || b == ' ' || b == '\t' || b == '\r') {
		if b == ':' {
			f.keySeen = false
			// Arm a capture only for the positions the whole-frame decoder
			// reads: root-level keys, or "usage" inside message/response.
			if f.keyIsUsage && (f.depth == 1 || f.parentDialect) {
				f.awaitObj = true
			}
		}
		return
	}
	f.keySeen = false
	startCapture := false
	if f.awaitObj {
		if b == ' ' || b == '\t' || b == '\r' {
			return
		}
		f.awaitObj = false
		startCapture = b == '{'
	}
	switch b {
	case '"':
		f.inString = true
		f.keyLen, f.keyLong = 0, false
	case '{', '[':
		f.depth++
		if startCapture && !f.capturing {
			f.capturing = true
			f.overCap = false
			f.capture = f.capture[:0]
			f.captureDepth = f.depth
			f.note('{')
		}
	case '}', ']':
		f.depth--
		if f.capturing && f.depth < f.captureDepth {
			f.finishCapture(s)
		}
		if f.depth == 1 {
			// A nested object under the frame root just closed; whatever key
			// named it no longer governs following depth-2 keys.
			f.parentDialect = false
		}
	}
}

// keyMatches reports whether the buffered key is one the dialects use for their
// usage object. The comparison is case-insensitive because the whole-frame
// decoder is: Go's encoding/json matches struct tags ignoring ASCII case, so a
// small frame carrying {"Usage":…} extracts usage there. The giant-frame path
// must recognize the same keys, or a frame's usage would depend on whether it
// happened to cross the scan buffer — the exact size-dependent divergence the
// oracle fuzz test forbids.
func (f *bigLineFilter) keyMatches() bool {
	return asciiFoldEqual(f.keyBuf[:f.keyLen], "usage") ||
		asciiFoldEqual(f.keyBuf[:f.keyLen], "usagemetadata")
}

// asciiFoldEqual reports whether buf equals want ignoring ASCII case; want must
// already be lowercase. It mirrors encoding/json's case-insensitive key match
// over the small, fixed set of dialect keys this filter recognizes (none of
// which collide under case folding, so the "prefer exact match" rule json also
// applies never changes the result here).
func asciiFoldEqual(buf []byte, want string) bool {
	if len(buf) != len(want) {
		return false
	}
	for i := 0; i < len(buf); i++ {
		c := buf[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

// note records one byte of the capture, abandoning the whole capture once it
// outgrows maxUsageCapture. Abandonment is counted: this is a usage the
// upstream DID report being dropped, and it must not be lost silently.
func (f *bigLineFilter) note(b byte) {
	if f.overCap {
		return
	}
	if len(f.capture) >= maxUsageCapture {
		f.overCap = true
		f.capture = f.capture[:0]
		incUsageObserverDegrade(degradeOversizedUsage)
		return
	}
	f.capture = append(f.capture, b)
}

// finishCapture decodes one closed usage object and folds it into the running
// counts. A capture abandoned for size is discarded whole: nothing is ever
// decoded from a truncated body, and an object that large is not accounting.
// A capture that fails to DECODE whole is counted as malformed — verbatim wire
// bytes that do not parse mean a corrupt or hostile upstream.
func (f *bigLineFilter) finishCapture(s *usageScanner) {
	f.capturing = false
	if f.overCap {
		return
	}
	var env usageEnvelope
	if err := json.Unmarshal(f.capture, &env); err != nil {
		incUsageObserverDegrade(degradeMalformedUsage)
		return
	}
	mergeUsage(&s.counts, env)
}

// scanLine inspects one complete SSE line for a usage object and for the
// content/terminal markers that decide whether the stream finished cleanly.
func (s *usageScanner) scanLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return
	}
	// An SSE "event:" line names the frame type. Several of these settle the
	// turn without any "usage" payload and must be read here rather than in the
	// JSON branch below: Anthropic's message_stop and mid-stream error, and —
	// because the Responses API terminates its stream with the envelope events
	// themselves and no documented [DONE] sentinel (the terminal markers are
	// response.completed / response.incomplete / response.failed, per the
	// official streaming reference; the SDKs only break on [DONE] if one
	// happens to arrive) — the Responses terminal events. Without them every
	// successful Responses relay would read as truncated.
	if rest, ok := bytes.CutPrefix(line, []byte("event:")); ok {
		if kind, known := terminalFromTypeName(string(bytes.TrimSpace(rest))); known {
			s.noteTerminal(kind)
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
		s.noteTerminal(terminalCompleted)
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
		// The frame mentioned usage but did not decode: whatever usage it
		// carried is lost. Count it — silently dropped usage must at least
		// leave an aggregate signal. (A frame that merely says "usage" inside
		// content can inflate this; as a degradation signal that is the right
		// bias — an unparseable frame deserves attention either way.)
		incUsageObserverDegrade(degradeMalformedUsage)
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
// Every field merges last-NON-NIL-wins: a frame that did not report a figure
// leaves whatever an earlier frame carried intact, and a later frame that DID
// report — including an explicit zero — is authoritative over it.
//
// The last-wins policy on totals is verified per dialect against official
// behavior — Anthropic's message_delta usage is documented cumulative,
// OpenAI Chat emits one final include_usage chunk carrying "token usage
// statistics for the entire request", Responses' response.completed carries the
// full usage, DeepSeek's last pre-[DONE] chunk carries the entire request, and
// Gemini reports usageMetadata only on the final chunk. Cumulative reporting is
// what makes last-wins-with-zeros safe: a zero on a usage-bearing frame is the
// upstream's real running total, and the final frame always carries the
// authoritative answer. A future dialect whose frames carry PER-CHUNK DELTAS
// rather than cumulative totals must accumulate inside its own normalize branch
// (emitting running totals) — this merge must not silently inherit the wrong
// semantics.
func mergeUsage(dst *usageCounts, env usageEnvelope) {
	c := env.normalize()
	if c.Input != nil {
		dst.Input = c.Input
	}
	if c.Output != nil {
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
		// Error carries the upstream's own error identity from a mid-stream
		// failure frame — Anthropic's {type:"error",error:{type:…}}, or OpenAI
		// chat's top-level {"error":{code,type}}. Only the short type/code
		// strings are read, never the message.
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
		// Response re-reads the Responses envelope far enough to catch
		// response.failed's error identity, which nests under response.error.
		Response struct {
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		} `json:"response"`
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
		s.noteTerminal(terminalCompleted)
	case "response.completed":
		s.noteTerminal(terminalCompleted)
	case "error", "response.failed":
		// A terminal failure frame settles the turn: the client is being told
		// the upstream's own error, which is a complete (if unhappy) outcome,
		// not a silent truncation — but it is also NOT a success, and it does
		// implicate the provider (mid-stream generation failures are
		// provider-side; request-level problems fail before the stream starts
		// with a 4xx). See forwardOneConnection for how the outcome is recorded.
		s.noteTerminal(terminalFailed)
		// Anthropic/OpenAI chat put the error identity top-level; Responses'
		// response.failed nests it under response.error.
		s.noteStreamErrCode(frame.Error.Type, frame.Error.Code)
		s.noteStreamErrCode(frame.Response.Error.Type, frame.Response.Error.Code)
	case "response.incomplete":
		// A limit ended the turn (incomplete_details.reason, e.g.
		// max_output_tokens). The provider enforced its own configured cap —
		// that is healthy behavior, never an outage.
		s.noteTerminal(terminalIncomplete)
	case "message_delta":
		// stop_reason on a message_delta is Anthropic's real end-of-turn signal.
		// max_tokens is the limit-triggered flavor: incomplete, not failed.
		if strings.TrimSpace(frame.Delta.StopReason) != "" {
			if limitStopReasons[strings.TrimSpace(frame.Delta.StopReason)] {
				s.noteTerminal(terminalIncomplete)
			} else {
				s.noteTerminal(terminalCompleted)
			}
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
			// length/content_filter mirror Responses' incomplete: the turn hit
			// a configured limit, which is not a provider failure.
			if limitStopReasons[strings.TrimSpace(c.FinishReason)] {
				s.noteTerminal(terminalIncomplete)
			} else {
				s.noteTerminal(terminalCompleted)
			}
		}
		if c.Delta.Content != "" || len(c.Delta.ToolCalls) > 0 {
			s.sawContent = true
		}
	}
}

// noteStreamErrCode records the upstream's error identity from a failed
// terminal frame. It prefers the error "type" (Anthropic's overloaded_error,
// OpenAI's server_error) over "code", keeps it short, and keeps only the first
// one seen.
func (s *usageScanner) noteStreamErrCode(errType, errCode string) {
	if s.streamErrCode != "" {
		return
	}
	code := strings.TrimSpace(errType)
	if code == "" {
		code = strings.TrimSpace(errCode)
	}
	if code == "" {
		return
	}
	if len(code) > 64 {
		code = code[:64]
	}
	s.streamErrCode = code
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
// Only the ABSENCE of a terminal frame counts. A failed or incomplete terminal
// still settled the turn — the client was told the outcome — so neither is a
// truncation; they are distinguished through Terminal() instead, because
// terminal is not the same thing as success.
func (s *usageScanner) Truncated() bool {
	s.flushPartial()
	return s.terminal == terminalNone
}

// Terminal reports the generation outcome the stream's terminal frame reported,
// after flushing any trailing partial line. terminalNone means no terminal was
// ever seen (see Truncated).
func (s *usageScanner) Terminal() streamTerminalKind {
	s.flushPartial()
	return s.terminal
}

// ErrCode reports the upstream's own error type/code from a failed terminal
// frame, or "" when the outcome was not a failure or none was legible.
func (s *usageScanner) ErrCode() string {
	return s.streamErrCode
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
