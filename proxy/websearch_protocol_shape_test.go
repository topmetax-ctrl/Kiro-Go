package proxy

import (
	"encoding/json"
	"testing"
)

// requireWebSearchResultItems asserts the Anthropic contract for the content[]
// of a web_search_tool_result block: every item is type "web_search_result"
// and carries title/url.
func requireWebSearchResultItems(t *testing.T, raw interface{}, wantCount int) {
	t.Helper()
	items, ok := raw.([]map[string]interface{})
	if !ok {
		// tolerate []interface{} after a JSON round-trip
		asAny, ok2 := raw.([]interface{})
		if !ok2 {
			t.Fatalf("web_search_tool_result.content must be an array, got %T", raw)
		}
		if len(asAny) != wantCount {
			t.Fatalf("content len = %d, want %d", len(asAny), wantCount)
		}
		for i, it := range asAny {
			m, ok := it.(map[string]interface{})
			if !ok {
				t.Fatalf("content[%d] not an object: %T", i, it)
			}
			if m["type"] != "web_search_result" {
				t.Errorf("content[%d].type = %v, want web_search_result", i, m["type"])
			}
		}
		return
	}
	if len(items) != wantCount {
		t.Fatalf("content len = %d, want %d", len(items), wantCount)
	}
	for i, m := range items {
		if m["type"] != "web_search_result" {
			t.Errorf("content[%d].type = %v, want web_search_result", i, m["type"])
		}
		if _, ok := m["url"]; !ok {
			t.Errorf("content[%d] missing url", i)
		}
		if _, ok := m["title"]; !ok {
			t.Errorf("content[%d] missing title", i)
		}
	}
}

// TestWebSearchNativeBlockMapsShape pins the Anthropic contract for the shared
// non-stream renderer: server_tool_use.id == web_search_tool_result.tool_use_id,
// and every result item is typed web_search_result.
func TestWebSearchNativeBlockMapsShape(t *testing.T) {
	searches := []WebSearchInvocation{{
		ToolUseID: "srvtoolu_abc123",
		Query:     "golang generics",
		Sources: []SearchSource{
			{Title: "T1", URL: "https://one.example"},
			{Title: "T2", URL: "https://two.example"},
		},
	}}
	blocks := buildWebSearchNativeBlockMaps(searches)
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks (server_tool_use + result), got %d", len(blocks))
	}
	stu, res := blocks[0], blocks[1]
	if stu["type"] != "server_tool_use" {
		t.Fatalf("blocks[0].type = %v", stu["type"])
	}
	if stu["id"] != "srvtoolu_abc123" {
		t.Fatalf("server_tool_use.id = %v", stu["id"])
	}
	if res["type"] != "web_search_tool_result" {
		t.Fatalf("blocks[1].type = %v", res["type"])
	}
	// The correlation field the client uses to pair result -> server_tool_use.
	if res["tool_use_id"] != stu["id"] {
		t.Fatalf("tool_use_id = %v, must equal server_tool_use.id = %v", res["tool_use_id"], stu["id"])
	}
	requireWebSearchResultItems(t, res["content"], 2)
}

// TestWebSearchNativeBlocksShape is the same contract for the []ClaudeContentBlock
// renderer used by the Kiro runner's non-stream path.
func TestWebSearchNativeBlocksShape(t *testing.T) {
	blocks := buildWebSearchNativeBlocks([]WebSearchInvocation{{
		ToolUseID: "srvtoolu_x",
		Query:     "q",
		Sources:   []SearchSource{{Title: "T", URL: "https://e.example"}},
	}})
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Type != "server_tool_use" || blocks[0].ID != "srvtoolu_x" {
		t.Fatalf("server_tool_use block = %+v", blocks[0])
	}
	if blocks[1].Type != "web_search_tool_result" {
		t.Fatalf("result block type = %q", blocks[1].Type)
	}
	if blocks[1].ToolUseID != blocks[0].ID {
		t.Fatalf("tool_use_id %q must equal server_tool_use.id %q", blocks[1].ToolUseID, blocks[0].ID)
	}
	requireWebSearchResultItems(t, blocks[1].Content, 1)

	// Survives JSON serialization with both fields present.
	raw, err := json.Marshal(blocks[1])
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]interface{}
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if round["tool_use_id"] != "srvtoolu_x" {
		t.Fatalf("serialized tool_use_id = %v", round["tool_use_id"])
	}
}

// TestWebSearchResultItemsNeverNull guards the "content is always an array"
// invariant: no sources must serialize as [] rather than null.
func TestWebSearchResultItemsNeverNull(t *testing.T) {
	items := webSearchResultItems(nil)
	if items == nil {
		t.Fatal("items must be non-nil so it serializes as []")
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[]" {
		t.Fatalf("serialized = %s, want []", raw)
	}
}

// TestPurePathBlocksCarryToolUseID pins the pure/MCP non-stream path to the same
// correlation contract (this was previously missing tool_use_id).
func TestPurePathBlocksCarryToolUseID(t *testing.T) {
	snippet := "s"
	results := &WebSearchResults{Results: []WebSearchResult{
		{Title: "T", URL: "https://e.example", Snippet: &snippet},
	}}
	blocks := buildWebSearchContentBlocks("q", "srvtoolu_pure", results)

	var stu, res map[string]interface{}
	for _, b := range blocks {
		switch b["type"] {
		case "server_tool_use":
			stu = b
		case "web_search_tool_result":
			res = b
		}
	}
	if stu == nil || res == nil {
		t.Fatalf("expected both server_tool_use and web_search_tool_result in %+v", blocks)
	}
	if res["tool_use_id"] != stu["id"] {
		t.Fatalf("tool_use_id = %v, must equal server_tool_use.id = %v", res["tool_use_id"], stu["id"])
	}
	requireWebSearchResultItems(t, res["content"], 1)
}

// TestForwardLocalClientBlocksShape asserts the forwarded-local CLIENT-facing
// rendering matches the same native contract (the review regression).
func TestForwardLocalClientBlocksShape(t *testing.T) {
	agg := KiroRunResult{Searches: []WebSearchInvocation{{
		ToolUseID: "srvtoolu_fl",
		Query:     "rust release",
		Sources:   []SearchSource{{Title: "R", URL: "https://rust.example"}},
	}}}
	blocks := buildForwardLocalContentBlocks(agg, "final answer")
	if len(blocks) != 3 {
		t.Fatalf("want server_tool_use + result + text, got %d: %+v", len(blocks), blocks)
	}
	if blocks[1]["type"] != "web_search_tool_result" {
		t.Fatalf("blocks[1].type = %v", blocks[1]["type"])
	}
	if blocks[1]["tool_use_id"] != "srvtoolu_fl" {
		t.Fatalf("forward-local result must carry tool_use_id, got %v", blocks[1]["tool_use_id"])
	}
	requireWebSearchResultItems(t, blocks[1]["content"], 1)
	if blocks[2]["type"] != "text" || blocks[2]["text"] != "final answer" {
		t.Fatalf("final text block = %+v", blocks[2])
	}
}
