package proxy

import (
	"strings"
	"testing"
)

// TestOpenAIToKiroDoesNotDuplicateToolResultText guards against a tool result's
// output being emitted twice — once as narrated text and once structurally.
// The original bug pre-filled a history entry with the "Tool results:"
// continuation text while also keeping the structured ToolResults, so the
// flattening pass narrated the same output a second time.
//
// Results now stay structured, so the output should appear exactly once across
// the whole payload, counting both structured result bodies and turn text.
func TestOpenAIToKiroDoesNotDuplicateToolResultText(t *testing.T) {
	const marker = "UNIQUE_OUTPUT_MARKER_12345"
	req := &OpenAIRequest{
		Model: "claude-opus-4.8",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "run it"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{newToolCall("call_1", "exec_command", `{"cmd":"ls"}`)}},
			{Role: "tool", ToolCallID: "call_1", Content: marker},
			{Role: "user", Content: "now summarize"},
		},
	}

	payload := OpenAIToKiro(req, false)

	count := 0
	for _, h := range mergedConversation(payload) {
		if h.UserInputMessage != nil {
			count += strings.Count(h.UserInputMessage.Content, marker)
		}
		if h.AssistantResponseMessage != nil {
			count += strings.Count(h.AssistantResponseMessage.Content, marker)
		}
		for _, r := range turnToolResults(h) {
			for _, c := range r.Content {
				count += strings.Count(c.Text, marker)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected tool result output exactly once in the payload, got %d\n%s",
			count, dumpConversation(mergedConversation(payload)))
	}

	assertKiroPayloadValid(t, payload)
}

func newToolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}
