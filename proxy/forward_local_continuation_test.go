package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAdvanceForwardWorkingPreservesStructuredAssistant is the P0 regression for
// the internal provider protocol: the continuation sent to the provider must keep
// the provider's own structured assistant content (notably the tool_use block
// whose id the following tool_result references), NOT a flattened text string.
func TestAdvanceForwardWorkingPreservesStructuredAssistant(t *testing.T) {
	raw := []map[string]interface{}{
		{"type": "text", "text": "I'll search."},
		{"type": "tool_use", "id": "toolu_123", "name": "web_search",
			"input": map[string]interface{}{"query": "rust latest"}},
		{"type": "text", "text": "one moment"},
	}
	res := &forwardLocalRoundResult{
		text:     "I'll search.one moment",
		toolUses: []KiroToolUse{{ToolUseID: "toolu_123", Name: "web_search"}},
		raw:      raw,
	}
	results := []KiroToolResult{{
		ToolUseID: "toolu_123",
		Content:   []KiroResultContent{{Text: "WEB_SEARCH_RESULTS part1 "}, {Text: "part2 END"}},
	}}

	working := ClaudeRequest{Model: "m", Messages: []ClaudeMessage{{Role: "user", Content: "q"}}}
	out := advanceForwardWorking(working, res, results)

	if len(out.Messages) != 3 {
		t.Fatalf("want user + assistant + user, got %d", len(out.Messages))
	}

	// Assistant turn keeps every structured block verbatim.
	assistant := out.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("messages[1].role = %q", assistant.Role)
	}
	blocks, ok := assistant.Content.([]interface{})
	if !ok {
		t.Fatalf("assistant content must stay a structured array, got %T (flattened?)", assistant.Content)
	}
	if len(blocks) != 3 {
		t.Fatalf("want 3 preserved blocks, got %d: %+v", len(blocks), blocks)
	}
	var sawToolUse bool
	for _, b := range blocks {
		m, ok := b.(map[string]interface{})
		if !ok {
			t.Fatalf("block not an object: %T", b)
		}
		if m["type"] == "tool_use" {
			sawToolUse = true
			if m["id"] != "toolu_123" {
				t.Fatalf("tool_use.id = %v, want toolu_123", m["id"])
			}
			if m["name"] != "web_search" {
				t.Fatalf("tool_use.name = %v", m["name"])
			}
		}
	}
	if !sawToolUse {
		t.Fatal("structured tool_use block was dropped — tool_result would dangle")
	}

	// User turn answers that exact id, with ALL content segments joined.
	user := out.Messages[2]
	ublocks, ok := user.Content.([]interface{})
	if !ok || len(ublocks) != 1 {
		t.Fatalf("user content = %#v", user.Content)
	}
	tr := ublocks[0].(map[string]interface{})
	if tr["type"] != "tool_result" {
		t.Fatalf("expected tool_result, got %v", tr["type"])
	}
	if tr["tool_use_id"] != "toolu_123" {
		t.Fatalf("tool_result.tool_use_id = %v, want toolu_123", tr["tool_use_id"])
	}
	if got := tr["content"]; got != "WEB_SEARCH_RESULTS part1 part2 END" {
		t.Fatalf("tool_result.content = %q (must join all segments)", got)
	}
}

// TestAdvanceForwardWorkingUsesClientToolProtocolNotServerTool proves the two
// protocols are not conflated: the body sent to provider round #2 must use
// ordinary tool_use/tool_result, never server_tool_use/web_search_tool_result.
func TestAdvanceForwardWorkingUsesClientToolProtocolNotServerTool(t *testing.T) {
	res := &forwardLocalRoundResult{
		toolUses: []KiroToolUse{{ToolUseID: "toolu_a", Name: "web_search"}},
		raw: []map[string]interface{}{
			{"type": "tool_use", "id": "toolu_a", "name": "web_search",
				"input": map[string]interface{}{"query": "q"}},
		},
	}
	results := []KiroToolResult{{ToolUseID: "toolu_a", Content: []KiroResultContent{{Text: "body"}}}}
	out := advanceForwardWorking(ClaudeRequest{Model: "m"}, res, results)

	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, forbidden := range []string{"server_tool_use", "web_search_tool_result", "web_search_result"} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("internal provider continuation must not contain %q (client-facing shape leaked): %s", forbidden, s)
		}
	}
	for _, required := range []string{`"type":"tool_use"`, `"type":"tool_result"`, `"tool_use_id":"toolu_a"`} {
		if !strings.Contains(s, required) {
			t.Fatalf("continuation missing %s: %s", required, s)
		}
	}
}

// TestAdvanceForwardWorkingFallbackWhenNoRaw covers providers whose body we could
// not capture structurally: the minimum protocol shape (text + tool_use) is still
// rebuilt so the tool_result has a matching call.
func TestAdvanceForwardWorkingFallbackWhenNoRaw(t *testing.T) {
	res := &forwardLocalRoundResult{
		text:     "searching",
		toolUses: []KiroToolUse{{ToolUseID: "toolu_z", Name: "web_search", Input: map[string]interface{}{"query": "q"}}},
	}
	results := []KiroToolResult{{ToolUseID: "toolu_z", Content: []KiroResultContent{{Text: "r"}}}}
	out := advanceForwardWorking(ClaudeRequest{Model: "m"}, res, results)

	blocks := out.Messages[0].Content.([]interface{})
	var sawToolUse bool
	for _, b := range blocks {
		if m := b.(map[string]interface{}); m["type"] == "tool_use" && m["id"] == "toolu_z" {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Fatalf("fallback must still emit tool_use toolu_z, got %+v", blocks)
	}
}
