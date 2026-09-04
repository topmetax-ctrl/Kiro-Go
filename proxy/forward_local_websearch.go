package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/metrics"
	"kiro-go/providererr"
	"kiro-go/search"
)

// forwardLocalRoundResult is the parsed outcome of one buffered upstream round
// in the forward local loop.
type forwardLocalRoundResult struct {
	text         string
	toolUses     []KiroToolUse
	inputTokens  int
	outputTokens int
	stopReason   string
}

// forwardLocalWebSearch handles the "local" web_search strategy for a forwarded
// Claude route. It keeps model inference pinned to the SAME resolved provider
// route for every round (same ProviderID + same route/model-target), while
// executing any web_search tool uses via the proxy-side SearchOrchestrator.
// The loop never silently falls back to the Kiro pool; an explicit
// __kiro_pool__ target in a later tier is only consulted by tryForwardUpstream
// when the local loop explicitly declines.
//
// Non-stream only: buffering a full response per round is required to inspect
// tool uses before committing a response to the client. Streaming with tool
// interleaving would require a different contract and is rejected with 400
// under the local strategy.
func (h *Handler) forwardLocalWebSearch(r *http.Request, w http.ResponseWriter, origBody []byte, origReq ClaudeRequest, policy WebSearchPolicy, route *config.ModelRoute, targets []config.ResolvedTarget, captureUserText string) bool {
	if origReq.Stream {
		h.sendClaudeError(w, 400, "invalid_request_error", "web_search with local strategy does not support streaming; send stream=false or configure the provider as native")
		return true
	}

	// Pin to the first non-pool target that declared local; that is definitionally
	// the "same provider" for the rest of the loop (ProviderID + route tie).
	var pinned *config.ResolvedTarget
	for i := range targets {
		if targets[i].Provider.ID == config.KiroPoolTargetID {
			continue
		}
		if targets[i].Provider.WebSearchStrategyResolved() == config.ProviderWebSearchStrategyLocal {
			cp := targets[i]
			pinned = &cp
			break
		}
	}
	if pinned == nil {
		return false
	}

	ctx := r.Context()
	apiKeyID := apiKeyIDFromContext(ctx)
	clientModel := origReq.Model
	executor := newWebSearchExecutor(search.NewOrchestratorFromConfig(
		func() *http.Client { return GetForwardClientForProxy(config.GetProxyURL()) },
	))

	// Working request we mutate with tool results; origReq is the template.
	working := origReq
	// Deep copy messages so appends do not alias.
	working.Messages = append([]ClaudeMessage(nil), origReq.Messages...)

	agg := KiroRunResult{}
	cache := map[string]KiroToolResult{}
	sourcesByQuery := map[string][]SearchSource{}

	for round := 0; round < policy.MaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return true // client gone
		}

		// Build payload for this round. For round 0 it is origReq's body bytes
		// (with memory already injected). For later rounds we serialize working.
		var body []byte
		var err error
		if round == 0 {
			body = origBody
			// Apply model rewrite for the pinned target if any (same semantics as
			// forwardToTarget's model rewrite). origBody still has client's model;
			// rewrite here so the upstream sees the configured targetModel.
			if tm := strings.TrimSpace(pinned.Target.TargetModel); tm != "" {
				body, err = rewriteModelField(body, tm)
				if err != nil {
					h.sendClaudeError(w, 400, "invalid_request_error", err.Error())
					return true
				}
			}
			// Native web_search must become a synthetic client tool for the upstream
			// that is in local mode. Rewrite the tools array once before round 0.
			body, err = rewriteNativeWebSearchToSynthetic(body)
			if err != nil {
				h.sendClaudeError(w, 400, "invalid_request_error", err.Error())
				return true
			}
		} else {
			body, err = json.Marshal(working)
			if err != nil {
				h.sendClaudeError(w, 500, "api_error", "failed to serialize continuation")
				return true
			}
			// Model rewrite for continuation as well (same pinned mapping every round
			// — proves same-route invariant).
			if tm := strings.TrimSpace(pinned.Target.TargetModel); tm != "" {
				body, err = rewriteModelField(body, tm)
				if err != nil {
					h.sendClaudeError(w, 400, "invalid_request_error", err.Error())
					return true
				}
			}
			// working already has synthetic tool in req; no need to rewrite again
		}

		res, forwardUsage, forwardErr := h.forwardRoundBuffered(ctx, pinned.Provider, *pinned, body, clientModel, route, captureUserText, round == 0)
		if forwardErr != nil {
			// Upstream failure before any byte committed; surface the classified error.
			// Do not silently fall through to Kiro pool — that would change provider.
			h.sendPublicForwardError(w, true, forwardErr.Public())
			return true
		}

		// Attribute request metrics to the pinned provider/route (same ProviderID
		// every round). Record after each successful upstream round so dashboards
		// reflect the provider actually used, not the Kiro pool.
		h.recordForwardLocalSuccess(forwardUsage, pinned.Provider, route, clientModel, ctx, apiKeyID)

		agg.TotalInputTokens += res.inputTokens
		agg.TotalOutputTokens += res.outputTokens

		internal, external := partitionToolUses(res.toolUses, executor)
		if len(internal) == 0 {
			// No web_search requested: terminal round. The upstream's text is the
			// final answer; render it directly with native blocks if any searches
			// already ran.
			agg.FinalRound = KiroRoundResult{
				VisibleContent: res.text,
				ToolUses:       external,
			}
			return h.renderForwardLocalFinal(w, origReq, agg, res.text, policy)
		}
		if len(external) > 0 {
			h.sendClaudeError(w, 400, "invalid_request_error", fmt.Sprintf("mixed tool uses in one turn: web_search with %v is not supported in forwarded local mode", toolUseNames(external)))
			return true
		}
		if agg.SearchCalls+len(internal) > policy.MaxSearches {
			// Budget exhausted: feed a notice and do one final round with tool stripped.
			notices := make([]KiroToolResult, 0, len(internal))
			for _, call := range internal {
				notices = append(notices, KiroToolResult{
					ToolUseID: call.ToolUseID,
					Content:   []KiroResultContent{{Text: "WEB_SEARCH_ERROR: search budget exhausted; answer with the information already gathered."}},
					Status:    "success",
				})
			}
			working = advanceForwardWorking(working, res, notices)
			stripped := stripForwardWebSearchTools(working)
			strippedBody, _ := json.Marshal(stripped)
			if tm := strings.TrimSpace(pinned.Target.TargetModel); tm != "" {
				strippedBody, _ = rewriteModelField(strippedBody, tm)
			}
			finalRes, finalUsage, ferr := h.forwardRoundBuffered(ctx, pinned.Provider, *pinned, strippedBody, clientModel, route, captureUserText, false)
			if ferr != nil {
				h.sendPublicForwardError(w, true, ferr.Public())
				return true
			}
			h.recordForwardLocalSuccess(finalUsage, pinned.Provider, route, clientModel, ctx, apiKeyID)
			agg.TotalInputTokens += finalRes.inputTokens
			agg.TotalOutputTokens += finalRes.outputTokens
			agg.FinalRound = KiroRoundResult{VisibleContent: finalRes.text, ToolUses: finalRes.toolUses}
			return h.renderForwardLocalFinal(w, origReq, agg, finalRes.text, policy)
		}

		distinctBefore := countDistinctQueries(internal, cache)
		toolResults, sources, credits, invocations, execErr := (&kiroConversationRunner{caller: nil, executor: executor}).executeAll(ctx, internal, policy, cache, sourcesByQuery)
		if execErr != nil {
			h.sendClaudeError(w, 500, "api_error", execErr.Error())
			return true
		}
		agg.SearchCalls += len(internal)
		agg.BackendExecutions += distinctBefore
		agg.CacheHits += len(internal) - distinctBefore
		agg.SearchRounds++
		agg.Sources = append(agg.Sources, sources...)
		agg.TavilyCredits += credits
		agg.Searches = append(agg.Searches, invocations...)

		working = advanceForwardWorking(working, res, toolResults)
	}

	// Exhausted MaxRounds while still searching: one finalization round with tool stripped.
	stripped := stripForwardWebSearchTools(working)
	strippedBody, _ := json.Marshal(stripped)
	if tm := strings.TrimSpace(pinned.Target.TargetModel); tm != "" {
		strippedBody, _ = rewriteModelField(strippedBody, tm)
	}
	p2 := *pinned
	finalRes, finalUsage, ferr := h.forwardRoundBuffered(ctx, p2.Provider, p2, strippedBody, clientModel, route, captureUserText, false)
	if ferr != nil {
		h.sendPublicForwardError(w, true, ferr.Public())
		return true
	}
	h.recordForwardLocalSuccess(finalUsage, p2.Provider, route, clientModel, ctx, apiKeyID)
	agg.TotalInputTokens += finalRes.inputTokens
	agg.TotalOutputTokens += finalRes.outputTokens
	agg.FinalRound = KiroRoundResult{VisibleContent: finalRes.text, ToolUses: finalRes.toolUses}
	return h.renderForwardLocalFinal(w, origReq, agg, finalRes.text, policy)
}

// forwardRoundBuffered does one buffered (non-stream) POST to the pinned provider
// and parses content[] for text/tool_use. It records no client response itself;
// the caller owns the ResponseWriter. Returns usage for metrics.
func (h *Handler) forwardRoundBuffered(ctx context.Context, provider config.UpstreamProvider, rt config.ResolvedTarget, body []byte, clientModel string, route *config.ModelRoute, captureUserText string, isFirstRound bool) (*forwardLocalRoundResult, usageCounts, *providerForwardError) {
	// Pick the first healthy connection for this provider (same determinism as
	// orderProviderConnections; pinning is at ProviderID level, not connection,
	// so rotating within the provider is still "same provider" per spec).
	conns := orderProviderConnections(provider, time.Now())
	if len(conns) == 0 {
		return nil, usageCounts{}, newProviderForwardError(502, "no enabled connections")
	}
	conn := conns[0]

	proxyURL := provider.ProxyURL
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	client := GetForwardClientForProxy(proxyURL)
	url := strings.TrimRight(provider.BaseURL, "/") + "/messages"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, usageCounts{}, newProviderForwardError(500, "failed to build upstream request: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("anthropic-version", "2023-06-01")
	if conn.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+conn.ApiKey)
		req.Header.Set("X-Api-Key", conn.ApiKey)
	}
	// No extra forwarded headers for local rounds; the per-round body already
	// carries the full Claude request including beta/tool headers if needed.
	// Top-level r headers (anthropic-beta etc.) are intentionally not replayed
	// here per round to keep the loop hermetic.

	resp, err := client.Do(req)
	if err != nil {
		return nil, usageCounts{}, newProviderForwardError(502, "upstream request failed: "+err.Error())
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, usageCounts{}, newProviderForwardError(502, "upstream read failed: "+err.Error())
	}
	if resp.StatusCode != 200 {
		return nil, usageCounts{}, classifyForwardError(resp.StatusCode, b, resp.Header, provider, conn, clientModel, route)
	}

	usage := usageFromJSONBody(b)
	res, err := parseClaudeContentForForwardLocal(b)
	if err != nil {
		return nil, usage, newProviderForwardError(502, err.Error())
	}
	res.inputTokens = int(usage.Input)
	res.outputTokens = int(usage.Output)
	return res, usage, nil
}

// rewriteModelField replaces body.model with targetModel when set.
func rewriteModelField(body []byte, targetModel string) ([]byte, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	m["model"] = targetModel
	return json.Marshal(m)
}

// rewriteNativeWebSearchToSynthetic rewrites Anthropic native web_search server
// tools (type web_search_*) into a synthetic client-tool schema so a "local"
// provider can receive it as an ordinary tool. It preserves all other tools.
// Idempotent: if no native shape is present, the body is returned unchanged.
func rewriteNativeWebSearchToSynthetic(body []byte) ([]byte, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	rawTools, ok := m["tools"]
	if !ok {
		return body, nil
	}
	arr, ok := rawTools.([]interface{})
	if !ok {
		return body, nil
	}
	hasNative := false
	for _, it := range arr {
		if tm, ok := it.(map[string]interface{}); ok {
			typeStr, _ := tm["type"].(string)
			nameStr, _ := tm["name"].(string)
			if nameStr == webSearchToolName && strings.HasPrefix(strings.TrimSpace(typeStr), "web_search_") {
				hasNative = true
				break
			}
		}
	}
	if !hasNative {
		return body, nil
	}
	// Replace native entry with synthetic client-tool spec; keep others.
	newTools := make([]interface{}, 0, len(arr))
	hasSynthetic := false
	for _, it := range arr {
		tm, ok := it.(map[string]interface{})
		if !ok {
			newTools = append(newTools, it)
			continue
		}
		typeStr, _ := tm["type"].(string)
		nameStr, _ := tm["name"].(string)
		if nameStr == webSearchToolName && strings.HasPrefix(strings.TrimSpace(typeStr), "web_search_") {
			if hasSynthetic {
				continue
			}
			hasSynthetic = true
			// Synthesize a client tool with explicit query schema.
			newTools = append(newTools, map[string]interface{}{
				"name":         webSearchToolName,
				"description":  "Search the web for up-to-date information.",
				"input_schema": webSearchQuerySchema(),
			})
			continue
		}
		newTools = append(newTools, it)
	}
	m["tools"] = newTools
	return json.Marshal(m)
}

// parseClaudeContentForForwardLocal extracts text and tool uses from a
// non-stream Anthropic Messages response body.
func parseClaudeContentForForwardLocal(body []byte) (*forwardLocalRoundResult, error) {
	var env struct {
		Content    []map[string]interface{} `json:"content"`
		StopReason *string                  `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("failed to parse upstream content: %w", err)
	}
	var text string
	var toolUses []KiroToolUse
	for _, block := range env.Content {
		typeStr, _ := block["type"].(string)
		switch typeStr {
		case "text":
			if s, ok := block["text"].(string); ok {
				text += s
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			var input map[string]interface{}
			if raw, ok := block["input"]; ok {
				if m, ok := raw.(map[string]interface{}); ok {
					input = m
				}
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			toolUses = append(toolUses, KiroToolUse{ToolUseID: id, Name: name, Input: input})
		}
	}
	stop := ""
	if env.StopReason != nil {
		stop = *env.StopReason
	}
	return &forwardLocalRoundResult{text: text, toolUses: toolUses, stopReason: stop}, nil
}

// advanceForwardWorking appends one assistant+tool_result turn to working (Claude shape).
func advanceForwardWorking(working ClaudeRequest, res *forwardLocalRoundResult, toolResults []KiroToolResult) ClaudeRequest {
	// Assistant turn with text + tool_use blocks.
	assistantBlocks := make([]interface{}, 0, 1+len(res.toolUses))
	if res.text != "" {
		assistantBlocks = append(assistantBlocks, map[string]interface{}{"type": "text", "text": res.text})
	}
	for _, tu := range res.toolUses {
		assistantBlocks = append(assistantBlocks, map[string]interface{}{
			"type": "tool_use", "id": tu.ToolUseID, "name": tu.Name, "input": tu.Input,
		})
	}
	working.Messages = append(working.Messages, ClaudeMessage{Role: "assistant", Content: assistantBlocks})

	// User turn with tool_result blocks.
	userBlocks := make([]interface{}, 0, len(toolResults))
	for _, tr := range toolResults {
		text := ""
		if len(tr.Content) > 0 {
			text = tr.Content[0].Text
		}
		userBlocks = append(userBlocks, map[string]interface{}{
			"type": "tool_result", "tool_use_id": tr.ToolUseID, "content": text,
		})
	}
	working.Messages = append(working.Messages, ClaudeMessage{Role: "user", Content: userBlocks})
	return working
}

// stripForwardWebSearchTools removes the synthetic web_search tool so the model
// must answer from gathered evidence on the finalization round.
func stripForwardWebSearchTools(req ClaudeRequest) ClaudeRequest {
	out := make([]ClaudeTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		if isWebSearchTool(t) {
			continue
		}
		out = append(out, t)
	}
	req.Tools = out
	return req
}

// renderForwardLocalFinal writes the final non-stream JSON response, synthesizing
// native server_tool_use blocks when searches ran so the client renders
// "Did N searches" with citations.
func (h *Handler) renderForwardLocalFinal(w http.ResponseWriter, origReq ClaudeRequest, agg KiroRunResult, finalText string, policy WebSearchPolicy) bool {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	content := buildForwardLocalContentBlocks(agg, finalText)

	// Build usage with server_tool_use counter when searches happened.
	usage := map[string]interface{}{
		"input_tokens":  agg.TotalInputTokens,
		"output_tokens": agg.TotalOutputTokens,
	}
	if len(agg.Searches) > 0 {
		usage["server_tool_use"] = map[string]interface{}{"web_search_requests": len(agg.Searches)}
	}

	// Append deterministic Sources list when enabled and we have sources.
	if config.WebSearchAppendSources() && len(agg.Sources) > 0 {
		finalText = finalText + formatSourcesList(agg.Sources)
		// Rebuild content with amended text already in content blocks above is handled
		// by buildForwardLocalContentBlocks including sources in last text block.
	}

	resp := map[string]interface{}{
		"id":          "msg_" + randomID(),
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"model":       origReq.Model,
		"stop_reason": "end_turn",
		"usage":       usage,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warnf("[ForwardLocal] final encode failed: %v", err)
		return true
	}
	// Emit tool observability (kiro_orchestrator, pinned provider).
	if len(agg.Searches) > 0 {
		u := metrics.ToolUsage{
			TimeMs:     time.Now().UnixMilli(),
			RequestID:  requestIDFromContext(context.Background()),
			ToolKind:   metrics.ToolKindWebSearch,
			Origin:     metrics.ToolOriginKiroOrchestrator,
			Uses:       int64(len(agg.Searches)),
			Executions: int64(agg.BackendExecutions),
			CacheHits:  int64(agg.CacheHits),
		}
		metrics.RecordToolUsage(u)
	}
	return true
}

func buildForwardLocalContentBlocks(agg KiroRunResult, finalText string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, inv := range agg.Searches {
		out = append(out, map[string]interface{}{
			"type": "server_tool_use", "id": inv.ToolUseID, "name": "web_search", "input": map[string]interface{}{"query": inv.Query},
		})
		// web_search_tool_result with sources
		sourcesArr := make([]map[string]interface{}, 0, len(inv.Sources))
		for _, s := range inv.Sources {
			sourcesArr = append(sourcesArr, map[string]interface{}{"title": s.Title, "url": s.URL})
		}
		out = append(out, map[string]interface{}{
			"type": "web_search_tool_result", "tool_use_id": inv.ToolUseID,
			"content": sourcesArr,
		})
	}
	out = append(out, map[string]interface{}{"type": "text", "text": finalText})
	return out
}

func randomID() string {
	b := make([]byte, 12)
	for i := range b {
		b[i] = byte('a' + (time.Now().UnixNano()+int64(i))%26)
	}
	return string(b)
}

// providerForwardError is the classified forward error for a buffered round.
type providerForwardError struct {
	status  int
	message string
}

func (e *providerForwardError) Error() string { return e.message }
func (e *providerForwardError) Public() providererr.PublicError {
	return providererr.PublicError{
		Code:       providererr.CodeError,
		Message:    e.message,
		HTTPStatus: e.status,
	}
}
func newProviderForwardError(status int, msg string) *providerForwardError {
	return &providerForwardError{status: status, message: msg}
}
func classifyForwardError(status int, body []byte, header http.Header, provider config.UpstreamProvider, conn config.UpstreamConnection, clientModel string, route *config.ModelRoute) *providerForwardError {
	// Reuse the provider error boundary for consistent classification.
	in := providererr.FromHTTP(status, body, header)
	in.ProviderID, in.ProviderName = provider.ID, provider.Name
	in.ConnectionID, in.ConnectionName = conn.ID, config.SafeConnectionLabel(conn)
	pub := in.Public()
	if pub.Message == "" {
		pub.Message = "upstream request failed"
	}
	return newProviderForwardError(pub.HTTPStatus, pub.Message)
}

// recordForwardLocalSuccess records metrics for one successful buffered upstream round.
func (h *Handler) recordForwardLocalSuccess(usage usageCounts, provider config.UpstreamProvider, route *config.ModelRoute, clientModel string, ctx context.Context, apiKeyID string) {
	metrics.Record(metrics.Event{
		ClientModel:  clientModel,
		RouteID:      route.ID,
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		Endpoint:     "claude",
		Status:       200,
		Ok:           true,
	})
}
