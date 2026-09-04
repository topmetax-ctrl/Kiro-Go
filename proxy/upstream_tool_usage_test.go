package proxy

import (
	"strings"
	"testing"
)

// TestUsageFromJSONBodyServerToolUse verifies the forwarding usage extractor
// reads usage.server_tool_use.web_search_requests from a non-stream body.
func TestUsageFromJSONBodyServerToolUse(t *testing.T) {
	body := `{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":20,"server_tool_use":{"web_search_requests":2}}}`
	u := usageFromJSONBody([]byte(body))
	if u.Input != 10 || u.Output != 20 {
		t.Fatalf("tokens = %d/%d, want 10/20", u.Input, u.Output)
	}
	if u.ServerTool.WebSearchRequests != 2 {
		t.Fatalf("web_search_requests = %d, want 2", u.ServerTool.WebSearchRequests)
	}
}

// TestUsageFromJSONBodyNoServerToolUse: absent field stays zero (partial
// observability: unknown must not be fabricated).
func TestUsageFromJSONBodyNoServerToolUse(t *testing.T) {
	body := `{"usage":{"input_tokens":10,"output_tokens":20}}`
	u := usageFromJSONBody([]byte(body))
	if u.ServerTool.WebSearchRequests != 0 {
		t.Fatalf("absent server_tool_use must read 0, got %d", u.ServerTool.WebSearchRequests)
	}
}

// TestUsageScannerServerToolUse reads web_search_requests from a late SSE
// message_delta frame and confirms last-wins (no double count from message_start).
func TestUsageScannerServerToolUse(t *testing.T) {
	s := &usageScanner{}
	frames := []string{
		`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20,"server_tool_use":{"web_search_requests":3}}}`,
		`event: message_stop
data: {"type":"message_stop"}`,
	}
	for _, f := range frames {
		if _, err := s.Write([]byte(f + "\n\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	u := s.Counts()
	if u.Output != 20 {
		t.Fatalf("output = %d, want 20", u.Output)
	}
	if u.ServerTool.WebSearchRequests != 3 {
		t.Fatalf("server web_search_requests = %d, want 3", u.ServerTool.WebSearchRequests)
	}
	if s.Truncated() {
		t.Fatal("stream should not be truncated (message_stop seen)")
	}
}

// TestUsageScannerServerToolUseCumulativeNoDoubleCount: a second frame with
// cumulative counts must overwrite, not add.
func TestUsageScannerServerToolUseCumulativeNoDoubleCount(t *testing.T) {
	s := &usageScanner{}
	for _, f := range []string{
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20,"server_tool_use":{"web_search_requests":2}}}`,
		`data: {"type":"message_delta","delta":{},"usage":{"server_tool_use":{"web_search_requests":7}}}`,
		`data: {"type":"message_stop"}`,
	} {
		if _, err := s.Write([]byte(f + "\n\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := s.Counts().ServerTool.WebSearchRequests; got != 7 {
		t.Fatalf("server web_search_requests = %d, want 7 (last-wins)", got)
	}
}

// TestServerToolUsePresentInAnyCase guards the scanner's cheap "usage" pre-filter:
// a frame mentioning server_tool_use must still be scanned.
func TestServerToolUsePresentInAnyCase(t *testing.T) {
	body := `{"usage":{"server_tool_use":{"web_search_requests":1}}}`
	if !strings.Contains(body, "usage") {
		t.Fatal("test body must contain usage substring")
	}
	u := usageFromJSONBody([]byte(body))
	if u.ServerTool.WebSearchRequests != 1 {
		t.Fatalf("web_search_requests = %d, want 1", u.ServerTool.WebSearchRequests)
	}
}
