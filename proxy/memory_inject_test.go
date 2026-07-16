package proxy

import (
	"strings"
	"testing"
)

func TestLastClaudeUserText(t *testing.T) {
	msgs := []ClaudeMessage{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "an answer"},
		{Role: "user", Content: "second question"},
	}
	if got := lastClaudeUserText(msgs); got != "second question" {
		t.Errorf("expected last user text, got %q", got)
	}
}

func TestLastClaudeUserTextSkipsToolResultOnly(t *testing.T) {
	// The final user turn carries only a tool_result (no new question); the
	// retrieval query should fall back to the prior genuine question.
	msgs := []ClaudeMessage{
		{Role: "user", Content: "how do I build this?"},
		{Role: "assistant", Content: "run make"},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "exit 0"},
		}},
	}
	if got := lastClaudeUserText(msgs); got != "how do I build this?" {
		t.Errorf("expected fallback to prior question, got %q", got)
	}
}

func TestLastClaudeUserTextEmptyWhenNoUser(t *testing.T) {
	msgs := []ClaudeMessage{{Role: "assistant", Content: "hi"}}
	if got := lastClaudeUserText(msgs); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBuildMemoryContextBlockEmpty(t *testing.T) {
	if got := buildMemoryContextBlock(nil, 1200); got != "" {
		t.Errorf("nil memories must produce empty block, got %q", got)
	}
	if got := buildMemoryContextBlock([]Memory{{Text: "   "}}, 1200); got != "" {
		t.Errorf("whitespace-only memory must produce empty block, got %q", got)
	}
}

func TestBuildMemoryContextBlockShape(t *testing.T) {
	block := buildMemoryContextBlock([]Memory{
		{Text: "uses Postgres for the ledger", Score: 0.9},
		{Text: "prefers tabs", Score: 0.5},
	}, 1200)
	if !strings.HasPrefix(block, memoryContextOpen) {
		t.Errorf("block must open with %q, got %q", memoryContextOpen, block)
	}
	if !strings.HasSuffix(block, memoryContextClose) {
		t.Errorf("block must close with %q, got %q", memoryContextClose, block)
	}
	if !strings.Contains(block, "- uses Postgres for the ledger") {
		t.Errorf("block missing first memory bullet: %q", block)
	}
	if !strings.Contains(block, "- prefers tabs") {
		t.Errorf("block missing second memory bullet: %q", block)
	}
}

func TestBuildMemoryContextBlockBoundsTokens(t *testing.T) {
	// Many long memories, a tiny cap: the block must keep at least the top one
	// but drop the rest, staying near the cap.
	mems := make([]Memory, 20)
	for i := range mems {
		mems[i] = Memory{Text: strings.Repeat("word ", 50), Score: float64(20 - i)}
	}
	block := buildMemoryContextBlock(mems, 60)
	bullets := strings.Count(block, "\n- ")
	if bullets == 0 {
		t.Fatal("expected at least one bullet under a tight cap")
	}
	if bullets == 20 {
		t.Errorf("token cap not enforced: all 20 memories included")
	}
	if got := estimateApproxTokens(block); got > 60*2 {
		t.Errorf("block far exceeds cap: est=%d", got)
	}
}

func TestInjectMemoryIntoRequestString(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: "what database do we use?"},
	}}
	ok := injectMemoryIntoRequest(req, "<memory>\n- uses Postgres\n</memory>")
	if !ok {
		t.Fatal("expected injection to succeed")
	}
	content, _ := req.Messages[0].Content.(string)
	if !strings.Contains(content, "<memory>") {
		t.Errorf("memory block not injected: %q", content)
	}
	if !strings.Contains(content, "what database do we use?") {
		t.Errorf("original question lost: %q", content)
	}
	// The memory block must come BEFORE the original question.
	if strings.Index(content, "<memory>") > strings.Index(content, "what database") {
		t.Errorf("memory block should precede the question: %q", content)
	}
}

func TestInjectMemoryIntoRequestBlocks(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "hello"},
		}},
	}}
	ok := injectMemoryIntoRequest(req, "<memory>\n- fact\n</memory>")
	if !ok {
		t.Fatal("expected injection to succeed")
	}
	blocks, _ := req.Messages[0].Content.([]interface{})
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (memory + original), got %d", len(blocks))
	}
	first, _ := blocks[0].(map[string]interface{})
	if txt, _ := first["text"].(string); !strings.Contains(txt, "<memory>") {
		t.Errorf("first block should be the memory block, got %v", first)
	}
}

func TestInjectMemoryIntoRequestNoUserMessage(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "assistant", Content: "hi"},
	}}
	if injectMemoryIntoRequest(req, "<memory>x</memory>") {
		t.Error("must not inject when there is no user message")
	}
}

func TestInjectMemoryIntoRequestEmptyBlock(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	if injectMemoryIntoRequest(req, "   ") {
		t.Error("must not inject an empty/whitespace block")
	}
	if req.Messages[0].Content.(string) != "hi" {
		t.Errorf("request must be unchanged, got %q", req.Messages[0].Content)
	}
}

func TestInjectMemoryTargetsLastUserMessage(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "new question"},
	}}
	injectMemoryIntoRequest(req, "<memory>\n- fact\n</memory>")
	// First user message must be untouched.
	if req.Messages[0].Content.(string) != "old question" {
		t.Errorf("first user message must be untouched, got %q", req.Messages[0].Content)
	}
	// Last user message must carry the block.
	if !strings.Contains(req.Messages[2].Content.(string), "<memory>") {
		t.Errorf("last user message should carry the block, got %q", req.Messages[2].Content)
	}
}
