package proxy

// advancePayload produces the payload for the next Kiro round after a search
// round. It preserves the single-active-tool-turn invariant that Kiro requires:
//
//  1. The working payload's current user message becomes a history user turn.
//  2. The just-received assistant round is appended as the final history turn,
//     carrying its structured tool_uses (the ones we executed).
//  3. A new current user message carries the structured tool_results (keyed to
//     those tool_uses) plus the original tool definitions so the model may
//     search again.
//  4. sanitizeKiroHistory flattens every older tool turn to text, leaving only
//     the final assistant turn structured (matched by the current toolResults).
//  5. The payload is re-truncated to the byte cap.
//
// The input payload is treated as the working copy owned by the runner; it is
// already a clone of the handler's original, so mutating it here is safe.
func advancePayload(working *KiroPayload, round KiroRoundResult, toolResults []KiroToolResult) *KiroPayload {
	next := cloneKiroPayload(working)

	cs := &next.ConversationState

	// (1) Demote the current user message to a history user turn. Strip its tool
	// context; history turns must not carry structured tool specs/results (they
	// get narrated to text by sanitizeKiroHistory anyway).
	prevUser := cs.CurrentMessage.UserInputMessage
	prevUser.UserInputMessageContext = nil
	cs.History = append(cs.History, KiroHistoryMessage{UserInputMessage: &prevUser})

	// (2) Append the assistant round with its structured tool_uses as the final
	// history turn. This is the "active" turn the current tool_results answer.
	internal := make([]KiroToolUse, 0, len(toolResults))
	for _, tu := range round.ToolUses {
		if isWebSearchToolName(tu.Name) {
			internal = append(internal, tu)
		}
	}
	cs.History = append(cs.History, KiroHistoryMessage{
		AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content:  round.VisibleContent,
			ToolUses: internal,
		},
	})

	// (3) New current user message: structured tool_results + preserved tool defs.
	origin := prevUser.Origin
	modelID := prevUser.ModelID
	tools := previousTools(working)
	cs.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: buildToolResultsContinuation(toolResults),
		ModelID: modelID,
		Origin:  origin,
		UserInputMessageContext: &UserInputMessageContext{
			Tools:       tools,
			ToolResults: toolResults,
		},
	}

	// (4) Keep exactly one active structured tool turn; flatten the rest.
	ids := collectToolResultIDs(toolResults)
	cs.History = sanitizeKiroHistory(cs.History, ids)

	// (5) Re-truncate to the byte cap (priming was folded into history already).
	truncatePayloadToLimit(next, false)
	return next
}

// previousTools returns the tool specifications carried by the working payload's
// current message, so the next round still advertises web_search (and any client
// tools) to the model.
func previousTools(working *KiroPayload) []KiroToolWrapper {
	uim := working.ConversationState.CurrentMessage.UserInputMessage
	if uim.UserInputMessageContext == nil {
		return nil
	}
	return uim.UserInputMessageContext.Tools
}

// stripWebSearchTools returns a clone of the payload with the web_search tool
// removed from the current message's tool list, forcing the model to answer from
// the evidence already gathered instead of issuing another search. Other (client)
// tools are preserved.
func stripWebSearchTools(payload *KiroPayload) *KiroPayload {
	next := cloneKiroPayload(payload)
	uim := &next.ConversationState.CurrentMessage.UserInputMessage
	if uim.UserInputMessageContext == nil {
		return next
	}
	kept := make([]KiroToolWrapper, 0, len(uim.UserInputMessageContext.Tools))
	for _, t := range uim.UserInputMessageContext.Tools {
		if isWebSearchToolName(t.ToolSpecification.Name) {
			continue
		}
		kept = append(kept, t)
	}
	uim.UserInputMessageContext.Tools = kept
	if len(kept) == 0 && len(uim.UserInputMessageContext.ToolResults) == 0 {
		uim.UserInputMessageContext = nil
	}
	return next
}
