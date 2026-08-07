package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// newRespWithHeaders builds a bare response carrying only the given headers,
// which is all forwardedUsageTokens inspects.
func newRespWithHeaders(hdrs map[string]string) *http.Response {
	resp := &http.Response{Header: make(http.Header)}
	for k, v := range hdrs {
		resp.Header.Set(k, v)
	}
	return resp
}

func TestUsageFromJSONBodyAnthropic(t *testing.T) {
	body := []byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":120,"output_tokens":45}}`)
	got := usageFromJSONBody(body)
	if got.Input != 120 || got.Output != 45 {
		t.Fatalf("got %+v, want 120/45", got)
	}
}

func TestUsageFromJSONBodyAnthropicCountsCacheTokens(t *testing.T) {
	// Cached input is still input the upstream billed for; excluding it would
	// under-report a cached request against an uncached one.
	body := []byte(`{"usage":{"input_tokens":10,"cache_creation_input_tokens":100,
		"cache_read_input_tokens":900,"output_tokens":5}}`)
	got := usageFromJSONBody(body)
	if got.Input != 1010 || got.Output != 5 {
		t.Fatalf("got %+v, want 1010/5", got)
	}
}

func TestUsageFromJSONBodyOpenAI(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hi"}}],
		"usage":{"prompt_tokens":300,"completion_tokens":80,"total_tokens":380}}`)
	got := usageFromJSONBody(body)
	if got.Input != 300 || got.Output != 80 {
		t.Fatalf("got %+v, want 300/80", got)
	}
}

func TestUsageFromJSONBodyTolerantOfJunk(t *testing.T) {
	for _, body := range []string{"", "not json", "{}", `{"usage":null}`, `{"usage":{}}`, "[1,2,3]"} {
		got := usageFromJSONBody([]byte(body))
		if got.known() {
			t.Fatalf("body %q yielded usage %+v, want none", body, got)
		}
	}
}

// feed writes s to the scanner in chunks of size n, exercising the partial-line
// buffering that a real network read pattern produces.
func feed(t *testing.T, s *usageScanner, data string, chunk int) {
	t.Helper()
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		if _, err := s.Write([]byte(data[i:end])); err != nil {
			t.Fatalf("scanner write: %v", err)
		}
	}
}

const anthropicStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":25,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":99}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestUsageScannerAnthropicStream(t *testing.T) {
	s := &usageScanner{}
	feed(t, s, anthropicStream, 4096)
	got := s.Counts()
	// input from message_start, output from the final message_delta
	if got.Input != 25 || got.Output != 99 {
		t.Fatalf("got %+v, want 25/99", got)
	}
}

func TestUsageScannerHandlesChunkBoundaries(t *testing.T) {
	// The usage frame must be found no matter where TCP splits the bytes.
	for _, chunk := range []int{1, 3, 7, 13, 64, 1024} {
		s := &usageScanner{}
		feed(t, s, anthropicStream, chunk)
		got := s.Counts()
		if got.Input != 25 || got.Output != 99 {
			t.Fatalf("chunk=%d: got %+v, want 25/99", chunk, got)
		}
	}
}

func TestUsageScannerOpenAIStream(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":150}}` + "\n\n" +
		"data: [DONE]\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 8)
	got := s.Counts()
	if got.Input != 500 || got.Output != 150 {
		t.Fatalf("got %+v, want 500/150", got)
	}
}

func TestUsageScannerCRLFLineEndings(t *testing.T) {
	stream := "event: message_delta\r\n" +
		`data: {"usage":{"input_tokens":7,"output_tokens":8}}` + "\r\n\r\n"
	s := &usageScanner{}
	feed(t, s, stream, 5)
	got := s.Counts()
	if got.Input != 7 || got.Output != 8 {
		t.Fatalf("got %+v, want 7/8", got)
	}
}

func TestUsageScannerNoTrailingNewline(t *testing.T) {
	// A stream that ends without a final newline must still be inspected, which
	// is what the flush inside Counts() is for.
	s := &usageScanner{}
	feed(t, s, `data: {"usage":{"input_tokens":3,"output_tokens":4}}`, 512)
	got := s.Counts()
	if got.Input != 3 || got.Output != 4 {
		t.Fatalf("got %+v, want 3/4", got)
	}
}

func TestUsageScannerIgnoresNonDataLines(t *testing.T) {
	stream := ": ping\n" +
		"event: usage\n" + // an event NAME containing "usage" is not a payload
		`data: {"choices":[{"delta":{"content":"usage"}}]}` + "\n\n"
	s := &usageScanner{}
	feed(t, s, stream, 16)
	if s.Counts().known() {
		t.Fatalf("got %+v, want no usage", s.Counts())
	}
}

func TestUsageScannerLastFrameWins(t *testing.T) {
	// Cumulative reporting: a later output count replaces an earlier one.
	stream := `data: {"usage":{"output_tokens":10}}` + "\n" +
		`data: {"usage":{"output_tokens":250}}` + "\n"
	s := &usageScanner{}
	feed(t, s, stream, 32)
	if got := s.Counts(); got.Output != 250 {
		t.Fatalf("got %+v, want output 250", got)
	}
}

func TestUsageScannerBoundedMemory(t *testing.T) {
	// A pathological upstream that never emits a newline must not grow the
	// scanner without bound.
	s := &usageScanner{}
	huge := strings.Repeat("x", maxUsageScanBuffer*3)
	feed(t, s, "data: "+huge, 8192)
	if len(s.partial) > maxUsageScanBuffer {
		t.Fatalf("partial buffer = %d bytes, want <= %d", len(s.partial), maxUsageScanBuffer)
	}
}

func TestUsageScannerStillFindsUsageAfterOverflow(t *testing.T) {
	// After discarding an oversized content line, a subsequent usage frame must
	// still be picked up.
	s := &usageScanner{}
	feed(t, s, "data: "+strings.Repeat("x", maxUsageScanBuffer*2)+"\n", 8192)
	feed(t, s, `data: {"usage":{"input_tokens":11,"output_tokens":22}}`+"\n", 64)
	got := s.Counts()
	if got.Input != 11 || got.Output != 22 {
		t.Fatalf("got %+v, want 11/22", got)
	}
}

func TestUsageScannerWriteReportsFullLength(t *testing.T) {
	// The scanner sits on the relay path as an io.Writer; a short write would be
	// interpreted as an error by generic copy helpers.
	s := &usageScanner{}
	p := []byte("data: {}\n")
	n, err := s.Write(p)
	if err != nil || n != len(p) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(p))
	}
}

func TestForwardedUsageTokensPrefersParsedUsage(t *testing.T) {
	resp := newRespWithHeaders(map[string]string{
		"X-Usage-Input-Tokens":  "1",
		"X-Usage-Output-Tokens": "2",
	})
	in, out := forwardedUsageTokens(resp, usageCounts{Input: 900, Output: 300})
	if in != 900 || out != 300 {
		t.Fatalf("got %d/%d, want 900/300 (parsed usage must win)", in, out)
	}
}

func TestForwardedUsageTokensFallsBackToHeaders(t *testing.T) {
	resp := newRespWithHeaders(map[string]string{
		"X-Usage-Input-Tokens":  "40",
		"X-Usage-Output-Tokens": "60",
	})
	in, out := forwardedUsageTokens(resp, usageCounts{})
	if in != 40 || out != 60 {
		t.Fatalf("got %d/%d, want 40/60", in, out)
	}
}

func TestForwardedUsageTokensChargesOneUnitWhenUnknown(t *testing.T) {
	resp := newRespWithHeaders(nil)
	in, out := forwardedUsageTokens(resp, usageCounts{})
	if in != 0 || out != 1 {
		t.Fatalf("got %d/%d, want 0/1", in, out)
	}
}

func TestForwardEndpointKind(t *testing.T) {
	if got := forwardEndpointKind(true); got != "claude" {
		t.Fatalf("got %q, want claude", got)
	}
	if got := forwardEndpointKind(false); got != "openai" {
		t.Fatalf("got %q, want openai", got)
	}
}
