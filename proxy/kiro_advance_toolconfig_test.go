package proxy

import "testing"

// The web-search runner builds each follow-up round with advancePayload, which
// carries structured tool_uses/tool_results forward. That traffic obliges the
// payload to declare a tool config too — upstream answers the mismatch with
//
//	400 ValidationException TOOL_CONFIG_MISSING
//	"The toolConfig field must be defined when using toolUse and toolResult
//	 content blocks."
//
// previousTools() returns nil whenever the client's original request declared no
// tools of its own, which is the normal case for server-side search: the proxy
// runs web_search itself, the client never asked for a tool. So the guarantee
// this test pins is not advancePayload's own doing — it comes from the
// repairKiroPayload call at the end, which synthesizes the declarations. Without
// that, every search round after the first would 400.
func TestAdvancePayloadDeclaresToolConfigForSearchRound(t *testing.T) {
	working := &KiroPayload{}
	cs := &working.ConversationState
	cs.ConversationID = "advance-toolcfg"
	cs.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "What is the weather in Hanoi?",
		Origin:  "AI_EDITOR",
		// No UserInputMessageContext at all: the client asked for no tools, so
		// previousTools() has nothing to carry over.
	}

	round := KiroRoundResult{
		VisibleContent: "Let me search.",
		ToolUses: []KiroToolUse{{
			ToolUseID: "ws_1",
			Name:      "web_search",
			Input:     map[string]interface{}{"query": "weather Hanoi"},
		}},
	}
	results := []KiroToolResult{{
		ToolUseID: "ws_1",
		Content:   []KiroResultContent{{Text: "27C, humid"}},
		Status:    "success",
	}}

	next := advancePayload(working, round, results)

	// The round's tool call and its result must both still be structured.
	merged := mergedConversation(next)
	census := takeConversationCensus(merged)
	if census.ToolUses == 0 {
		t.Fatalf("advancePayload dropped the structured tool call: %s", census)
	}
	if census.ToolResults == 0 {
		t.Fatalf("advancePayload dropped the structured tool result: %s", census)
	}
	if census.Unanswered != 0 || census.Orphans != 0 {
		t.Fatalf("advancePayload left broken pairing: %s", census)
	}

	// And the payload must declare a tool config for that traffic.
	uic := next.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if uic == nil || len(uic.Tools) == 0 {
		t.Fatalf("no tool config declared alongside structured tool traffic; "+
			"upstream would answer 400 TOOL_CONFIG_MISSING. census=%s", census)
	}
	found := false
	for _, w := range uic.Tools {
		if w.ToolSpecification.Name == "web_search" {
			found = true
		}
		if w.ToolSpecification.InputSchema.JSON == nil {
			t.Errorf("tool %q declared with a nil input schema", w.ToolSpecification.Name)
		}
	}
	if !found {
		t.Errorf("web_search is used in the conversation but not declared; tools=%d", len(uic.Tools))
	}

	// The conversation must also satisfy the ported shape rules.
	assertKiroPayloadValid(t, next)
}

// When the client DID supply tool specs, those must be carried forward unchanged
// rather than replaced by synthesized stand-ins.
func TestAdvancePayloadKeepsClientToolSpecs(t *testing.T) {
	clientTool := KiroToolWrapper{}
	clientTool.ToolSpecification.Name = "get_weather"
	clientTool.ToolSpecification.Description = "Real client-provided description."
	clientTool.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"city": map[string]interface{}{"type": "string"}},
		"required":   []interface{}{"city"},
	}}

	working := &KiroPayload{}
	cs := &working.ConversationState
	cs.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "Weather in Hanoi?",
		Origin:  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			Tools: []KiroToolWrapper{clientTool},
		},
	}

	round := KiroRoundResult{
		VisibleContent: "Searching.",
		ToolUses: []KiroToolUse{{
			ToolUseID: "ws_1", Name: "web_search",
			Input: map[string]interface{}{"query": "hanoi weather"},
		}},
	}
	results := []KiroToolResult{{
		ToolUseID: "ws_1",
		Content:   []KiroResultContent{{Text: "27C"}},
		Status:    "success",
	}}

	next := advancePayload(working, round, results)

	uic := next.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if uic == nil || len(uic.Tools) == 0 {
		t.Fatalf("client tool specs were lost")
	}
	for _, w := range uic.Tools {
		if w.ToolSpecification.Name != "get_weather" {
			continue
		}
		if w.ToolSpecification.Description != "Real client-provided description." {
			t.Errorf("client tool description was overwritten with %q",
				w.ToolSpecification.Description)
		}
		return
	}
	t.Errorf("client tool get_weather is no longer declared")
}
