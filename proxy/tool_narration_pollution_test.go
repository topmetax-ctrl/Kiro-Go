package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// TestNoToolInvocationTextInAssistantHistory is a regression guard for the
// few-shot pollution bug: when historical tool calls were narrated as
// "[Called tool X with input ...]" inside assistant turns, the model learned to
// emit that literal text instead of issuing real structured tool calls.
//
// The guard is unchanged by the switch to structured history tool calls: tool
// activity must still never appear as INVOCATION TEXT inside an assistant turn.
// What changed is where the activity legitimately lives — in structured
// toolUses/toolResults, which the model cannot mistake for text to imitate.
func TestNoToolInvocationTextInAssistantHistory(t *testing.T) {
	// Build a long OpenAI conversation with many completed tool cycles.
	msgs := []OpenAIMessage{{Role: "user", Content: "start a multi-step task"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			OpenAIMessage{Role: "assistant", Content: "", ToolCalls: []ToolCall{
				newPollToolCall(fmt.Sprintf("call_%d", i), "exec_command", fmt.Sprintf(`{"cmd":"step %d"}`, i)),
			}},
			OpenAIMessage{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", i), Content: fmt.Sprintf("OUTPUT_%d", i)},
			OpenAIMessage{Role: "user", Content: fmt.Sprintf("continue %d", i)},
		)
	}
	msgs = append(msgs, OpenAIMessage{Role: "user", Content: "summarize"})

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		// No assistant turn may contain tool-invocation-looking TEXT. This is the
		// pattern the model imitated; structured toolUses are not imitable.
		for _, bad := range []string{"[Called tool", "Called tool ", "with input {"} {
			if strings.Contains(a.Content, bad) {
				t.Fatalf("history[%d] assistant content contains mimicable tool text %q: %q", i, bad, a.Content)
			}
		}
	}

	// The structured tool calls must survive, so the model sees real examples of
	// issuing a tool call rather than examples of announcing and stopping.
	structuredUses := 0
	for _, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			structuredUses += len(a.ToolUses)
		}
	}
	if structuredUses != 8 {
		t.Fatalf("expected all 8 history tool calls to stay structured, got %d", structuredUses)
	}

	// Tool outputs must still be preserved, now as structured results.
	seen := map[string]bool{}
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage == nil {
			continue
		}
		for _, r := range turnToolResults(h) {
			for _, c := range r.Content {
				for i := 0; i < 8; i++ {
					if strings.Contains(c.Text, fmt.Sprintf("OUTPUT_%d", i)) {
						seen[fmt.Sprintf("OUTPUT_%d", i)] = true
					}
				}
			}
		}
		// Narrated text is also acceptable (orphan-repair path).
		for i := 0; i < 8; i++ {
			if strings.Contains(h.UserInputMessage.Content, fmt.Sprintf("OUTPUT_%d", i)) {
				seen[fmt.Sprintf("OUTPUT_%d", i)] = true
			}
		}
	}
	for i := 0; i < 8; i++ {
		marker := fmt.Sprintf("OUTPUT_%d", i)
		if !seen[marker] {
			t.Fatalf("tool output %q lost from history", marker)
		}
	}

	assertKiroPayloadValid(t, payload)
}

func newPollToolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// TestIdenticalToolResultsKeepTheirOwnPairs covers a client retry loop that
// sends the same failing output many times. The text is identical, but each
// result answers a DIFFERENT tool call (c0..c4), so the turns must not be
// collapsed: dropping four of them would leave four calls unanswered, which is
// exactly the TOOL_USES_AND_RESULTS violation that produces upstream 400s.
//
// An earlier version collapsed these runs to save context. That was safe only
// because the structured calls had already been stripped; with the pairing
// restored, the dedup pass must exempt turns carrying structured results.
func TestIdenticalToolResultsKeepTheirOwnPairs(t *testing.T) {
	msgs := []OpenAIMessage{{Role: "user", Content: "start"}}
	// 5 identical failing cycles in a row (model retrying the same tool).
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			OpenAIMessage{Role: "assistant", Content: "", ToolCalls: []ToolCall{
				newPollToolCall(fmt.Sprintf("c%d", i), "exec_command", `{"cmd":"x"}`),
			}},
			OpenAIMessage{Role: "tool", ToolCallID: fmt.Sprintf("c%d", i), Content: "SAME_ERROR_OUTPUT"},
		)
	}
	msgs = append(msgs, OpenAIMessage{Role: "user", Content: "final"})

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	// Every call keeps its own answering result.
	answered := map[string]bool{}
	for _, h := range payload.ConversationState.History {
		for _, r := range turnToolResults(h) {
			answered[r.ToolUseID] = true
		}
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("c%d", i)
		if !answered[id] {
			t.Fatalf("tool call %s lost its result; upstream would reject the request", id)
		}
	}

	assertKiroPayloadValid(t, payload)
}

// TestDropsDotPollutedAssistantTurns covers the second-order pollution: after
// stripping "[Called tool ...]" from assistant turns that held only that text,
// the turns become empty and must NOT be backfilled with ".". A history full of
// "." assistant turns trains the model to reply ".". Such hollow turns are
// dropped instead.
func TestDropsDotPollutedAssistantTurns(t *testing.T) {
	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 6; i++ {
		// Assistant turn that is pure replayed tool-call text (becomes empty after scrub).
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "[Called tool exec_command with input {\"cmd\":\"x\"}]"},
			ClaudeMessage{Role: "user", Content: "continue"},
		)
		// Also a turn that is already a bare "." (client-replayed prior placeholder).
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "."},
			ClaudeMessage{Role: "user", Content: "go on"},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "final question"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		c := strings.TrimSpace(a.Content)
		if c == "." || c == "" {
			t.Fatalf("history[%d] is a hollow/dot assistant turn that should have been dropped", i)
		}
		if strings.Contains(a.Content, "[Called tool") {
			t.Fatalf("history[%d] still contains replayed tool-call text", i)
		}
	}
}

// TestScrubsClientReplayedToolCallText covers the recovery path: a polluted
// client stored the model's "[Called tool ...]" text output as assistant
// history and replays it. The proxy must strip that text from assistant turns
// so the pattern is not reinforced.
func TestScrubsClientReplayedToolCallText(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "do the task"},
			// Assistant text the client captured from the model's polluted output.
			{Role: "assistant", Content: "Let me check.\n\n[Called tool exec_command with input {\"cmd\":\"pwd\"}]"},
			{Role: "user", Content: "continue"},
			{Role: "assistant", Content: "[Called tool exec_command with input {\"cmd\":\"ls\"}]"},
			{Role: "user", Content: "continue"},
		},
	}

	payload := ClaudeToKiro(req, false)

	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			if strings.Contains(a.Content, "[Called tool") {
				t.Fatalf("history[%d] still contains replayed tool-call text: %q", i, a.Content)
			}
		}
	}

	// The natural prose around the stripped marker must be preserved.
	var combined strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.AssistantResponseMessage != nil {
			combined.WriteString(h.AssistantResponseMessage.Content)
			combined.WriteString("\n")
		}
	}
	if !strings.Contains(combined.String(), "Let me check.") {
		t.Fatalf("expected surrounding assistant prose to survive scrubbing, got:\n%s", combined.String())
	}
}
