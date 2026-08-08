package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// assertKiroPayloadValid checks a built payload against the conversation-shape
// rules ported from the Kiro IDE's own validator. Every translated payload must
// satisfy them, so this is the strongest oracle available without spending
// upstream quota: a violation here is what upstream answers with HTTP 400
// "Improperly formed request".
func assertKiroPayloadValid(t *testing.T, payload *KiroPayload) {
	t.Helper()
	if payload == nil {
		t.Fatalf("payload is nil")
	}
	merged := mergedConversation(payload)
	if v := validateKiroConversation(merged); len(v) > 0 {
		t.Fatalf("payload violates upstream conversation rules: %s\n%s",
			formatViolations(v), dumpConversation(merged))
	}
}

// mergedConversation rebuilds the full turn list (history + current message) the
// way upstream sees it.
func mergedConversation(payload *KiroPayload) []KiroHistoryMessage {
	cs := payload.ConversationState
	out := make([]KiroHistoryMessage, 0, len(cs.History)+1)
	out = append(out, cs.History...)
	cur := cs.CurrentMessage.UserInputMessage
	out = append(out, KiroHistoryMessage{UserInputMessage: &cur})
	return out
}

// dumpConversation renders a conversation compactly for failure messages.
func dumpConversation(msgs []KiroHistoryMessage) string {
	var b strings.Builder
	for i, m := range msgs {
		switch {
		case m.UserInputMessage != nil:
			ids := make([]string, 0)
			for _, r := range turnToolResults(m) {
				ids = append(ids, r.ToolUseID)
			}
			fmt.Fprintf(&b, "  [%d] user  results=%v content=%q\n", i, ids, truncForDump(m.UserInputMessage.Content))
		case m.AssistantResponseMessage != nil:
			ids := make([]string, 0)
			for _, u := range m.AssistantResponseMessage.ToolUses {
				ids = append(ids, u.ToolUseID)
			}
			fmt.Fprintf(&b, "  [%d] asst  uses=%v content=%q\n", i, ids, truncForDump(m.AssistantResponseMessage.Content))
		default:
			fmt.Fprintf(&b, "  [%d] EMPTY\n", i)
		}
	}
	return b.String()
}

func truncForDump(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Validator
// ---------------------------------------------------------------------------

func userTurn(content string) KiroHistoryMessage {
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: content, Origin: "AI_EDITOR"}}
}

func assistantTurn(content string) KiroHistoryMessage {
	return KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: content}}
}

func assistantToolTurn(content string, ids ...string) KiroHistoryMessage {
	uses := make([]KiroToolUse, 0, len(ids))
	for _, id := range ids {
		uses = append(uses, KiroToolUse{ToolUseID: id, Name: "exec_command", Input: map[string]interface{}{}})
	}
	return KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: content, ToolUses: uses}}
}

func userResultTurn(content string, ids ...string) KiroHistoryMessage {
	results := make([]KiroToolResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, KiroToolResult{
			ToolUseID: id,
			Content:   []KiroResultContent{{Text: "output for " + id}},
			Status:    "success",
		})
	}
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
		Content:                 content,
		Origin:                  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{ToolResults: results},
	}}
}

func ruleSet(vs []historyRuleViolation) map[string]bool {
	out := map[string]bool{}
	for _, v := range vs {
		out[v.Rule] = true
	}
	return out
}

func TestValidatorAcceptsWellFormedToolConversation(t *testing.T) {
	// A conversation with three structured tool cycles is valid. This is the
	// shape the Kiro IDE itself sends, and the fact that it validates is why the
	// "upstream rejects structured tool turns in history" premise was wrong.
	msgs := []KiroHistoryMessage{userTurn("start")}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("t%d", i)
		msgs = append(msgs, assistantToolTurn("working", id), userResultTurn("", id))
	}
	msgs = append(msgs, assistantTurn("done"), userTurn("thanks"))

	if v := validateKiroConversation(msgs); len(v) > 0 {
		t.Fatalf("expected valid conversation, got: %s", formatViolations(v))
	}
}

func TestValidatorFlagsEachRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []KiroHistoryMessage
		want string
	}{
		{
			name: "starts with assistant",
			msgs: []KiroHistoryMessage{assistantTurn("hi"), userTurn("hello")},
			want: ruleStartsWithUserMessage,
		},
		{
			name: "ends with assistant",
			msgs: []KiroHistoryMessage{userTurn("hi"), assistantTurn("hello")},
			want: ruleEndsWithUserMessage,
		},
		{
			name: "two users in a row",
			msgs: []KiroHistoryMessage{userTurn("a"), userTurn("b")},
			want: ruleAlternatingMessages,
		},
		{
			name: "tool use with no following result",
			msgs: []KiroHistoryMessage{userTurn("a"), assistantToolTurn("calling", "t1"), userTurn("unrelated")},
			want: ruleToolUsesAndResults,
		},
		{
			name: "results with no preceding use",
			msgs: []KiroHistoryMessage{userTurn("a"), assistantTurn("no calls"), userResultTurn("", "t1")},
			want: ruleToolResultsAndNoUses,
		},
		{
			name: "result id does not match the call",
			msgs: []KiroHistoryMessage{userTurn("a"), assistantToolTurn("calling", "t1"), userResultTurn("", "tX")},
			want: ruleToolUsesAndResults,
		},
		{
			name: "blank user turn",
			msgs: []KiroHistoryMessage{userTurn("a"), assistantTurn("b"), userTurn("   ")},
			want: ruleNonEmptyUserMessage,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ruleSet(validateKiroConversation(tc.msgs))
			if !got[tc.want] {
				t.Fatalf("expected rule %s to fire, got %v", tc.want, got)
			}
		})
	}
}

func TestValidatorFlagsDuplicateResultIDs(t *testing.T) {
	dup := KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
		Origin: "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{ToolResults: []KiroToolResult{
			{ToolUseID: "t1", Content: []KiroResultContent{{Text: "a"}}, Status: "success"},
			{ToolUseID: "t1", Content: []KiroResultContent{{Text: "b"}}, Status: "success"},
		}},
	}}
	msgs := []KiroHistoryMessage{userTurn("a"), assistantToolTurn("calling", "t1"), dup}
	got := ruleSet(validateKiroConversation(msgs))
	if !got[ruleToolUsesAndResults] && !got[ruleToolResultsOrphanIDs] {
		t.Fatalf("expected a pairing rule to fire for duplicate result IDs, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Repair pipeline
// ---------------------------------------------------------------------------

// TestRepairFixesEveryViolation is the core property: whatever shape goes in, a
// valid conversation comes out. Upstream 400s are exactly the cases this misses.
func TestRepairFixesEveryViolation(t *testing.T) {
	ctx := repairContext{ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}

	for _, tc := range []struct {
		name string
		msgs []KiroHistoryMessage
	}{
		{"empty", nil},
		{"assistant only", []KiroHistoryMessage{assistantTurn("hi")}},
		{"starts with assistant", []KiroHistoryMessage{assistantTurn("hi"), userTurn("hello")}},
		{"ends with assistant", []KiroHistoryMessage{userTurn("hi"), assistantTurn("bye")}},
		{"consecutive users", []KiroHistoryMessage{userTurn("a"), userTurn("b"), userTurn("c")}},
		{"consecutive assistants", []KiroHistoryMessage{userTurn("a"), assistantTurn("b"), assistantTurn("c"), userTurn("d")}},
		{"unanswered tool call", []KiroHistoryMessage{userTurn("a"), assistantToolTurn("calling", "t1"), userTurn("never mind")}},
		{"unanswered call at end", []KiroHistoryMessage{userTurn("a"), assistantToolTurn("calling", "t1")}},
		{"orphan results", []KiroHistoryMessage{userTurn("a"), assistantTurn("no calls"), userResultTurn("", "t1")}},
		{"mismatched ids", []KiroHistoryMessage{userTurn("a"), assistantToolTurn("calling", "t1"), userResultTurn("", "tX")}},
		{"partial results", []KiroHistoryMessage{userTurn("a"), assistantToolTurn("calling", "t1", "t2"), userResultTurn("", "t1")}},
		{"blank user turn", []KiroHistoryMessage{userTurn("a"), assistantTurn("b"), userTurn("  ")}},
		{"results separated from call", []KiroHistoryMessage{
			userTurn("a"), assistantToolTurn("calling", "t1"), userTurn("interjection"), userResultTurn("", "t1"),
		}},
		{"parallel calls answered together", []KiroHistoryMessage{
			userTurn("a"), assistantToolTurn("calling", "t1", "t2"), userResultTurn("", "t1", "t2"),
		}},
		{"already valid", []KiroHistoryMessage{
			userTurn("a"), assistantToolTurn("calling", "t1"), userResultTurn("", "t1"), assistantTurn("done"), userTurn("thanks"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := repairKiroConversation(append([]KiroHistoryMessage(nil), tc.msgs...), ctx)
			if v := validateKiroConversation(got); len(v) > 0 {
				t.Fatalf("repair left violations %s\n%s", formatViolations(v), dumpConversation(got))
			}
		})
	}
}

// TestRepairIsIdempotent guards the double-repair call sites (before and after
// truncation): a second pass must not churn an already-valid conversation.
func TestRepairIsIdempotent(t *testing.T) {
	ctx := repairContext{ModelID: "m", Origin: "AI_EDITOR"}
	msgs := []KiroHistoryMessage{
		userTurn("a"), assistantToolTurn("calling", "t1"), userTurn("interjection"),
		assistantToolTurn("calling again", "t2"), userResultTurn("", "t2"),
	}
	once := repairKiroConversation(append([]KiroHistoryMessage(nil), msgs...), ctx)
	twice := repairKiroConversation(append([]KiroHistoryMessage(nil), once...), ctx)

	if len(once) != len(twice) {
		t.Fatalf("second repair changed turn count: %d -> %d\nfirst:\n%s\nsecond:\n%s",
			len(once), len(twice), dumpConversation(once), dumpConversation(twice))
	}
	if v := validateKiroConversation(twice); len(v) > 0 {
		t.Fatalf("second repair introduced violations: %s", formatViolations(v))
	}
}

// TestRepairPreservesRealToolPairs is the anti-regression guard for the bug this
// work fixes: repair must not "solve" a problem by deleting tool calls.
func TestRepairPreservesRealToolPairs(t *testing.T) {
	ctx := repairContext{ModelID: "m", Origin: "AI_EDITOR"}
	msgs := []KiroHistoryMessage{userTurn("start")}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("t%d", i)
		msgs = append(msgs, assistantToolTurn("step", id), userResultTurn("", id))
	}
	msgs = append(msgs, assistantTurn("done"), userTurn("summary please"))

	got := repairKiroConversation(msgs, ctx)

	uses := 0
	for _, m := range got {
		if m.AssistantResponseMessage != nil {
			uses += len(m.AssistantResponseMessage.ToolUses)
		}
	}
	if uses != 12 {
		t.Fatalf("expected all 12 structured tool calls preserved, got %d\n%s", uses, dumpConversation(got))
	}
	if v := validateKiroConversation(got); len(v) > 0 {
		t.Fatalf("unexpected violations: %s", formatViolations(v))
	}
}

// TestBackfillSynthesizesErrorResultForDroppedResult is the truncation-safety
// property: when the turn holding a result is gone, the call gets a synthetic
// error result rather than corrupting the request.
func TestBackfillSynthesizesErrorResultForDroppedResult(t *testing.T) {
	ctx := repairContext{ModelID: "m", Origin: "AI_EDITOR"}
	msgs := []KiroHistoryMessage{userTurn("a"), assistantToolTurn("calling", "t1"), userTurn("next")}

	got := repairKiroConversation(msgs, ctx)

	if v := validateKiroConversation(got); len(v) > 0 {
		t.Fatalf("expected valid conversation, got %s\n%s", formatViolations(v), dumpConversation(got))
	}
	found := false
	for _, m := range got {
		for _, r := range turnToolResults(m) {
			if r.ToolUseID == "t1" {
				found = true
				if r.Status != toolResultStatusError {
					t.Fatalf("synthetic result should be status %q, got %q", toolResultStatusError, r.Status)
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected a synthetic result for t1\n%s", dumpConversation(got))
	}
}

// TestRepairKeepsCurrentMessageToolSpecs verifies tool definitions survive the
// merge/split round trip — losing them would silently disable tool calling.
func TestRepairKeepsCurrentMessageToolSpecs(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.History = []KiroHistoryMessage{
		userTurn("a"), assistantToolTurn("calling", "t1"),
	}
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "exec_command"
	tool.ToolSpecification.Description = "run"
	tool.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{"type": "object"}}

	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "",
		Origin:  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			Tools: []KiroToolWrapper{tool},
			ToolResults: []KiroToolResult{{
				ToolUseID: "t1",
				Content:   []KiroResultContent{{Text: "ok"}},
				Status:    "success",
			}},
		},
	}

	repairKiroPayload(payload)

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.Tools) != 1 {
		t.Fatalf("tool specs lost from current message")
	}
	if len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("current message tool results lost")
	}
	assertKiroPayloadValid(t, payload)
}

// TestRepairPayloadIsIdempotent covers the payload-level entry point being called
// twice around truncation.
func TestRepairPayloadIsIdempotent(t *testing.T) {
	build := func() *KiroPayload {
		p := &KiroPayload{}
		p.ConversationState.History = []KiroHistoryMessage{
			userTurn("a"), assistantToolTurn("calling", "t1"), userTurn("interjected"),
		}
		p.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
			Content: "carry on", Origin: "AI_EDITOR",
		}
		return p
	}
	once := build()
	repairKiroPayload(once)
	twice := build()
	repairKiroPayload(twice)
	repairKiroPayload(twice)

	if len(once.ConversationState.History) != len(twice.ConversationState.History) {
		t.Fatalf("repeated repair changed history length: %d -> %d",
			len(once.ConversationState.History), len(twice.ConversationState.History))
	}
	assertKiroPayloadValid(t, twice)
}

// TestPrepareKeepsTextFreeAssistantToolTurn guards a subtle regression: an
// assistant turn that called a tool without narrating has no text, and the old
// code dropped such turns, orphaning the results that followed.
func TestPrepareKeepsTextFreeAssistantToolTurn(t *testing.T) {
	msgs := []KiroHistoryMessage{
		userTurn("a"), assistantToolTurn("", "t1"), userResultTurn("", "t1"),
	}
	got := prepareKiroConversation(msgs)
	uses := 0
	for _, m := range got {
		if m.AssistantResponseMessage != nil {
			uses += len(m.AssistantResponseMessage.ToolUses)
		}
	}
	if uses != 1 {
		t.Fatalf("text-free assistant tool turn was dropped; results would be orphaned\n%s", dumpConversation(got))
	}
}

// TestPrepareDropsHollowAssistantTurns keeps the earlier pollution fix: turns
// left empty by scrubbing, or replayed "." placeholders, are removed.
func TestPrepareDropsHollowAssistantTurns(t *testing.T) {
	msgs := []KiroHistoryMessage{
		userTurn("a"),
		assistantTurn("."),
		userTurn("b"),
		assistantTurn("[Called tool exec_command with input {\"cmd\":\"x\"}]"),
		userTurn("c"),
	}
	got := prepareKiroConversation(msgs)
	for i, m := range got {
		if a := m.AssistantResponseMessage; a != nil {
			c := strings.TrimSpace(a.Content)
			if c == "" || c == "." {
				t.Fatalf("hollow assistant turn survived at %d", i)
			}
			if strings.Contains(a.Content, "[Called tool") {
				t.Fatalf("replayed tool-call text survived at %d", i)
			}
		}
	}
}

// allConversationText concatenates every turn's readable text, for assertions
// about tool output surviving somewhere the model can read it.
func allConversationText(payload *KiroPayload) string {
	var b strings.Builder
	for _, m := range mergedConversation(payload) {
		if m.UserInputMessage != nil {
			b.WriteString(m.UserInputMessage.Content)
			b.WriteString("\n")
			for _, r := range turnToolResults(m) {
				for _, c := range r.Content {
					b.WriteString(c.Text)
					b.WriteString("\n")
				}
			}
		}
		if m.AssistantResponseMessage != nil {
			b.WriteString(m.AssistantResponseMessage.Content)
			b.WriteString("\n")
			for _, u := range m.AssistantResponseMessage.ToolUses {
				b.WriteString(u.Name)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}
