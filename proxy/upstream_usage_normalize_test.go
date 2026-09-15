package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// normalizeBody runs one raw usage JSON through the envelope decode + normalize
// pipeline. Fixtures are usage objects, so the decode goes straight into the
// envelope; a malformed body yields the all-unknown zero, mirroring
// usageFromJSONBody's tolerance.
func normalizeBody(t *testing.T, body string) usageCounts {
	t.Helper()
	var env usageEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return usageCounts{}
	}
	return env.normalize()
}

// The fixtures below follow the official response shapes:
//   - Anthropic Messages: platform.claude.com/docs/en/api/messages and the
//     prompt-caching guide ("total_input_tokens = cache_read + cache_creation + input_tokens").
//   - OpenAI Chat Completions + Responses: the openai-openapi spec
//     (prompt_tokens_details.cached_tokens is a subset of prompt_tokens).
//   - DeepSeek: api-docs.deepseek.com/guides/kv_cache (prompt_tokens = hit + miss).
//   - Gemini: ai.google.dev/api/generate-content (promptTokenCount includes
//     cachedContentTokenCount).

func TestNormalizeAnthropicNoCache(t *testing.T) {
	c := normalizeBody(t, `{"input_tokens":120,"output_tokens":45}`)
	if tot(c.Input) != 120 || tot(c.Output) != 45 {
		t.Fatalf("totals = %d/%d, want 120/45", tot(c.Input), tot(c.Output))
	}
	if c.CacheReadInputTokens != nil || c.CacheCreationInputTokens != nil {
		t.Fatalf("cache fields = %v/%v, want nil/nil (no cache telemetry)", c.CacheReadInputTokens, c.CacheCreationInputTokens)
	}
	if c.Protocol != "" {
		t.Fatalf("protocol = %q, want shape-agnostic (bare totals)", c.Protocol)
	}
}

func TestNormalizeAnthropicCacheRead(t *testing.T) {
	// Doc example: 100k cached, 50 uncached after the breakpoint.
	c := normalizeBody(t, `{"input_tokens":50,"cache_read_input_tokens":100000,"output_tokens":500}`)
	if tot(c.Input) != 100050 {
		t.Fatalf("input = %d, want 100050 (input + cache_read)", tot(c.Input))
	}
	if tot(c.Output) != 500 {
		t.Fatalf("output = %d, want 500", tot(c.Output))
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 100000 {
		t.Fatalf("cache read = %v, want 100000", c.CacheReadInputTokens)
	}
	if c.CacheCreationInputTokens != nil {
		t.Fatalf("cache creation = %v, want nil (absent)", c.CacheCreationInputTokens)
	}
	if c.Protocol != protocolAnthropic {
		t.Fatalf("protocol = %q, want anthropic", c.Protocol)
	}
}

func TestNormalizeAnthropicCacheReadAndCreation(t *testing.T) {
	// The caching doc's 1h-TTL example shape.
	c := normalizeBody(t, `{"input_tokens":2048,"cache_read_input_tokens":1800,"cache_creation_input_tokens":248,"output_tokens":503}`)
	if tot(c.Input) != 4096 {
		t.Fatalf("input = %d, want 4096 (2048+1800+248)", tot(c.Input))
	}
	if *c.CacheReadInputTokens != 1800 || *c.CacheCreationInputTokens != 248 {
		t.Fatalf("cache = %v/%v, want 1800/248", *c.CacheReadInputTokens, *c.CacheCreationInputTokens)
	}
}

func TestNormalizeAnthropicExplicitZeroCache(t *testing.T) {
	// Streaming message_start often reports explicit zeros — that is a reported
	// zero, not a missing field.
	c := normalizeBody(t, `{"input_tokens":2679,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}`)
	if tot(c.Input) != 2679 {
		t.Fatalf("input = %d, want 2679", tot(c.Input))
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 0 {
		t.Fatalf("cache read = %v, want reported zero", c.CacheReadInputTokens)
	}
	if !c.cacheKnown() {
		t.Fatal("explicit zeros must read as known cache telemetry")
	}
	// The canonical ratio is derived, never stored: 0 read / 2679 total input.
	if r := float64(*c.CacheReadInputTokens) / float64(tot(c.Input)); r != 0 {
		t.Fatalf("derived ratio = %v, want 0", r)
	}
}

func TestNormalizeOpenAIChatNoCache(t *testing.T) {
	c := normalizeBody(t, `{"prompt_tokens":300,"completion_tokens":80,"total_tokens":380}`)
	if tot(c.Input) != 300 || tot(c.Output) != 80 {
		t.Fatalf("totals = %d/%d, want 300/80", tot(c.Input), tot(c.Output))
	}
	if c.CacheReadInputTokens != nil {
		t.Fatalf("cache read = %v, want nil", c.CacheReadInputTokens)
	}
}

func TestNormalizeOpenAIChatCachedTokens(t *testing.T) {
	// cached_tokens is a SUBSET of prompt_tokens: the totals must not gain it.
	c := normalizeBody(t, `{"prompt_tokens":1000,"completion_tokens":20,
		"prompt_tokens_details":{"cached_tokens":850},
		"completion_tokens_details":{"reasoning_tokens":12}}`)
	if tot(c.Input) != 1000 {
		t.Fatalf("input = %d, want 1000 (cached is inside prompt, not added)", tot(c.Input))
	}
	if tot(c.Output) != 20 {
		t.Fatalf("output = %d, want 20", tot(c.Output))
	}
	if *c.CacheReadInputTokens != 850 {
		t.Fatalf("cache read = %v, want 850", *c.CacheReadInputTokens)
	}
	if *c.ReasoningOutputTokens != 12 {
		t.Fatalf("reasoning = %v, want 12", *c.ReasoningOutputTokens)
	}
	if c.Protocol != protocolOpenAI {
		t.Fatalf("protocol = %q, want openai", c.Protocol)
	}
}

func TestNormalizeOpenAIResponsesUsage(t *testing.T) {
	// ResponseUsage from the OpenAPI spec: input_tokens_details{cached_tokens,
	// cache_write_tokens} and output_tokens_details{reasoning_tokens}.
	c := normalizeBody(t, `{"input_tokens":5000,"output_tokens":800,
		"input_tokens_details":{"cached_tokens":4500,"cache_write_tokens":200},
		"output_tokens_details":{"reasoning_tokens":300},"total_tokens":5800}`)
	if tot(c.Input) != 5000 {
		t.Fatalf("input = %d, want 5000", tot(c.Input))
	}
	if *c.CacheReadInputTokens != 4500 || *c.CacheCreationInputTokens != 200 {
		t.Fatalf("cache = %v/%v, want 4500/200", *c.CacheReadInputTokens, *c.CacheCreationInputTokens)
	}
	if *c.ReasoningOutputTokens != 300 {
		t.Fatalf("reasoning = %v, want 300", *c.ReasoningOutputTokens)
	}
	if c.Protocol != protocolResponses {
		t.Fatalf("protocol = %q, want responses", c.Protocol)
	}
}

func TestNormalizeDeepSeekHitMiss(t *testing.T) {
	c := normalizeBody(t, `{"prompt_tokens":2000,"completion_tokens":300,
		"prompt_cache_hit_tokens":1600,"prompt_cache_miss_tokens":400}`)
	if tot(c.Input) != 2000 {
		t.Fatalf("input = %d, want 2000", tot(c.Input))
	}
	if *c.CacheReadInputTokens != 1600 {
		t.Fatalf("cache read = %v, want 1600 (hit tokens)", *c.CacheReadInputTokens)
	}
	if c.CacheCreationInputTokens != nil {
		t.Fatalf("cache creation = %v, want nil (DeepSeek reports no writes)", c.CacheCreationInputTokens)
	}
}

func TestNormalizeDeepSeekMissingPromptTokens(t *testing.T) {
	// hit + miss stand in for the total when prompt_tokens is absent.
	c := normalizeBody(t, `{"prompt_cache_hit_tokens":1600,"prompt_cache_miss_tokens":400}`)
	if tot(c.Input) != 2000 {
		t.Fatalf("input = %d, want 1600+400", tot(c.Input))
	}
}

func TestNormalizeGeminiUsageMetadata(t *testing.T) {
	c := normalizeBody(t, `{"usageMetadata":{"promptTokenCount":3000,"candidatesTokenCount":200,
		"cachedContentTokenCount":2600,"thoughtsTokenCount":80,"totalTokenCount":3280}}`)
	if tot(c.Input) != 3000 {
		t.Fatalf("input = %d, want 3000 (cached share already inside prompt)", tot(c.Input))
	}
	// Official generateContent reference: totalTokenCount = "prompt + thoughts
	// + response candidates" — thoughts are ADDITIVE to candidates, so the
	// canonical output side is 200 + 80.
	if tot(c.Output) != 280 {
		t.Fatalf("output = %d, want 280 (candidates 200 + thoughts 80)", tot(c.Output))
	}
	if *c.CacheReadInputTokens != 2600 {
		t.Fatalf("cache read = %v, want 2600", *c.CacheReadInputTokens)
	}
	if *c.ReasoningOutputTokens != 80 {
		t.Fatalf("thoughts = %v, want 80", *c.ReasoningOutputTokens)
	}
	// Canonical invariant: reasoning is a subset of the output side.
	if *c.ReasoningOutputTokens > tot(c.Output) {
		t.Fatalf("reasoning %d exceeds output %d", *c.ReasoningOutputTokens, tot(c.Output))
	}
	if c.Protocol != protocolGemini {
		t.Fatalf("protocol = %q, want gemini", c.Protocol)
	}
}

func TestNormalizeGeminiThoughtsExceedCandidates(t *testing.T) {
	// The audit's invariant case: a short answer that thought a long time.
	// candidates=1000, thoughts=4000 must give output 5000 with reasoning 4000
	// INSIDE it — the old mapping (output = candidates alone) recorded
	// reasoning 4000 > output 1000 and understated the output side.
	c := normalizeBody(t, `{"promptTokenCount":10000,"candidatesTokenCount":1000,"thoughtsTokenCount":4000}`)
	if tot(c.Input) != 10000 || tot(c.Output) != 5000 || *c.ReasoningOutputTokens != 4000 {
		t.Fatalf("totals = %d/%d reasoning %v, want 10000/5000/4000", tot(c.Input), tot(c.Output), c.ReasoningOutputTokens)
	}
	if *c.ReasoningOutputTokens > tot(c.Output) {
		t.Fatal("canonical invariant broken: reasoning must be a subset of output")
	}
}

func TestNormalizeGeminiNoThoughts(t *testing.T) {
	// A non-thinking model reports no thoughtsTokenCount: output is exactly
	// the candidate count, reasoning stays unknown.
	c := normalizeBody(t, `{"promptTokenCount":100,"candidatesTokenCount":10}`)
	if tot(c.Output) != 10 {
		t.Fatalf("output = %d, want 10", tot(c.Output))
	}
	if c.ReasoningOutputTokens != nil {
		t.Fatalf("reasoning = %v, want nil (no thoughts reported)", c.ReasoningOutputTokens)
	}
}

func TestNormalizeGeminiExplicitZeroThoughts(t *testing.T) {
	// An explicit thinking=0 is a reported zero: reasoning publishes as known
	// zero while the output side stays the candidate count.
	c := normalizeBody(t, `{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":0}`)
	if tot(c.Output) != 10 {
		t.Fatalf("output = %d, want 10 (thoughts zero adds nothing)", tot(c.Output))
	}
	if c.ReasoningOutputTokens == nil || *c.ReasoningOutputTokens != 0 {
		t.Fatalf("reasoning = %v, want known zero", c.ReasoningOutputTokens)
	}
}

func TestUsageFromJSONBodyGemini(t *testing.T) {
	// The non-stream path: Gemini nests usage under usageMetadata, not usage.
	body := `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10}}`
	c := usageFromJSONBody([]byte(body))
	if tot(c.Input) != 100 || tot(c.Output) != 10 {
		t.Fatalf("totals = %d/%d, want 100/10", tot(c.Input), tot(c.Output))
	}
}

func TestNormalizeMalformedAndEmptyUsage(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"usage":{}}`,
		`{"usage":null}`,
		`{"usage":"string"}`,
		`{"prompt_tokens":"x"}`,
	} {
		c := normalizeBody(t, body)
		if c.known() || c.cacheKnown() || c.ReasoningOutputTokens != nil {
			t.Fatalf("body %q yielded %+v, want all-unknown", body, c)
		}
	}
}

func TestNormalizeUnknownExtraFieldsIgnored(t *testing.T) {
	// A gateway adding its own fields must not confuse the dialect detection.
	c := normalizeBody(t, `{"prompt_tokens":50,"completion_tokens":5,"router_meta":{"cache":{"hit":999}}}`)
	if tot(c.Input) != 50 || tot(c.Output) != 5 {
		t.Fatalf("totals = %d/%d, want 50/5", tot(c.Input), tot(c.Output))
	}
	if c.CacheReadInputTokens != nil {
		t.Fatalf("unknown nested shape must not fabricate cache telemetry: %v", *c.CacheReadInputTokens)
	}
}

func TestUsageProtocolFallbackByPath(t *testing.T) {
	bare := usageCounts{Input: ptr(10), Output: ptr(2)} // no shape-detected dialect
	if got := usageProtocol(bare, "/messages"); got != protocolAnthropic {
		t.Fatalf("messages path -> %q, want anthropic", got)
	}
	if got := usageProtocol(bare, "/responses"); got != protocolResponses {
		t.Fatalf("responses path -> %q, want responses", got)
	}
	if got := usageProtocol(bare, "/chat/completions"); got != protocolOpenAI {
		t.Fatalf("chat path -> %q, want openai", got)
	}
	// A shape-detected dialect wins over the path: an OpenAI-shaped usage on a
	// Claude route still says it spoke OpenAI.
	deep := usageCounts{Input: ptr(10), Output: ptr(2), Protocol: protocolOpenAI}
	if got := usageProtocol(deep, "/messages"); got != protocolOpenAI {
		t.Fatalf("shape detection -> %q, want openai", got)
	}
}

// Streaming merges: usage frames split across transport reads, cumulative
// semantics (never summed), and per-field last-non-nil-wins.

func TestUsageScannerAnthropicCacheAcrossChunkBoundaries(t *testing.T) {
	// Real Anthropic caching stream: message_start reports the cache fields
	// (per the prompt-caching doc's streaming note), message_delta reports the
	// final output total. TCP may split anywhere.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2679,"cache_creation_input_tokens":0,"cache_read_input_tokens":2400,"output_tokens":3}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":89}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	for _, chunk := range []int{1, 7, 64, 4096} {
		s := &usageScanner{}
		feed(t, s, stream, chunk)
		c := s.Counts()
		if tot(c.Input) != 2679+2400 {
			t.Fatalf("chunk=%d: input = %d, want 5079 (input + cache_read)", chunk, tot(c.Input))
		}
		if tot(c.Output) != 89 {
			t.Fatalf("chunk=%d: output = %d, want 89 (final cumulative)", chunk, tot(c.Output))
		}
		if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 2400 {
			t.Fatalf("chunk=%d: cache read = %v, want 2400", chunk, c.CacheReadInputTokens)
		}
	}
}

func TestUsageScannerAnthropicFinalDeltaCarriesFullUsage(t *testing.T) {
	// The streaming-with-thinking doc shows message_delta usage carrying the
	// FULL final usage. Its explicit cache zero is authoritative over the
	// earlier nonzero from message_start.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":900,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10682,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":510}}` + "\n\n" +
		"event: message_stop\n"
	s := &usageScanner{}
	feed(t, s, stream, 128)
	c := s.Counts()
	if tot(c.Input) != 10682 {
		t.Fatalf("input = %d, want 10682 (final delta wins)", tot(c.Input))
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 0 {
		t.Fatalf("cache read = %v, want explicit final zero", c.CacheReadInputTokens)
	}
}

func TestUsageScannerOpenAIChatCachedAndReasoning(t *testing.T) {
	// SSE data frames are single-line (per the SSE spec), usage object included.
	stream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":900},"completion_tokens_details":{"reasoning_tokens":30}}}` + "\n\n" +
		"data: [DONE]\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 13)
	c := s.Counts()
	if tot(c.Input) != 1000 {
		t.Fatalf("input = %d, want 1000 (never 1900)", tot(c.Input))
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 900 {
		t.Fatalf("cache read = %v, want 900", c.CacheReadInputTokens)
	}
	if c.ReasoningOutputTokens == nil || *c.ReasoningOutputTokens != 30 {
		t.Fatalf("reasoning = %v, want 30", c.ReasoningOutputTokens)
	}
	if c.Protocol != protocolOpenAI {
		t.Fatalf("protocol = %q, want openai", c.Protocol)
	}
}

func TestUsageScannerResponsesCompletedEvent(t *testing.T) {
	// OpenAI Responses' terminal response.completed event nests usage under
	// "response".
	stream := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":2000,"output_tokens":300,"input_tokens_details":{"cached_tokens":1500},"output_tokens_details":{"reasoning_tokens":250}}}}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 32)
	c := s.Counts()
	if tot(c.Input) != 2000 || tot(c.Output) != 300 {
		t.Fatalf("totals = %d/%d, want 2000/300", tot(c.Input), tot(c.Output))
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 1500 {
		t.Fatalf("cache read = %v, want 1500", c.CacheReadInputTokens)
	}
	if c.Protocol != protocolResponses {
		t.Fatalf("protocol = %q, want responses", c.Protocol)
	}
}

func TestUsageScannerGeminiChunk(t *testing.T) {
	// Gemini's SSE chunks carry usageMetadata with the camelCase fields.
	// thoughtsTokenCount is additive to candidatesTokenCount (official
	// totalTokenCount = prompt + thoughts + candidates), so output = 120+60.
	stream := `data: {"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":3000,"candidatesTokenCount":120,"cachedContentTokenCount":2600,"thoughtsTokenCount":60}}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 21)
	c := s.Counts()
	if tot(c.Input) != 3000 || tot(c.Output) != 180 {
		t.Fatalf("totals = %d/%d, want 3000/180", tot(c.Input), tot(c.Output))
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 2600 {
		t.Fatalf("cache read = %v, want 2600", c.CacheReadInputTokens)
	}
	if c.ReasoningOutputTokens == nil || *c.ReasoningOutputTokens != 60 {
		t.Fatalf("reasoning = %v, want 60 (subset of the 180 output)", c.ReasoningOutputTokens)
	}
	if c.Protocol != protocolGemini {
		t.Fatalf("protocol = %q, want gemini", c.Protocol)
	}
}

func TestUsageScannerCacheSurvivesFrameWithoutUsage(t *testing.T) {
	// A later frame reporting only output_tokens must not erase the cache
	// breakdown an earlier frame carried (per-field merge, not per-object).
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":500,"cache_read_input_tokens":400,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":77}}` + "\n\n" +
		"event: message_stop\n"
	s := &usageScanner{}
	feed(t, s, stream, 48)
	c := s.Counts()
	if tot(c.Output) != 77 || tot(c.Input) != 900 {
		t.Fatalf("totals = %d/%d, want 900/77", tot(c.Input), tot(c.Output))
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 400 {
		t.Fatalf("cache read = %v, want 400 preserved", c.CacheReadInputTokens)
	}
}

// A single SSE line larger than the scanner's 64KB buffer (giant tool-call
// arguments, a huge content delta) must NOT kill the observer: the oversized
// line is dropped, and a final usage frame after it is still captured. The
// client stream is untouched either way — this scanner is only ever a
// side-observer fed the same slices.
func TestUsageScannerGiantFrameThenUsage(t *testing.T) {
	giant := strings.Repeat("x", 100*1024)
	stream := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"` + giant + `"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1000,"output_tokens":77}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	// 16KB mirrors the relay's read buffer; 1500/8192 sample other splits, and
	// every chunk boundary lands at a different byte of the giant line.
	for _, chunk := range []int{1500, 8192, 16384} {
		s := &usageScanner{}
		feed(t, s, stream, chunk)
		c := s.Counts()
		if tot(c.Input) != 1000 || tot(c.Output) != 77 {
			t.Fatalf("chunk=%d: usage after a giant frame = %d/%d, want 1000/77", chunk, tot(c.Input), tot(c.Output))
		}
		if s.Truncated() {
			t.Fatalf("chunk=%d: terminal markers lost across the giant frame", chunk)
		}
		// Note: the dropped giant line's own content marker is lost with it
		// (its JSON can no longer decode). That only colors the truncation
		// message — never the verdict, and never the usage.
	}
}

// The giant line itself may carry a usage field inside its own (dropped) JSON
// when the whole frame exceeds the buffer. That frame's usage is lost — it is
// content, not accounting — but the NEXT frame's usage must still win, and the
// totals must never be invented from a partial decode.
func TestUsageScannerOversizedUsageFrameDroppedWithoutFabrication(t *testing.T) {
	giant := strings.Repeat("y", 100*1024)
	stream := `data: {"choices":[{"delta":{"content":"` + giant + `"}}],"usage":{"prompt_tokens":500,"completion_tokens":10}}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":999}}` + "\n\n" +
		"data: [DONE]\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 16384)
	c := s.Counts()
	// The small final frame is authoritative; nothing was fabricated from the
	// truncated tail of the giant one.
	if tot(c.Input) != 500 || tot(c.Output) != 999 {
		t.Fatalf("usage = %d/%d, want 500/999 (final small frame wins)", tot(c.Input), tot(c.Output))
	}
}

// --- Issue: canonical tri-state on totals -------------------------------------
// The parse layer must preserve wire presence exactly: absent stays nil, an
// explicit zero stays a pointer to zero. Placeholder suppression belongs to the
// stream merge, which sees whole frames — never to this parser.

func TestNormalizeTotalsPreserveExplicitZero(t *testing.T) {
	// OpenAI Chat with a reported zero on both sides.
	c := normalizeBody(t, `{"prompt_tokens":0,"completion_tokens":0}`)
	if c.Input == nil || *c.Input != 0 || c.Output == nil || *c.Output != 0 {
		t.Fatalf("totals = %v/%v, want ptr(0)/ptr(0)", c.Input, c.Output)
	}
	if !c.known() {
		t.Fatal("an explicit zero is a report: known() must be true")
	}
	// Anthropic/Responses spelling of the same.
	c = normalizeBody(t, `{"input_tokens":0,"output_tokens":0}`)
	if c.Input == nil || *c.Input != 0 || c.Output == nil || *c.Output != 0 {
		t.Fatalf("totals = %v/%v, want ptr(0)/ptr(0)", c.Input, c.Output)
	}
}

func TestNormalizeTotalsAbsentStayUnknown(t *testing.T) {
	// One side reported, the other absent: only the reported side is present.
	c := normalizeBody(t, `{"prompt_tokens":500}`)
	if c.Input == nil || *c.Input != 500 {
		t.Fatalf("input = %v, want ptr(500)", c.Input)
	}
	if c.Output != nil {
		t.Fatalf("output = %v, want nil (absent stays unknown)", c.Output)
	}
	if !c.known() {
		t.Fatal("a one-sided report is still a report")
	}
	// Nothing at all: everything unknown.
	c = normalizeBody(t, `{"total_tokens":0}`)
	if c.known() || c.Input != nil || c.Output != nil {
		t.Fatalf("%+v, want all-unknown", c)
	}
}

func TestNormalizeAnthropicZeroInputWithCache(t *testing.T) {
	// input_tokens explicitly zero plus cache fields: the sum keeps presence
	// and the zero is not lost.
	c := normalizeBody(t, `{"input_tokens":0,"cache_read_input_tokens":900,"output_tokens":5}`)
	if c.Input == nil || *c.Input != 900 {
		t.Fatalf("input = %v, want ptr(900)", c.Input)
	}
	if *c.CacheReadInputTokens != 900 {
		t.Fatalf("cache read = %v, want 900", c.CacheReadInputTokens)
	}
}

func TestUsageScannerMergeExplicitZeroIsAuthoritative(t *testing.T) {
	// The Anthropic streaming doc's final message_delta carries the FULL final
	// usage; an explicit zero there is authoritative over an earlier nonzero.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 64)
	c := s.Counts()
	if c.Output == nil || *c.Output != 0 {
		t.Fatalf("output = %v, want authoritative zero", c.Output)
	}
	if c.Input == nil || *c.Input != 100 {
		t.Fatalf("input = %v, want 100 preserved (later frame had none)", c.Input)
	}
}

func TestUsageScannerMergeFrameWithoutTotalsPreserves(t *testing.T) {
	// A usage-bearing frame that omits a side must not erase it.
	stream := `data: {"usage":{"input_tokens":42,"output_tokens":7}}` + "\n" +
		`data: {"usage":{"output_tokens":9}}` + "\n"
	s := &usageScanner{}
	feed(t, s, stream, 16)
	c := s.Counts()
	if tot(c.Input) != 42 || tot(c.Output) != 9 {
		t.Fatalf("totals = %d/%d, want 42/9", tot(c.Input), tot(c.Output))
	}
}

func TestUsageFromJSONBodyExplicitZeroTotals(t *testing.T) {
	// End to end through the body reader: zeros survive, absence does not
	// fabricate.
	c := usageFromJSONBody([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	if c.Input == nil || *c.Input != 0 || c.Output == nil || *c.Output != 0 {
		t.Fatalf("totals = %v/%v, want ptr(0)/ptr(0)", c.Input, c.Output)
	}
	c = usageFromJSONBody([]byte(`{"usage":{}}`))
	if c.Input != nil || c.Output != nil {
		t.Fatalf("empty usage object = %v/%v, want nil/nil", c.Input, c.Output)
	}
}

// --- Issue: oversized response.completed keeps its usage ----------------------
// OpenAI Responses' terminal event carries the ENTIRE generated output inside
// "response" beside the authoritative usage, so a long answer makes the frame
// far larger than the scan buffer. The oversized line is streamed through
// bigLineFilter, which must still capture the usage — dropping the frame (the
// old behavior) would drop the only usage the stream ever reports.

func responsesCompletedFrame(pad int) string {
	// Key order mirrors the real event: output (the bulk) first, usage after.
	return "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"` +
		strings.Repeat("x", pad) +
		`"}]}],"usage":{"input_tokens":2000,"output_tokens":300,"input_tokens_details":{"cached_tokens":1500,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":250}},"status":"completed"}}` + "\n\n"
}

func TestUsageScannerOversizedResponsesCompletedKeepsUsage(t *testing.T) {
	// 100KB of generated output: the frame dwarfs the 64KB scan buffer, and its
	// usage sits in the second half — exactly what a keep-the-tail policy loses.
	stream := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		responsesCompletedFrame(100*1024)
	for _, chunk := range []int{1500, 8192, 16384, 65536} {
		s := &usageScanner{}
		feed(t, s, stream, chunk)
		c := s.Counts()
		if tot(c.Input) != 2000 || tot(c.Output) != 300 {
			t.Fatalf("chunk=%d: usage = %d/%d, want 2000/300 (authoritative usage lost)", chunk, tot(c.Input), tot(c.Output))
		}
		if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 1500 {
			t.Fatalf("chunk=%d: cache read = %v, want 1500", chunk, c.CacheReadInputTokens)
		}
		if c.ReasoningOutputTokens == nil || *c.ReasoningOutputTokens != 250 {
			t.Fatalf("chunk=%d: reasoning = %v, want 250", chunk, c.ReasoningOutputTokens)
		}
		if c.Protocol != protocolResponses {
			t.Fatalf("chunk=%d: protocol = %q, want responses", chunk, c.Protocol)
		}
		// The event: response.completed line settles the turn even though the
		// oversized frame itself is never decoded as a whole.
		if s.Truncated() {
			t.Fatalf("chunk=%d: response.completed must be terminal without [DONE]", chunk)
		}
	}
}

func TestUsageScannerResponsesTerminalEventsNoDone(t *testing.T) {
	// The Responses API ends its stream with the envelope events — no
	// documented [DONE] sentinel. Every terminal variant must settle the turn.
	for _, ev := range []string{"response.completed", "response.incomplete", "response.failed"} {
		stream := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
			`data: {"type":"` + ev + `","response":{"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n\n"
		s := &usageScanner{}
		feed(t, s, stream, 64)
		if s.Truncated() {
			t.Fatalf("%s must settle the stream without [DONE]", ev)
		}
	}
}

func TestUsageScannerOversizedUsageObjectBeyondCapAbandoned(t *testing.T) {
	// A "usage" object larger than the capture cap is not accounting; the
	// capture is abandoned whole and nothing is fabricated from a partial body.
	// A later authoritative frame still wins.
	bulk := `{"type":"response.completed","response":{"output":[{"text":"` + strings.Repeat("y", 100*1024) + `"}],` +
		`"usage":{"input_tokens":777,"junk":"` + strings.Repeat("z", 32*1024) + `"}}}`
	stream := "data: " + bulk + "\n\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":500,"output_tokens":999}}}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 16384)
	c := s.Counts()
	if tot(c.Input) != 500 || tot(c.Output) != 999 {
		t.Fatalf("usage = %d/%d, want 500/999 (final frame wins, nothing fabricated)", tot(c.Input), tot(c.Output))
	}
}

func TestUsageScannerMalformedOversizedFrameHarmless(t *testing.T) {
	// A giant line that is not valid JSON must not break the observer: no
	// panic, no fabricated counts, later frames still parsed.
	stream := "data: " + strings.Repeat("{", 100*1024) + "\n\n" +
		`data: {"usage":{"input_tokens":11,"output_tokens":22}}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 4096)
	c := s.Counts()
	if tot(c.Input) != 11 || tot(c.Output) != 22 {
		t.Fatalf("usage = %d/%d, want 11/22", tot(c.Input), tot(c.Output))
	}
}

func TestUsageScannerOversizedChatChunkWithUsageCaptured(t *testing.T) {
	// The old policy dropped the oversized line entirely, losing its usage.
	// The filter now captures the usage object inside it, and a later small
	// frame still wins the merge.
	giant := `data: {"choices":[{"delta":{"content":"` + strings.Repeat("y", 100*1024) + `"}}],"usage":{"prompt_tokens":500,"completion_tokens":10}}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":999}}` + "\n\n" +
		"data: [DONE]\n\n"
	s := &usageScanner{}
	feed(t, s, giant, 16384)
	c := s.Counts()
	if tot(c.Input) != 500 || tot(c.Output) != 999 {
		t.Fatalf("usage = %d/%d, want 500/999 (final small frame wins)", tot(c.Input), tot(c.Output))
	}
}

func TestUsageScannerOversizedGeminiChunkWithUsageCaptured(t *testing.T) {
	// Gemini has no event: lines and no [DONE]; the giant final chunk's
	// usageMetadata must still be captured (thoughts additive).
	bulk := `data: {"candidates":[{"content":{"parts":[{"text":"` + strings.Repeat("g", 90*1024) + `"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":40,"thoughtsTokenCount":8}}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, bulk, 8192)
	c := s.Counts()
	if tot(c.Input) != 100 || tot(c.Output) != 48 {
		t.Fatalf("usage = %d/%d, want 100/48 (40 candidates + 8 thoughts)", tot(c.Input), tot(c.Output))
	}
	if c.ReasoningOutputTokens == nil || *c.ReasoningOutputTokens != 8 {
		t.Fatalf("reasoning = %v, want 8", c.ReasoningOutputTokens)
	}
}

func TestUsageScannerOversizedFrameUsageTextCannotTriggerCapture(t *testing.T) {
	// The literal bytes "usage" inside response TEXT (a JSON string, escaped
	// quotes included) must not start a capture — only real object keys count.
	tricky := `data: {"type":"response.completed","response":{"output":[{"text":"she said \"usage\": {\"input_tokens\": 1} aloud and then ` + strings.Repeat("q", 80*1024) + `"}]},"usage":{"input_tokens":2000,"output_tokens":300}}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, tricky, 4096)
	c := s.Counts()
	if tot(c.Input) != 2000 || tot(c.Output) != 300 {
		t.Fatalf("usage = %d/%d, want 2000/300 (text mention ignored, real usage captured)", tot(c.Input), tot(c.Output))
	}
}

func TestNormalizeGeminiThoughtsOnlyStillGemini(t *testing.T) {
	// A usageMetadata carrying nothing but the thought count must still be
	// recognized as the Gemini dialect rather than falling through unknown.
	c := normalizeBody(t, `{"thoughtsTokenCount":7}`)
	if tot(c.Output) != 7 || c.ReasoningOutputTokens == nil || *c.ReasoningOutputTokens != 7 {
		t.Fatalf("thoughts-only envelope = %+v, want output 7 reasoning 7", c)
	}
	if c.Protocol != protocolGemini {
		t.Fatalf("protocol = %q, want gemini", c.Protocol)
	}
	if c.Input != nil {
		t.Fatalf("input = %v, want nil (prompt not reported)", c.Input)
	}
}
