package proxy

import (
	"encoding/json"
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
	if c.Input != 120 || c.Output != 45 {
		t.Fatalf("totals = %d/%d, want 120/45", c.Input, c.Output)
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
	if c.Input != 100050 {
		t.Fatalf("input = %d, want 100050 (input + cache_read)", c.Input)
	}
	if c.Output != 500 {
		t.Fatalf("output = %d, want 500", c.Output)
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
	if c.Input != 4096 {
		t.Fatalf("input = %d, want 4096 (2048+1800+248)", c.Input)
	}
	if *c.CacheReadInputTokens != 1800 || *c.CacheCreationInputTokens != 248 {
		t.Fatalf("cache = %v/%v, want 1800/248", *c.CacheReadInputTokens, *c.CacheCreationInputTokens)
	}
}

func TestNormalizeAnthropicExplicitZeroCache(t *testing.T) {
	// Streaming message_start often reports explicit zeros — that is a reported
	// zero, not a missing field.
	c := normalizeBody(t, `{"input_tokens":2679,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}`)
	if c.Input != 2679 {
		t.Fatalf("input = %d, want 2679", c.Input)
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 0 {
		t.Fatalf("cache read = %v, want reported zero", c.CacheReadInputTokens)
	}
	if !c.cacheKnown() {
		t.Fatal("explicit zeros must read as known cache telemetry")
	}
	// The canonical ratio is derived, never stored: 0 read / 2679 total input.
	if r := float64(*c.CacheReadInputTokens) / float64(c.Input); r != 0 {
		t.Fatalf("derived ratio = %v, want 0", r)
	}
}

func TestNormalizeOpenAIChatNoCache(t *testing.T) {
	c := normalizeBody(t, `{"prompt_tokens":300,"completion_tokens":80,"total_tokens":380}`)
	if c.Input != 300 || c.Output != 80 {
		t.Fatalf("totals = %d/%d, want 300/80", c.Input, c.Output)
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
	if c.Input != 1000 {
		t.Fatalf("input = %d, want 1000 (cached is inside prompt, not added)", c.Input)
	}
	if c.Output != 20 {
		t.Fatalf("output = %d, want 20", c.Output)
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
	if c.Input != 5000 {
		t.Fatalf("input = %d, want 5000", c.Input)
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
	if c.Input != 2000 {
		t.Fatalf("input = %d, want 2000", c.Input)
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
	if c.Input != 2000 {
		t.Fatalf("input = %d, want 1600+400", c.Input)
	}
}

func TestNormalizeGeminiUsageMetadata(t *testing.T) {
	c := normalizeBody(t, `{"usageMetadata":{"promptTokenCount":3000,"candidatesTokenCount":200,
		"cachedContentTokenCount":2600,"thoughtsTokenCount":80,"totalTokenCount":3200}}`)
	if c.Input != 3000 {
		t.Fatalf("input = %d, want 3000 (cached share already inside prompt)", c.Input)
	}
	if c.Output != 200 {
		t.Fatalf("output = %d, want 200", c.Output)
	}
	if *c.CacheReadInputTokens != 2600 {
		t.Fatalf("cache read = %v, want 2600", *c.CacheReadInputTokens)
	}
	if *c.ReasoningOutputTokens != 80 {
		t.Fatalf("thoughts = %v, want 80", *c.ReasoningOutputTokens)
	}
	if c.Protocol != protocolGemini {
		t.Fatalf("protocol = %q, want gemini", c.Protocol)
	}
}

func TestUsageFromJSONBodyGemini(t *testing.T) {
	// The non-stream path: Gemini nests usage under usageMetadata, not usage.
	body := `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10}}`
	c := usageFromJSONBody([]byte(body))
	if c.Input != 100 || c.Output != 10 {
		t.Fatalf("totals = %d/%d, want 100/10", c.Input, c.Output)
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
	if c.Input != 50 || c.Output != 5 {
		t.Fatalf("totals = %d/%d, want 50/5", c.Input, c.Output)
	}
	if c.CacheReadInputTokens != nil {
		t.Fatalf("unknown nested shape must not fabricate cache telemetry: %v", *c.CacheReadInputTokens)
	}
}

func TestUsageProtocolFallbackByPath(t *testing.T) {
	bare := usageCounts{Input: 10, Output: 2} // no shape-detected dialect
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
	deep := usageCounts{Input: 10, Output: 2, Protocol: protocolOpenAI}
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
		if c.Input != 2679+2400 {
			t.Fatalf("chunk=%d: input = %d, want 5079 (input + cache_read)", chunk, c.Input)
		}
		if c.Output != 89 {
			t.Fatalf("chunk=%d: output = %d, want 89 (final cumulative)", chunk, c.Output)
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
	if c.Input != 10682 {
		t.Fatalf("input = %d, want 10682 (final delta wins)", c.Input)
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
	if c.Input != 1000 {
		t.Fatalf("input = %d, want 1000 (never 1900)", c.Input)
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
	if c.Input != 2000 || c.Output != 300 {
		t.Fatalf("totals = %d/%d, want 2000/300", c.Input, c.Output)
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
	stream := `data: {"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":3000,"candidatesTokenCount":120,"cachedContentTokenCount":2600,"thoughtsTokenCount":60}}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 21)
	c := s.Counts()
	if c.Input != 3000 || c.Output != 120 {
		t.Fatalf("totals = %d/%d, want 3000/120", c.Input, c.Output)
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 2600 {
		t.Fatalf("cache read = %v, want 2600", c.CacheReadInputTokens)
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
	if c.Output != 77 || c.Input != 900 {
		t.Fatalf("totals = %d/%d, want 900/77", c.Input, c.Output)
	}
	if c.CacheReadInputTokens == nil || *c.CacheReadInputTokens != 400 {
		t.Fatalf("cache read = %v, want 400 preserved", c.CacheReadInputTokens)
	}
}
