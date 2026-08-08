package proxy

import (
	"strings"
	"testing"
)

// TestClaudeToKiroKeepsHistoryToolCyclesStructured covers the context-compaction
// scenario: a long conversation whose history contains completed tool cycles
// (assistant tool_use + user tool_result), followed by a plain-text instruction.
//
// History MUST retain those structured tool pairs. Upstream requires tool calls
// and results to appear as matched pairs (TOOL_USES_AND_RESULTS /
// TOOL_RESULTS_AND_NO_USES); it does not reject their presence. This test
// previously asserted the opposite, encoding the misdiagnosis described in
// kiro_history_repair.go — stripping the pairs is what produced the "model states
// an intention then ends the turn" failure.
func TestClaudeToKiroKeepsHistoryToolCyclesStructured(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run the build"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "running build"},
				map[string]interface{}{"type": "tool_use", "id": "t1", "name": "exec_command", "input": map[string]interface{}{"cmd": "make"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "build ok"},
			}},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t2", "name": "exec_command", "input": map[string]interface{}{"cmd": "test"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t2", "content": "tests pass"},
			}},
			// Final plain-text instruction (the compaction request).
			{Role: "user", Content: "Summarize everything that happened above."},
		},
	}

	payload := ClaudeToKiro(req, false)

	assertKiroPayloadValid(t, payload)

	// Both historical tool calls survive as structured tool uses.
	gotUses := map[string]bool{}
	gotResults := map[string]bool{}
	for _, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			for _, tu := range a.ToolUses {
				gotUses[tu.ToolUseID] = true
			}
		}
		for _, tr := range turnToolResults(h) {
			gotResults[tr.ToolUseID] = true
		}
	}
	for _, id := range []string{"t1", "t2"} {
		if !gotUses[id] {
			t.Fatalf("history lost structured tool use %q; upstream requires the pair", id)
		}
		if !gotResults[id] {
			t.Fatalf("history lost structured tool result for %q", id)
		}
	}

	// The current message is a plain instruction and must carry no tool results.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		t.Fatalf("current message should not carry structured toolResults for a plain instruction")
	}
	if !strings.Contains(cur.Content, "Summarize everything") {
		t.Fatalf("expected current content to be the compaction instruction, got %q", cur.Content)
	}

	// Tool output text must still be readable in the conversation.
	combined := allConversationText(payload)
	if !strings.Contains(combined, "tests pass") {
		t.Fatalf("expected tool result output to survive, got:\n%s", combined)
	}

	// Regression guard: assistant turns must never contain tool-invocation text.
	// Such text trains the model to emit it instead of real tool calls.
	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			if strings.Contains(a.Content, "[Called tool") {
				t.Fatalf("history[%d] assistant content contains mimicable tool-invocation text: %q", i, a.Content)
			}
		}
	}
}

// TestClaudeToKiroKeepsActiveToolTurnStructured verifies the in-progress tool
// case: the last assistant turn issues a tool_use and the final user message
// delivers the matching tool_result, which must stay structured on both sides.
func TestClaudeToKiroKeepsActiveToolTurnStructured(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: []ClaudeTool{{Name: "exec_command", Description: "run", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run ls"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t9", "name": "exec_command", "input": map[string]interface{}{"cmd": "ls"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t9", "content": "file1 file2"},
			}},
		},
	}

	payload := ClaudeToKiro(req, false)

	assertKiroPayloadValid(t, payload)

	hist := payload.ConversationState.History
	if len(hist) == 0 {
		t.Fatalf("expected non-empty history")
	}
	last := hist[len(hist)-1].AssistantResponseMessage
	if last == nil || len(last.ToolUses) != 1 || last.ToolUses[0].ToolUseID != "t9" {
		t.Fatalf("expected last history assistant to keep the active structured tool use t9")
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected current message to keep the matching structured tool result")
	}
	if cur.UserInputMessageContext.ToolResults[0].ToolUseID != "t9" {
		t.Fatalf("expected current tool result to answer t9, got %q", cur.UserInputMessageContext.ToolResults[0].ToolUseID)
	}
}
