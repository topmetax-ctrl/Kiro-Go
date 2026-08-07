package proxy

import (
	"context"
	"strings"

	"kiro-go/config"
)

// memoryContextOpen / memoryContextClose bound the injected memory block. The
// wrapper is explicit so the model can tell injected recall from the live
// conversation. Capture never sees these markers: lastClaudeUserText reads the
// ORIGINAL request messages, and the inject mutation happens on a request the
// caller owns after that text is already extracted.
const (
	memoryContextOpen  = "<memory>"
	memoryContextClose = "</memory>"
	memoryContextIntro = "Relevant facts from earlier sessions (may be outdated; prefer current context if conflicting):"
)

// memoryScopeForRequest resolves the per-caller memory scope principal for a
// request: the API key ID when auth put one on the context, else the shared
// anonymous scope. Reuses apiKeyIDFromContext so inject (search) and capture
// (add) key on the same principal the responses store already uses.
func memoryScopeForRequest(ctx context.Context) string {
	if id := apiKeyIDFromContext(ctx); id != "" {
		return id
	}
	return anonymousOwner
}

// lastClaudeUserText returns the most recent genuine user question, walking the
// messages backwards and skipping tool-result-only turns (which carry no new
// question). Mirrors firstClaudeConversationAnchor but from the end — the last
// user text is the retrieval query and the user side of a captured turn.
func lastClaudeUserText(messages []ClaudeMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		text, _, _ := extractClaudeUserContent(msg.Content)
		if strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
		// A tool-result-only (or empty) user turn is not a question; keep walking.
	}
	return ""
}

// buildMemoryContextBlock renders retrieved memories into a bounded context
// block, or "" when there is nothing to inject. Memories arrive most-relevant-
// first (Mem0 returns by score desc); bullets are added in order and the loop
// stops once the whole block (wrapper + intro + bullets) would exceed maxTokens,
// so the highest-scored facts survive the cap. The token budget is measured on
// the real accumulated string, so the estimate matches what reaches the model.
func buildMemoryContextBlock(mems []Memory, maxTokens int) string {
	if len(mems) == 0 {
		return ""
	}
	if maxTokens <= 0 {
		maxTokens = config.DefaultMemoryMaxInjectTokens
	}

	head := memoryContextOpen + "\n" + memoryContextIntro
	tail := "\n" + memoryContextClose

	var bullets strings.Builder
	added := 0
	for _, m := range mems {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		// Collapse whitespace so one memory stays one bullet line.
		text = strings.Join(strings.Fields(text), " ")

		// Measure the block as it WOULD be with this bullet included.
		candidate := head + bullets.String() + "\n- " + text + tail
		if estimateApproxTokens(candidate) > maxTokens && added > 0 {
			break
		}
		bullets.WriteString("\n- ")
		bullets.WriteString(text)
		added++
	}
	if added == 0 {
		return ""
	}
	return head + bullets.String() + tail
}

// injectMemoryIntoRequest prepends the memory context block to the last user
// message's content, leaving the real question intact after it. Returns true when
// a block was injected. It does NOT inject when block is empty or the request has
// no user message to attach to. Handles both string and []block content shapes.
//
// This mutates req in place (the caller passes a request it owns). It deliberately
// does NOT touch req.System — FilterClaudeCode replaces the entire system prompt,
// so an injected system block would be dropped by the translator.
func injectMemoryIntoRequest(req *ClaudeRequest, block string) bool {
	if req == nil || strings.TrimSpace(block) == "" {
		return false
	}
	// Find the last user message to attach to.
	idx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}

	msg := &req.Messages[idx]
	switch c := msg.Content.(type) {
	case string:
		msg.Content = block + "\n\n" + c
		return true
	case []interface{}:
		// Prepend a text block carrying the memory context, keeping existing blocks
		// (text, images, tool_results) after it in order.
		memBlock := map[string]interface{}{"type": "text", "text": block}
		msg.Content = append([]interface{}{memBlock}, c...)
		return true
	case nil:
		msg.Content = block
		return true
	default:
		// Unknown content shape: do not risk corrupting it.
		return false
	}
}
