package proxy

import (
	"fmt"
	"strings"

	"kiro-go/logger"
)

// ============================================================================
// Kiro conversation-shape rules, ported from the Kiro IDE agent bundle.
//
// WHY THIS FILE EXISTS
//
// An earlier fix (commit 72da572, "上游 400") concluded that "Kiro 上游不接受
// history 中携带结构化 toolUses/toolResults" and responded by stripping the
// structured toolUses from every history assistant turn except one. That
// diagnosis was wrong, and the workaround caused a worse bug.
//
// The Kiro IDE ships its own copy of the upstream conversation validator
// (node_modules/@kiro/agent/dist/validator-*.js inside
// Kiro.app/.../kiro.kiro-agent/dist/extension.js). Its rule set is:
//
//	STARTS_WITH_USER_MESSAGE   Conversation must start with a user message
//	ENDS_WITH_USER_MESSAGE     Conversation must end with a user message
//	ALTERNATING_MESSAGES       Between every two user messages there must be an
//	                           assistant message (and vice versa)
//	TOOL_USES_AND_RESULTS      If an assistant message has tool uses, the next
//	                           message must be a user message with corresponding
//	                           tool results
//	TOOL_RESULTS_AND_NO_USES   If there is a message with tool result, there has
//	                           to be a corresponding message with tool use
//	TOOL_RESULTS_ORPHAN_IDS    User message has toolResults whose toolUseIds do
//	                           not match any toolUse in the preceding assistant
//	                           message
//	NON_EMPTY_USER_MESSAGE     User messages must have either content or tool
//	                           results
//
// Structured tool calls in history are not merely allowed — TOOL_USES_AND_RESULTS
// and TOOL_RESULTS_AND_NO_USES *require* them to appear in matched pairs. The IDE
// itself emits structured toolUses for every tool turn in a session, with no cap.
// The HTTP 400 "Improperly formed request" was never caused by the presence of
// structured tool turns; it was caused by BROKEN PAIRING between them.
//
// Deleting all but one tool turn made the 400s disappear only because it left
// almost no pairs to break. The cost was severe: a long agent session shipped
// thousands of assistant turns that state an intention in one sentence and then
// end, with the tool output appearing in the *next user* turn as if a human had
// pasted it. That is a few-shot demonstration, repeated thousands of times, of
// "announce what you are about to do, then stop without calling a tool" — and
// the model duly imitates it, producing the "one sentence, then the turn ends"
// failure. Because the stripped turn carries no toolUses, mapClaudeStopReason
// reports end_turn, so the response is a protocol-valid success and no
// truncation detector can see it.
//
// The correct fix is to keep the structured tool turns and repair the pairing,
// which is exactly what the IDE does. It runs its message list through a repair
// pipeline before sending; the functions below are that pipeline, ported 1:1 and
// named for what they do. IDE symbol names are noted so the port can be
// re-verified against a future bundle.
// ============================================================================

// Conversation-shape rule identifiers. Values match the IDE's rule enum (i11) so
// log lines can be compared directly against IDE debug output.
const (
	ruleStartsWithUserMessage = "STARTS_WITH_USER_MESSAGE"
	ruleEndsWithUserMessage   = "ENDS_WITH_USER_MESSAGE"
	ruleAlternatingMessages   = "ALTERNATING_MESSAGES"
	ruleToolUsesAndResults    = "TOOL_USES_AND_RESULTS"
	ruleToolResultsAndNoUses  = "TOOL_RESULTS_AND_NO_USES"
	ruleToolResultsOrphanIDs  = "TOOL_RESULTS_ORPHAN_IDS"
	ruleNonEmptyUserMessage   = "NON_EMPTY_USER_MESSAGE"
)

// Filler turns the IDE injects to satisfy the shape rules (its U5/R6/d8).
const (
	fillerLeadingUserContent  = "Hello"
	fillerTrailingUserContent = "Continue"
	fillerAssistantContent    = "understood"
)

// missingToolResultText is substituted when an assistant tool call has no
// matching result anywhere in the conversation — the client's history is broken,
// or truncation dropped the result. The upstream requires a paired result, so one
// must be synthesized; status "error" keeps it honest rather than fabricating
// success. The IDE uses "Tool execution failed" here; the wording below is
// clearer about what actually happened, which matters because the model reads it.
const missingToolResultText = "Tool result unavailable (omitted from conversation history)."

// toolResultStatusError matches the upstream ToolResultStatus.ERROR enum value.
const toolResultStatusError = "error"

// synthesizedToolDescription labels a tool spec this file generated to satisfy
// upstream's tool-config requirement. It is deliberately explicit that the spec
// is a placeholder: it is only there so a conversation carrying tool traffic can
// declare the tools it references, not to invite a call.
const synthesizedToolDescription = "Tool referenced earlier in this conversation. Declared so the conversation's tool history stays valid; not offered for a new call."

// placeholderToolName declares a tool config for a conversation whose tool
// results survived but whose calls did not, leaving no real name to declare.
const placeholderToolName = "unavailable_tool"

// historyRuleViolation is one conversation-shape rule failure. Index is the
// position of the offending message in the merged conversation.
type historyRuleViolation struct {
	Rule    string
	Index   int
	Message string
}

func (v historyRuleViolation) String() string {
	return fmt.Sprintf("%s@%d: %s", v.Rule, v.Index, v.Message)
}

// repairContext carries the per-request fields that synthesized filler turns need
// so they look like the rest of the conversation.
type repairContext struct {
	ModelID string
	Origin  string
}

// ---------------------------------------------------------------------------
// Shape predicates (IDE: l7, c10, _6, E6, h8)
// ---------------------------------------------------------------------------

func isUserTurn(m KiroHistoryMessage) bool { return m.UserInputMessage != nil }

func isAssistantTurn(m KiroHistoryMessage) bool { return m.AssistantResponseMessage != nil }

// turnHasToolResults reports whether a user turn carries structured tool results.
func turnHasToolResults(m KiroHistoryMessage) bool {
	return m.UserInputMessage != nil &&
		m.UserInputMessage.UserInputMessageContext != nil &&
		len(m.UserInputMessage.UserInputMessageContext.ToolResults) > 0
}

// turnHasToolUses reports whether an assistant turn carries structured tool calls.
func turnHasToolUses(m KiroHistoryMessage) bool {
	return m.AssistantResponseMessage != nil && len(m.AssistantResponseMessage.ToolUses) > 0
}

// turnToolResults returns a user turn's structured results, or nil.
func turnToolResults(m KiroHistoryMessage) []KiroToolResult {
	if m.UserInputMessage == nil || m.UserInputMessage.UserInputMessageContext == nil {
		return nil
	}
	return m.UserInputMessage.UserInputMessageContext.ToolResults
}

// turnHasUserText reports whether a user turn has non-blank content.
func turnHasUserText(m KiroHistoryMessage) bool {
	return m.UserInputMessage != nil && strings.TrimSpace(m.UserInputMessage.Content) != ""
}

// toolUsesMatchResults reports whether tool calls and results correspond exactly
// — every call answered, and every result answering a call (IDE: h8). An empty
// call set matches trivially; results with no calls never match.
func toolUsesMatchResults(uses []KiroToolUse, results []KiroToolResult) bool {
	if len(uses) == 0 {
		return true
	}
	if len(results) == 0 {
		return false
	}
	resultIDs := make(map[string]bool, len(results))
	for _, r := range results {
		resultIDs[r.ToolUseID] = true
	}
	useIDs := make(map[string]bool, len(uses))
	for _, u := range uses {
		useIDs[u.ToolUseID] = true
		if !resultIDs[u.ToolUseID] {
			return false
		}
	}
	for _, r := range results {
		if !useIDs[r.ToolUseID] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Validator (IDE: k6 and its six rule checks w7/H4/W3/y7/P6/b6)
//
// Reports violations without mutating. Used for diagnostics: the repair pipeline
// is expected to leave zero violations, so anything reported after a repair is a
// defect in the repair itself.
// ---------------------------------------------------------------------------

func validateKiroConversation(msgs []KiroHistoryMessage) []historyRuleViolation {
	var out []historyRuleViolation

	// Must start with a user message (IDE: w7).
	if len(msgs) == 0 || !isUserTurn(msgs[0]) {
		out = append(out, historyRuleViolation{ruleStartsWithUserMessage, 0,
			"Conversation must start with a user message"})
	}

	// Must end with a user message (IDE: H4).
	if len(msgs) == 0 || !isUserTurn(msgs[len(msgs)-1]) {
		out = append(out, historyRuleViolation{ruleEndsWithUserMessage, len(msgs) - 1,
			"Conversation must end with a user message"})
	}

	// Roles must alternate (IDE: W3).
	for i := 1; i < len(msgs); i++ {
		prev, cur := msgs[i-1], msgs[i]
		if isUserTurn(prev) && isUserTurn(cur) {
			out = append(out, historyRuleViolation{ruleAlternatingMessages, i,
				"Between every two user messages there must be an assistant message"})
			break
		}
		if isAssistantTurn(prev) && isAssistantTurn(cur) {
			out = append(out, historyRuleViolation{ruleAlternatingMessages, i,
				"Between every two assistant messages there must be a user message"})
			break
		}
	}

	// Tool calls must be answered by the immediately following user turn, and a
	// results turn must follow a turn that actually made calls (IDE: y7).
	for i := 0; i < len(msgs)-1; i++ {
		cur, next := msgs[i], msgs[i+1]
		if isAssistantTurn(cur) && turnHasToolUses(cur) {
			if !isUserTurn(next) || !toolUsesMatchResults(cur.AssistantResponseMessage.ToolUses, turnToolResults(next)) {
				out = append(out, historyRuleViolation{ruleToolUsesAndResults, i + 1,
					"If an assistant message has tool uses, the next message must be a user message with corresponding tool results"})
				break
			}
		}
		if isAssistantTurn(cur) && !turnHasToolUses(cur) && isUserTurn(next) && turnHasToolResults(next) {
			out = append(out, historyRuleViolation{ruleToolResultsAndNoUses, i,
				"If there is a message with tool result, there has to be a corresponding message with tool use."})
			break
		}
	}

	// Result IDs must reference a call in the preceding assistant turn, with no
	// duplicates (IDE: P6).
	for i := 1; i < len(msgs); i++ {
		prev, cur := msgs[i-1], msgs[i]
		if !isAssistantTurn(prev) || !turnHasToolUses(prev) || !isUserTurn(cur) || !turnHasToolResults(cur) {
			continue
		}
		useIDs := make(map[string]bool)
		for _, u := range prev.AssistantResponseMessage.ToolUses {
			if u.ToolUseID != "" {
				useIDs[u.ToolUseID] = true
			}
		}
		seen := make(map[string]bool)
		bad := false
		for _, r := range turnToolResults(cur) {
			if r.ToolUseID == "" || !useIDs[r.ToolUseID] || seen[r.ToolUseID] {
				bad = true
				break
			}
			seen[r.ToolUseID] = true
		}
		if bad {
			out = append(out, historyRuleViolation{ruleToolResultsOrphanIDs, i,
				"User message has toolResults whose toolUseIds do not match any toolUse in the preceding assistant message."})
			break
		}
	}

	// Every user turn needs content or tool results (IDE: b6).
	for i, m := range msgs {
		if isUserTurn(m) && !turnHasUserText(m) && !turnHasToolResults(m) {
			out = append(out, historyRuleViolation{ruleNonEmptyUserMessage, i,
				"User messages must have either content or tool results"})
			break
		}
	}

	return out
}

// ---------------------------------------------------------------------------
// Repair pipeline (IDE: $3 = O5 → G5 → x6 → A5 → L6 → v13 → N4)
// ---------------------------------------------------------------------------

// repairKiroConversation rewrites a conversation so it satisfies every shape rule
// while preserving structured tool pairs. Stage order matches the IDE's pipeline;
// changing it can reintroduce violations (e.g. backfilling missing results before
// orphans are filtered would pair synthetic results against calls that are about
// to be rewritten).
func repairKiroConversation(msgs []KiroHistoryMessage, ctx repairContext) []KiroHistoryMessage {
	msgs = ensureStartsWithUserTurn(msgs, ctx)
	msgs = dropContentlessUserTurns(msgs)
	msgs = pullToolResultsAfterTheirCalls(msgs)
	msgs = filterOrphanToolResults(msgs)
	msgs = backfillMissingToolResults(msgs, ctx)
	msgs = insertFillerBetweenSameRoleTurns(msgs, ctx)
	msgs = ensureEndsWithUserTurn(msgs, ctx)
	return msgs
}

func newUserFiller(content string, ctx repairContext) KiroHistoryMessage {
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
		Content: content,
		ModelID: ctx.ModelID,
		Origin:  ctx.Origin,
	}}
}

func newAssistantFiller(content string) KiroHistoryMessage {
	return KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: content}}
}

// ensureStartsWithUserTurn prepends a greeting when the conversation opens with an
// assistant turn (IDE: O5).
func ensureStartsWithUserTurn(msgs []KiroHistoryMessage, ctx repairContext) []KiroHistoryMessage {
	if len(msgs) > 0 && isUserTurn(msgs[0]) {
		return msgs
	}
	return append([]KiroHistoryMessage{newUserFiller(fillerLeadingUserContent, ctx)}, msgs...)
}

// ensureEndsWithUserTurn appends a continuation when the conversation ends on an
// assistant turn (IDE: N4). Upstream always expects the final turn to be the user's.
func ensureEndsWithUserTurn(msgs []KiroHistoryMessage, ctx repairContext) []KiroHistoryMessage {
	if len(msgs) == 0 {
		return []KiroHistoryMessage{newUserFiller(fillerLeadingUserContent, ctx)}
	}
	if isUserTurn(msgs[len(msgs)-1]) {
		return msgs
	}
	return append(msgs, newUserFiller(fillerTrailingUserContent, ctx))
}

// dropContentlessUserTurns removes user turns that carry neither text nor tool
// results, which would trip NON_EMPTY_USER_MESSAGE (IDE: G5). The first user turn
// is always kept so the conversation retains an opening.
func dropContentlessUserTurns(msgs []KiroHistoryMessage) []KiroHistoryMessage {
	if len(msgs) <= 1 {
		return msgs
	}
	firstUser := -1
	for i := range msgs {
		if isUserTurn(msgs[i]) {
			firstUser = i
			break
		}
	}
	out := make([]KiroHistoryMessage, 0, len(msgs))
	for i, m := range msgs {
		if !isUserTurn(m) || i == firstUser {
			out = append(out, m)
			continue
		}
		if turnHasUserText(m) || turnHasToolResults(m) || len(m.UserInputMessage.Images) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// pullToolResultsAfterTheirCalls moves a results turn to sit directly after the
// assistant turn that made the calls (IDE: x6). Clients that batch or reorder
// parallel tool calls can otherwise separate a pair, which reads as a violation
// even though both halves are present.
func pullToolResultsAfterTheirCalls(msgs []KiroHistoryMessage) []KiroHistoryMessage {
	var callTurns []int
	resultHome := make(map[string]int) // toolUseId -> index of the turn carrying its result
	for i, m := range msgs {
		switch {
		case isAssistantTurn(m) && turnHasToolUses(m):
			callTurns = append(callTurns, i)
		case isUserTurn(m) && turnHasToolResults(m):
			for _, r := range turnToolResults(m) {
				if r.ToolUseID != "" {
					if _, seen := resultHome[r.ToolUseID]; !seen {
						resultHome[r.ToolUseID] = i
					}
				}
			}
		}
	}
	if len(callTurns) == 0 {
		return msgs
	}

	out := make([]KiroHistoryMessage, 0, len(msgs))
	placed := make(map[int]bool, len(msgs))
	for i, m := range msgs {
		if placed[i] {
			continue
		}
		out = append(out, m)
		placed[i] = true
		if !isAssistantTurn(m) || !turnHasToolUses(m) {
			continue
		}
		for _, u := range m.AssistantResponseMessage.ToolUses {
			home, ok := resultHome[u.ToolUseID]
			if !ok || home == i+1 || placed[home] {
				continue
			}
			out = append(out, msgs[home])
			placed[home] = true
		}
	}
	return out
}

// filterOrphanToolResults drops result entries whose toolUseId matches no call in
// the immediately preceding assistant turn, along with duplicate IDs (IDE: A5).
//
// Deviation from the IDE, deliberate: where the IDE discards an orphaned results
// turn that has no text of its own, this narrates the orphaned output into the
// turn's content instead, so the tool output is not silently lost. Narration into
// a USER turn is safe — the model reads user turns but never authors them, so
// there is no invocation pattern for it to imitate. Narrating tool activity into
// an ASSISTANT turn is what caused the earlier "[Called tool ...]" pollution and
// must never be reintroduced.
func filterOrphanToolResults(msgs []KiroHistoryMessage) []KiroHistoryMessage {
	out := make([]KiroHistoryMessage, 0, len(msgs))
	for i, m := range msgs {
		if !isUserTurn(m) || !turnHasToolResults(m) {
			out = append(out, m)
			continue
		}

		var callIDs map[string]bool
		var toolNames map[string]string
		if i > 0 && isAssistantTurn(msgs[i-1]) && turnHasToolUses(msgs[i-1]) {
			callIDs = make(map[string]bool)
			toolNames = make(map[string]string)
			for _, u := range msgs[i-1].AssistantResponseMessage.ToolUses {
				if u.ToolUseID != "" {
					callIDs[u.ToolUseID] = true
					toolNames[u.ToolUseID] = u.Name
				}
			}
		}

		all := turnToolResults(m)
		kept := make([]KiroToolResult, 0, len(all))
		var orphaned []KiroToolResult
		seen := make(map[string]bool, len(all))
		for _, r := range all {
			if r.ToolUseID == "" || !callIDs[r.ToolUseID] || seen[r.ToolUseID] {
				orphaned = append(orphaned, r)
				continue
			}
			seen[r.ToolUseID] = true
			kept = append(kept, r)
		}

		if len(kept) == len(all) {
			out = append(out, m)
			continue
		}

		repaired := *m.UserInputMessage
		if narrated := narrateToolResults(orphaned, toolNames); narrated != "" {
			repaired.Content = joinHistoryText(repaired.Content, narrated)
		}
		if len(kept) > 0 {
			ctxCopy := *m.UserInputMessage.UserInputMessageContext
			ctxCopy.ToolResults = kept
			repaired.UserInputMessageContext = &ctxCopy
		} else {
			repaired.UserInputMessageContext = nil
		}
		if strings.TrimSpace(repaired.Content) == "" && len(kept) == 0 && len(repaired.Images) == 0 {
			continue // nothing left to say; dropContentlessUserTurns' rule applies
		}
		out = append(out, KiroHistoryMessage{UserInputMessage: &repaired})
	}
	return out
}

// backfillMissingToolResults inserts a synthetic error result for any assistant
// tool call left unanswered (IDE: L6). Upstream rejects an unanswered call, so a
// placeholder is the only way to keep the rest of the conversation intact — this
// is what makes truncation safe: dropping the turn that held a result no longer
// corrupts the request.
//
// A call whose results live elsewhere in the conversation is answered with a
// placeholder rather than by moving them: pullToolResultsAfterTheirCalls has
// already had its chance to reunite them, and double-answering the same ID would
// trip TOOL_RESULTS_ORPHAN_IDS.
//
// When the adjacent turn answers SOME of the calls, its results are topped up in
// place instead of a second turn being appended. Appending would leave the
// partial turn with no call in front of it, which is itself a violation
// (TOOL_RESULTS_AND_NO_USES). Topping up keeps the real output that was present
// and synthesizes only what is missing.
func backfillMissingToolResults(msgs []KiroHistoryMessage, ctx repairContext) []KiroHistoryMessage {
	out := make([]KiroHistoryMessage, 0, len(msgs))
	skip := -1
	for i, m := range msgs {
		if i == skip {
			continue
		}
		out = append(out, m)
		if !isAssistantTurn(m) || !turnHasToolUses(m) {
			continue
		}
		uses := m.AssistantResponseMessage.ToolUses

		var next *KiroHistoryMessage
		if i+1 < len(msgs) {
			next = &msgs[i+1]
		}
		if next == nil || !isUserTurn(*next) || !turnHasToolResults(*next) {
			out = append(out, syntheticToolResultTurn(uses, ctx))
			continue
		}
		if toolUsesMatchResults(uses, turnToolResults(*next)) {
			continue
		}
		// The adjacent results belong to a different call turn; leave that pairing
		// intact and answer this turn's calls with placeholders.
		answeredElsewhere := false
		for j, other := range msgs {
			if j == i || !isAssistantTurn(other) || !turnHasToolUses(other) {
				continue
			}
			if toolUsesMatchResults(other.AssistantResponseMessage.ToolUses, turnToolResults(*next)) {
				answeredElsewhere = true
				break
			}
		}
		if answeredElsewhere {
			out = append(out, syntheticToolResultTurn(uses, ctx))
			continue
		}
		// Partial overlap: rewrite the adjacent turn so its results match the calls
		// exactly, then consume it so it is not emitted twice.
		out = append(out, topUpToolResultTurn(*next, uses, ctx))
		skip = i + 1
	}
	return out
}

// topUpToolResultTurn returns the results turn rewritten to answer exactly the
// given calls: existing results are kept for the IDs they cover, missing IDs get a
// synthetic error result, and results that answer no call are dropped (their text
// is narrated into the turn's content so the output is not lost).
func topUpToolResultTurn(turn KiroHistoryMessage, uses []KiroToolUse, ctx repairContext) KiroHistoryMessage {
	existing := make(map[string]KiroToolResult, len(turnToolResults(turn)))
	var unmatched []KiroToolResult
	wanted := make(map[string]bool, len(uses))
	for _, u := range uses {
		if u.ToolUseID != "" {
			wanted[u.ToolUseID] = true
		}
	}
	for _, r := range turnToolResults(turn) {
		if r.ToolUseID != "" && wanted[r.ToolUseID] {
			if _, dup := existing[r.ToolUseID]; !dup {
				existing[r.ToolUseID] = r
				continue
			}
		}
		unmatched = append(unmatched, r)
	}

	results := make([]KiroToolResult, 0, len(uses))
	names := make(map[string]string, len(uses))
	for i, u := range uses {
		id := u.ToolUseID
		if id == "" {
			id = fmt.Sprintf("toolUse_%d", i+1)
		}
		names[id] = u.Name
		if have, ok := existing[id]; ok {
			results = append(results, have)
			continue
		}
		results = append(results, KiroToolResult{
			ToolUseID: id,
			Content:   []KiroResultContent{{Text: missingToolResultText}},
			Status:    toolResultStatusError,
		})
	}

	rebuilt := *turn.UserInputMessage
	if narrated := narrateToolResults(unmatched, names); narrated != "" {
		rebuilt.Content = joinHistoryText(rebuilt.Content, narrated)
	}
	if rebuilt.ModelID == "" {
		rebuilt.ModelID = ctx.ModelID
	}
	if rebuilt.Origin == "" {
		rebuilt.Origin = ctx.Origin
	}
	rebuilt.UserInputMessageContext = &UserInputMessageContext{ToolResults: results}
	return KiroHistoryMessage{UserInputMessage: &rebuilt}
}

// syntheticToolResultTurn builds the placeholder results turn for a set of
// unanswered calls (IDE: g9). Calls with a blank ID get a positional one so the
// pairing is still exact.
func syntheticToolResultTurn(uses []KiroToolUse, ctx repairContext) KiroHistoryMessage {
	results := make([]KiroToolResult, 0, len(uses))
	for i, u := range uses {
		id := u.ToolUseID
		if id == "" {
			id = fmt.Sprintf("toolUse_%d", i+1)
		}
		results = append(results, KiroToolResult{
			ToolUseID: id,
			Content:   []KiroResultContent{{Text: missingToolResultText}},
			Status:    toolResultStatusError,
		})
	}
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
		Content: "",
		ModelID: ctx.ModelID,
		Origin:  ctx.Origin,
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: results,
		},
	}}
}

// insertFillerBetweenSameRoleTurns restores strict alternation by wedging a filler
// turn between consecutive same-role turns (IDE: v13). Runs last among the
// content-changing stages so the pairing stages see the real adjacency.
func insertFillerBetweenSameRoleTurns(msgs []KiroHistoryMessage, ctx repairContext) []KiroHistoryMessage {
	if len(msgs) <= 1 {
		return msgs
	}
	out := make([]KiroHistoryMessage, 0, len(msgs))
	out = append(out, msgs[0])
	for i := 1; i < len(msgs); i++ {
		last, cur := out[len(out)-1], msgs[i]
		switch {
		case isUserTurn(last) && isUserTurn(cur):
			out = append(out, newAssistantFiller(fillerAssistantContent))
		case isAssistantTurn(last) && isAssistantTurn(cur):
			out = append(out, newUserFiller(fillerTrailingUserContent, ctx))
		}
		out = append(out, cur)
	}
	return out
}

// ---------------------------------------------------------------------------
// Pre-repair cleanup
// ---------------------------------------------------------------------------

// prepareKiroConversation performs the context hygiene that is independent of the
// shape rules: scrubbing legacy pollution, removing tool specs from non-final
// turns, and collapsing redundancy. It runs before repairKiroConversation.
//
// Note what is deliberately NOT done here: assistant turns are never emptied of
// their structured toolUses, and an assistant turn that holds only tool calls is
// never dropped. A text-free assistant tool turn is legitimate — the model called
// a tool without narrating — and dropping it would orphan the results that follow.
func prepareKiroConversation(msgs []KiroHistoryMessage) []KiroHistoryMessage {
	out := make([]KiroHistoryMessage, 0, len(msgs))
	for i := range msgs {
		m := msgs[i]

		if a := m.AssistantResponseMessage; a != nil {
			if a.Content != "" {
				scrubbed := *a
				scrubbed.Content = stripPollutedToolCallText(a.Content)
				m.AssistantResponseMessage = &scrubbed
				a = &scrubbed
			}
			// Drop an assistant turn with no tool calls and no real content. Such
			// turns are left behind by scrubbing, or are the "." placeholder an
			// earlier version emitted; replayed in bulk the model imitates them.
			if len(a.ToolUses) == 0 {
				c := strings.TrimSpace(a.Content)
				if c == "" || c == minimalFallbackUserContent {
					continue
				}
			}
		}

		if u := m.UserInputMessage; u != nil && u.UserInputMessageContext != nil {
			// History turns must not advertise tool specs; only the final message
			// carries them, and it is attached after the repair pipeline runs.
			if len(u.UserInputMessageContext.Tools) > 0 {
				uc := *u
				cc := *u.UserInputMessageContext
				cc.Tools = nil
				if len(cc.ToolResults) == 0 {
					uc.UserInputMessageContext = nil
				} else {
					uc.UserInputMessageContext = &cc
				}
				m.UserInputMessage = &uc
			}
		}

		// Collapse a run of identical user turns, which a client stuck retrying the
		// same call produces. Turns carrying structured results are exempt: their
		// IDs pair with distinct calls, so collapsing them would orphan a call.
		if m.UserInputMessage != nil && !turnHasToolResults(m) && len(out) > 0 {
			prev := out[len(out)-1]
			if prev.UserInputMessage != nil && !turnHasToolResults(prev) &&
				len(m.UserInputMessage.Images) == 0 &&
				strings.TrimSpace(m.UserInputMessage.Content) != "" &&
				strings.TrimSpace(prev.UserInputMessage.Content) == strings.TrimSpace(m.UserInputMessage.Content) {
				continue
			}
		}

		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// Payload-level entry point
// ---------------------------------------------------------------------------

// repairKiroPayload normalizes a payload's conversation so upstream accepts it.
//
// History and the current message are merged into one list first, because most of
// the shape rules span that boundary: the current message's tool results pair with
// the last history assistant turn, and ENDS_WITH_USER_MESSAGE constrains which
// turn may be current at all. The list is repaired as a whole, then split back —
// the same construction the IDE uses (it repairs one array, then takes
// `history = list[:-1]`, `currentMessage = list[-1]`).
//
// The current message's tool specs are detached before the repair and re-attached
// after, so no stage can mistake them for history state or drop them.
//
// Safe to call more than once; repairing an already-valid conversation is a no-op.
func repairKiroPayload(payload *KiroPayload) {
	if payload == nil {
		return
	}
	cs := &payload.ConversationState

	ctx := repairContext{
		ModelID: cs.CurrentMessage.UserInputMessage.ModelID,
		Origin:  cs.CurrentMessage.UserInputMessage.Origin,
	}
	if ctx.Origin == "" {
		ctx.Origin = "AI_EDITOR"
	}

	// Detach the current message's tool specs; re-attached after the repair.
	current := cs.CurrentMessage.UserInputMessage
	var tools []KiroToolWrapper
	if current.UserInputMessageContext != nil {
		tools = current.UserInputMessageContext.Tools
		ctxCopy := *current.UserInputMessageContext
		ctxCopy.Tools = nil
		if len(ctxCopy.ToolResults) == 0 {
			current.UserInputMessageContext = nil
		} else {
			current.UserInputMessageContext = &ctxCopy
		}
	}

	merged := make([]KiroHistoryMessage, 0, len(cs.History)+1)
	merged = append(merged, cs.History...)
	merged = append(merged, KiroHistoryMessage{UserInputMessage: &current})

	before := validateKiroConversation(merged)
	censusBefore := takeConversationCensus(merged)
	if len(before) > 0 {
		logger.Debugf("[KiroHistory] repairing %d shape violation(s) across %d turns: %s",
			len(before), len(merged), formatViolations(before))
	}

	merged = prepareKiroConversation(merged)
	merged = repairKiroConversation(merged, ctx)

	// Positive evidence that structured tool pairs survive the repair. The bug this
	// file replaced was invisible precisely because it produced a valid-looking
	// payload, so "no violations" alone proves nothing — the counts do.
	censusAfter := takeConversationCensus(merged)
	if censusBefore.ToolUses > 0 || censusAfter.ToolUses > 0 {
		logger.Debugf("[KiroHistory] tool pairs preserved: before[%s] after[%s]",
			censusBefore, censusAfter)
	}
	if lost := censusBefore.ToolUses - censusAfter.ToolUses; lost > 0 {
		// Truncation may legitimately drop whole turns; anything else is a defect.
		logger.Warnf("[KiroHistory] repair dropped %d structured tool call(s): before[%s] after[%s]",
			lost, censusBefore, censusAfter)
	}
	if censusAfter.Unanswered > 0 || censusAfter.Orphans > 0 {
		logger.Warnf("[KiroHistory] repaired conversation still has %d unanswered call(s) and %d orphan result(s)",
			censusAfter.Unanswered, censusAfter.Orphans)
	}

	// The repair guarantees a trailing user turn; a violation here is a defect in
	// this file rather than in the client's request, so it is logged loudly.
	if after := validateKiroConversation(merged); len(after) > 0 {
		logger.Warnf("[KiroHistory] conversation still violates upstream rules after repair (%d turns) — upstream will likely return 400: %s",
			len(merged), formatViolations(after))
	}

	last := merged[len(merged)-1]
	if last.UserInputMessage == nil {
		// ensureEndsWithUserTurn makes this unreachable; guard anyway rather than
		// panicking on a malformed request.
		logger.Warnf("[KiroHistory] repaired conversation does not end with a user turn; keeping original current message")
		return
	}

	newCurrent := *last.UserInputMessage

	// A conversation that carries structured tool traffic MUST also advertise a
	// tool config, even when this particular turn does not want a tool called.
	// Upstream (Bedrock, via the Kiro runtime) rejects the mismatch outright:
	//
	//	400 ValidationException TOOL_CONFIG_MISSING
	//	"The toolConfig field must be defined when using toolUse and toolResult
	//	 content blocks."
	//
	// The old strip-everything workaround never hit this, because it deleted the
	// tool traffic that requires the config. Keeping pairs means the config has to
	// be kept in step with them — otherwise the last turn of a tool conversation
	// ("now summarize what you found"), which legitimately declares no tools,
	// would 400 on a payload that is otherwise perfectly well formed.
	//
	// Specs are synthesized from the tool names still present in the conversation
	// when the client did not supply them. A name and a permissive object schema
	// are enough to satisfy the config requirement; the model is not being invited
	// to call these (nothing in the turn asks it to), so schema fidelity does not
	// matter here — only that every referenced tool is declared.
	if len(tools) == 0 {
		tools = synthesizeToolSpecsFor(merged)
		if len(tools) > 0 {
			logger.Debugf("[KiroHistory] current message declared no tools but the conversation carries tool traffic; synthesized %d spec(s) to satisfy toolConfig", len(tools))
		}
	}
	if len(tools) > 0 {
		if newCurrent.UserInputMessageContext == nil {
			newCurrent.UserInputMessageContext = &UserInputMessageContext{}
		} else {
			ctxCopy := *newCurrent.UserInputMessageContext
			newCurrent.UserInputMessageContext = &ctxCopy
		}
		newCurrent.UserInputMessageContext.Tools = tools
	}
	// The current message must always say something; upstream rejects a blank turn
	// that carries no results either.
	if strings.TrimSpace(newCurrent.Content) == "" &&
		(newCurrent.UserInputMessageContext == nil || len(newCurrent.UserInputMessageContext.ToolResults) == 0) {
		if len(newCurrent.Images) > 0 {
			newCurrent.Content = normalizeUserContent("", true)
		} else {
			newCurrent.Content = minimalFallbackUserContent
		}
	}

	cs.CurrentMessage.UserInputMessage = newCurrent
	cs.History = merged[:len(merged)-1]
	if len(cs.History) == 0 {
		cs.History = nil
	}
}

// synthesizeToolSpecsFor builds the minimal tool declarations needed for a
// conversation that carries structured tool traffic but whose current message
// supplied no specs of its own.
//
// Only names are recoverable: a toolResult references its call by id, and the
// name lives on the assistant's toolUse. That is sufficient — the requirement
// being satisfied is "every tool referenced by the conversation is declared", not
// "the model can call it correctly". The schema is deliberately permissive rather
// than invented, so a synthesized spec can never contradict a real one the client
// sends on a later turn.
//
// Returns nil when the conversation carries no tool traffic, so a plain chat turn
// is never given a tool config it does not need.
func synthesizeToolSpecsFor(msgs []KiroHistoryMessage) []KiroToolWrapper {
	var names []string
	seen := make(map[string]bool)
	sawToolTraffic := false

	for _, m := range msgs {
		if isAssistantTurn(m) {
			for _, u := range m.AssistantResponseMessage.ToolUses {
				sawToolTraffic = true
				name := strings.TrimSpace(u.Name)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				names = append(names, name)
			}
			continue
		}
		if len(turnToolResults(m)) > 0 {
			sawToolTraffic = true
		}
	}

	// Results with no surviving call carry no name to declare. The conversation
	// still needs a config, so fall back to a single placeholder rather than
	// leaving it unset and taking a TOOL_CONFIG_MISSING.
	if sawToolTraffic && len(names) == 0 {
		names = []string{placeholderToolName}
	}
	if len(names) == 0 {
		return nil
	}

	specs := make([]KiroToolWrapper, 0, len(names))
	for _, name := range names {
		var w KiroToolWrapper
		w.ToolSpecification.Name = name
		w.ToolSpecification.Description = synthesizedToolDescription
		w.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}}
		specs = append(specs, w)
	}
	return specs
}

// conversationCensus counts the structural features that the earlier workaround
// destroyed, so production logs can show the repair is preserving tool pairs
// rather than only reporting failures.
type conversationCensus struct {
	Turns        int
	UserTurns    int
	AsstTurns    int
	ToolUses     int
	ToolResults  int
	Unanswered   int // calls with no matching result
	Orphans      int // results with no matching call
	Synthesized  int // results this file backfilled
	AsstToolOnly int // assistant turns that carry calls and no prose
}

func takeConversationCensus(msgs []KiroHistoryMessage) conversationCensus {
	var c conversationCensus
	c.Turns = len(msgs)

	callIDs := make(map[string]bool)
	resultIDs := make(map[string]bool)
	for _, m := range msgs {
		switch {
		case isAssistantTurn(m):
			c.AsstTurns++
			uses := m.AssistantResponseMessage.ToolUses
			c.ToolUses += len(uses)
			for _, u := range uses {
				callIDs[u.ToolUseID] = true
			}
			if len(uses) > 0 && strings.TrimSpace(m.AssistantResponseMessage.Content) == "" {
				c.AsstToolOnly++
			}
		case isUserTurn(m):
			c.UserTurns++
			for _, r := range turnToolResults(m) {
				c.ToolResults++
				resultIDs[r.ToolUseID] = true
				for _, part := range r.Content {
					if part.Text == missingToolResultText {
						c.Synthesized++
						break
					}
				}
			}
		}
	}
	for id := range callIDs {
		if !resultIDs[id] {
			c.Unanswered++
		}
	}
	for id := range resultIDs {
		if !callIDs[id] {
			c.Orphans++
		}
	}
	return c
}

func (c conversationCensus) String() string {
	return fmt.Sprintf("turns=%d(user=%d asst=%d) toolUses=%d toolResults=%d unanswered=%d orphans=%d synthesized=%d asstToolOnly=%d",
		c.Turns, c.UserTurns, c.AsstTurns, c.ToolUses, c.ToolResults,
		c.Unanswered, c.Orphans, c.Synthesized, c.AsstToolOnly)
}

func formatViolations(vs []historyRuleViolation) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, v.String())
	}
	return strings.Join(parts, "; ")
}
