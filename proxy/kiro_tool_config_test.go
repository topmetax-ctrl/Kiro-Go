package proxy

import (
	"strings"
	"testing"
)

// ============================================================================
// TOOL_CONFIG_MISSING regression.
//
// Live upstream (eu-central-1, 2026-08-08) rejected a repaired payload with:
//
//	400 ValidationException TOOL_CONFIG_MISSING
//	"Bedrock error message: The toolConfig field must be defined when using
//	 toolUse and toolResult content blocks."
//
// The conversation was well formed by every rule in kiro_history_repair.go: the
// tool pair was intact and the turn order was legal. What was missing is the tool
// DECLARATION — the current message carried no specs, because the client's last
// turn ("never mind, just say OK") does not want a tool called.
//
// The old strip-everything workaround never hit this: it deleted the tool traffic
// that requires a config. Keeping pairs means the config must be kept in step
// with them.
// ============================================================================

// toolNamesIn returns the declared tool names on a payload's current message.
func toolNamesIn(payload *KiroPayload) []string {
	uctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if uctx == nil {
		return nil
	}
	names := make([]string, 0, len(uctx.Tools))
	for _, t := range uctx.Tools {
		names = append(names, t.ToolSpecification.Name)
	}
	return names
}

// payloadWith builds a payload from history turns plus a current user turn.
func payloadWith(current KiroUserInputMessage, history ...KiroHistoryMessage) *KiroPayload {
	p := &KiroPayload{}
	p.ConversationState.ChatTriggerType = "MANUAL"
	p.ConversationState.History = history
	p.ConversationState.CurrentMessage.UserInputMessage = current
	return p
}

// The exact shape live upstream rejected: an intact tool pair in history, and a
// final user turn that declares no tools.
func TestRepairDeclaresToolConfigWhenHistoryCarriesToolTraffic(t *testing.T) {
	p := payloadWith(
		KiroUserInputMessage{Content: "Never mind, just say OK.", Origin: "AI_EDITOR"},
		userTurn("What time is it in Hanoi?"),
		assistantToolTurn("", "call_1"),
		userResultTurn("", "call_1"),
		assistantTurn("It is 12:00 in Hanoi."),
	)

	repairKiroPayload(p)
	assertKiroPayloadValid(t, p)

	names := toolNamesIn(p)
	if len(names) == 0 {
		t.Fatalf("conversation carries a structured tool pair but the repaired payload "+
			"declares no tools — upstream answers this with 400 TOOL_CONFIG_MISSING.\n%s",
			dumpConversation(mergedConversation(p)))
	}
	// Every tool referenced by a surviving call must be declared.
	if !contains(names, "exec_command") {
		t.Fatalf("declared tools %v do not cover the tool called in history (exec_command)", names)
	}
}

// A plain chat turn must NOT be handed a tool config it never needed.
func TestRepairDoesNotInventToolConfigForPlainChat(t *testing.T) {
	p := payloadWith(
		KiroUserInputMessage{Content: "How are you?", Origin: "AI_EDITOR"},
		userTurn("Hello"),
		assistantTurn("Hi there."),
	)

	repairKiroPayload(p)
	assertKiroPayloadValid(t, p)

	if names := toolNamesIn(p); len(names) > 0 {
		t.Fatalf("plain chat turn was given tool specs %v; nothing in the conversation "+
			"uses tools, so no config should be synthesized", names)
	}
}

// Client-supplied specs always win: synthesis is a fallback, never an override.
func TestRepairKeepsClientToolSpecsOverSynthesized(t *testing.T) {
	p := payloadWith(
		KiroUserInputMessage{
			Content: "Check again.",
			Origin:  "AI_EDITOR",
			UserInputMessageContext: &UserInputMessageContext{
				Tools: []KiroToolWrapper{echoTool()},
			},
		},
		userTurn("What time is it?"),
		assistantToolTurn("", "call_1"),
		userResultTurn("", "call_1"),
		assistantTurn("Noon."),
	)

	repairKiroPayload(p)
	assertKiroPayloadValid(t, p)

	names := toolNamesIn(p)
	if len(names) != 1 || names[0] != "get_time" {
		t.Fatalf("client-declared specs must be preserved verbatim, got %v", names)
	}
}

// Every distinct tool name in history gets declared, not just the first.
func TestRepairDeclaresEveryToolNameSeenInHistory(t *testing.T) {
	hist := []KiroHistoryMessage{
		userTurn("Do two things."),
		assistantToolTurn("", "call_a"),
		userResultTurn("", "call_a"),
		assistantTurn("First done."),
		userTurn("And the second?"),
		assistantToolTurn("", "call_b"),
		userResultTurn("", "call_b"),
		assistantTurn("Second done."),
	}
	// Give the second call a different tool name than the helper's default.
	hist[5].AssistantResponseMessage.ToolUses[0].Name = "read_file"

	p := payloadWith(
		KiroUserInputMessage{Content: "Now summarize.", Origin: "AI_EDITOR"},
		hist...,
	)

	repairKiroPayload(p)
	assertKiroPayloadValid(t, p)

	names := toolNamesIn(p)
	for _, want := range []string{"exec_command", "read_file"} {
		if !contains(names, want) {
			t.Fatalf("tool %q is called in history but not declared; got %v", want, names)
		}
	}
}

// A conversation whose only tool traffic is a backfilled result still needs a
// config. The backfill synthesizes a result for an unanswered call, so the call's
// name survives and should be what gets declared.
func TestRepairDeclaresToolConfigAfterBackfill(t *testing.T) {
	p := payloadWith(
		KiroUserInputMessage{Content: "Forget it, say OK.", Origin: "AI_EDITOR"},
		userTurn("Run the command."),
		assistantToolTurn("", "unanswered_call"),
		userTurn("Actually stop."),
	)

	repairKiroPayload(p)
	assertKiroPayloadValid(t, p)

	if names := toolNamesIn(p); len(names) == 0 {
		t.Fatalf("backfilled tool pair left the payload without a tool config\n%s",
			dumpConversation(mergedConversation(p)))
	}
}

// Synthesized specs must be structurally complete: upstream validates the shape
// of each declaration, not merely its presence.
func TestSynthesizedToolSpecsAreWellFormed(t *testing.T) {
	specs := synthesizeToolSpecsFor([]KiroHistoryMessage{
		userTurn("go"),
		assistantToolTurn("", "call_1"),
		userResultTurn("", "call_1"),
	})
	if len(specs) == 0 {
		t.Fatal("expected at least one synthesized spec")
	}
	for _, s := range specs {
		if strings.TrimSpace(s.ToolSpecification.Name) == "" {
			t.Error("synthesized spec has an empty name")
		}
		if strings.TrimSpace(s.ToolSpecification.Description) == "" {
			t.Error("synthesized spec has an empty description")
		}
		schema, ok := s.ToolSpecification.InputSchema.JSON.(map[string]interface{})
		if !ok {
			t.Fatalf("synthesized schema is not a JSON object: %T", s.ToolSpecification.InputSchema.JSON)
		}
		if schema["type"] != "object" {
			t.Errorf("synthesized schema type = %v, want object", schema["type"])
		}
		if _, ok := schema["properties"]; !ok {
			t.Error("synthesized schema is missing a properties field")
		}
	}
}

// Synthesis must not fire for a conversation with no tool traffic at all.
func TestSynthesizeReturnsNothingWithoutToolTraffic(t *testing.T) {
	if specs := synthesizeToolSpecsFor([]KiroHistoryMessage{
		userTurn("hi"),
		assistantTurn("hello"),
	}); len(specs) != 0 {
		t.Fatalf("no tool traffic, but %d spec(s) were synthesized", len(specs))
	}
}

// Repair stays idempotent with synthesis in play: a second pass must not stack
// duplicate declarations onto the current message.
func TestRepairToolConfigIsIdempotent(t *testing.T) {
	p := payloadWith(
		KiroUserInputMessage{Content: "Say OK.", Origin: "AI_EDITOR"},
		userTurn("Run it."),
		assistantToolTurn("", "call_1"),
		userResultTurn("", "call_1"),
		assistantTurn("Done."),
	)

	repairKiroPayload(p)
	first := toolNamesIn(p)
	repairKiroPayload(p)
	second := toolNamesIn(p)

	if len(first) != len(second) {
		t.Fatalf("repair is not idempotent for tool specs: %v then %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("repair is not idempotent for tool specs: %v then %v", first, second)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
