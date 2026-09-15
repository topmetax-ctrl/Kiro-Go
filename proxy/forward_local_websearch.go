package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"kiro-go/apikey"
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
	text     string
	toolUses []KiroToolUse
	// raw is the provider's content[] verbatim. It is what gets replayed into the
	// next round's assistant turn, so structured blocks (notably tool_use, whose
	// id the following tool_result references) survive the continuation instead of
	// being flattened into text.
	raw          []map[string]interface{}
	inputTokens  int
	outputTokens int
	stopReason   string
}

// forwardLocalConsumption accumulates what the pinned provider actually consumed
// across the loop's model rounds, so the whole logical request files exactly ONE
// metrics.Event.
//
// This is the accounting invariant the loop must not break: metrics.Event means
// one request/attempt outcome, and metrics.Record increments the request counters
// of every aggregate it touches (overall, provider, route, per-minute bucket). A
// three-round loop that filed one Event per round would therefore report three
// provider/API requests for one client request — the dashboard would disagree
// with the handler's own counters. Provider consumption is instead reported on
// the single Event via Event.ModelRounds, which is summed separately from
// requests, so "1 request, 3 model rounds, 450 tokens" is expressible without
// inflating volume.
type forwardLocalConsumption struct {
	rounds       int
	inputTokens  int64
	outputTokens int64
	costUSD      float64
	latencyMs    int64
	// stream is how the CLIENT asked to be served, which is what the metrics
	// store's streamed counter means. The upstream rounds are always buffered, so
	// reading the flag off a round would report every request as non-streamed.
	stream bool

	// Cache/reasoning sums across rounds, with per-field presence: a round that
	// reported no breakdown must not turn a sum into a false zero, so the
	// figure is only published on the Event when at least one round reported it.
	cacheReadTokens     int64
	cacheCreationTokens int64
	reasoningTokens     int64
	cacheReported       bool
	reasoningReported   bool
}

// addRound folds one completed upstream round into the totals.
func (c *forwardLocalConsumption) addRound(usage usageCounts, provider config.UpstreamProvider, latencyMs int64) {
	c.rounds++
	c.inputTokens += usage.Input
	c.outputTokens += usage.Output
	c.costUSD += provider.CostUSD(usage.Input, usage.Output)
	c.latencyMs += latencyMs
	if usage.cacheKnown() {
		c.cacheReported = true
		c.cacheReadTokens += usage.cacheReadOrZero()
		if usage.CacheCreationInputTokens != nil {
			c.cacheCreationTokens += *usage.CacheCreationInputTokens
		}
	}
	if usage.ReasoningOutputTokens != nil {
		c.reasoningReported = true
		c.reasoningTokens += *usage.ReasoningOutputTokens
	}
}

// usage builds the Event's nested usage object for the whole loop, or nil when
// no round reported anything beyond the flat totals.
func (c *forwardLocalConsumption) usage() *metrics.EventUsage {
	if !c.cacheReported && !c.reasoningReported {
		return nil
	}
	u := &metrics.EventUsage{Source: metrics.UsageSourceUpstream, Protocol: protocolAnthropic}
	if c.cacheReported {
		read := c.cacheReadTokens
		create := c.cacheCreationTokens
		u.CacheReadInputTokens = &read
		u.CacheCreationInputTokens = &create
	}
	if c.reasoningReported {
		r := c.reasoningTokens
		u.ReasoningOutputTokens = &r
	}
	return u
}

// forwardLocalWebSearch handles the "local" web_search strategy for a forwarded
// Claude route. It keeps model inference pinned to the SAME resolved provider
// route for every round (same ProviderID + same route/model-target), while
// executing any web_search tool uses via the proxy-side SearchOrchestrator.
// The loop never silently falls back to the Kiro pool; an explicit
// __kiro_pool__ target in a later tier is only consulted by tryForwardUpstream
// when the local loop explicitly declines.
//
// The loop is always buffered UPSTREAM: a full response per round has to be read
// before its tool uses can be inspected, and nothing may be committed to the
// client until the last round is known. That is an upstream constraint, not a
// client-facing one, so a client asking for stream=true still gets SSE — the
// completed result is replayed as a well-formed event sequence at the terminal
// point (see renderForwardLocalFinalStream). Claude Code always streams, so
// rejecting stream=true here would reject every real request.
func (h *Handler) forwardLocalWebSearch(r *http.Request, w http.ResponseWriter, origBody []byte, origReq ClaudeRequest, policy WebSearchPolicy, route *config.ModelRoute, targets []config.ResolvedTarget, captureUserText string) bool {
	// Pin to the first non-pool target that declared local, then never reassign
	// `pinned` for the lifetime of this loop. That is the mid-conversation
	// invariant: once the provider has produced an intermediate tool call, the
	// transcript belongs to THAT provider, so continuation must go back to the
	// same ProviderID and the same targetModel mapping.
	//
	// This is deliberately different from pre-conversation failover: before the
	// first successful round, tryForwardUpstream may walk the route's targets.
	// After it, a silent switch would hand a foreign provider a transcript
	// containing another provider's tool_use ids (and, for a differently-mapped
	// target, a different model) — so a mid-loop round failure surfaces as an
	// error instead of failing over. Mid-loop failover would need an explicit
	// documented policy plus transcript-compatibility guarantees; there is none.
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
	requestID := requestIDFromContext(ctx)
	apiKeyID := apiKeyIDFromContext(ctx)
	clientModel := origReq.Model
	// Resolve the canonical session identity once from the inbound request; every
	// continuation round replays it, so the whole local loop stays one logical
	// session for the pinned provider.
	sess := clientSessionAffinity(r)
	executor := newWebSearchExecutor(search.NewOrchestratorFromConfig(
		func() *http.Client { return GetForwardClientForProxy(config.GetProxyURL()) },
	))

	// Working request we mutate with tool results; origReq is the template.
	working := origReq
	// Deep copy messages so appends do not alias.
	working.Messages = append([]ClaudeMessage(nil), origReq.Messages...)
	// Rewrite native web_search in the working tool list ONCE (struct level), so
	// every continuation round serializes the synthetic client tool — not just
	// round 0's body bytes. round 0 still rewrites origBody because memory inject
	// may have enriched it separately.
	working.Tools = syntheticWebSearchTools(working.Tools)

	agg := KiroRunResult{}
	cache := map[string]KiroToolResult{}
	sourcesByQuery := map[string][]SearchSource{}
	// Provider consumption across every round of this ONE logical request. Filed
	// as a single metrics.Event at whichever terminal point ends the loop.
	consumed := forwardLocalConsumption{stream: origReq.Stream}

	// fail closes out the request on any non-success exit: one failure Event
	// carrying the consumption incurred so far, plus the tool usage already
	// performed. Every early return below goes through it so no path can leave the
	// request with zero Events (invisible on the dashboard) or two.
	fail := func(status int, errMsg string) bool {
		h.finishForwardLocalFailure(ctx, consumed, agg, pinned.Provider, *pinned, route, clientModel, apiKeyID, requestID, status, errMsg)
		return true
	}

	for round := 0; round < policy.MaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			// Client disconnected: recorded as canceled (not a failure), with the
			// consumption already incurred still attributed to the provider.
			return fail(499, "client canceled")
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
					return fail(400, "invalid_request_error: "+err.Error())
				}
			}
			// Round-0 body still carries the memory-enriched original; apply the
			// synthetic rewrite to the BYTES as well so the first upstream call and
			// all later serializations agree.
			body, err = rewriteNativeWebSearchToSynthetic(body)
			if err != nil {
				h.sendClaudeError(w, 400, "invalid_request_error", err.Error())
				return fail(400, "invalid_request_error: "+err.Error())
			}
		} else {
			body, err = json.Marshal(working)
			if err != nil {
				h.sendClaudeError(w, 500, "api_error", "failed to serialize continuation")
				return fail(500, "failed to serialize continuation")
			}
			// Model rewrite for continuation as well (same pinned mapping every round
			// — proves same-route invariant).
			if tm := strings.TrimSpace(pinned.Target.TargetModel); tm != "" {
				body, err = rewriteModelField(body, tm)
				if err != nil {
					h.sendClaudeError(w, 400, "invalid_request_error", err.Error())
					return fail(400, "invalid_request_error: "+err.Error())
				}
			}
		}

		roundStart := time.Now()
		res, forwardUsage, forwardErr := h.forwardRoundBuffered(ctx, pinned.Provider, *pinned, sess, body, clientModel, route, captureUserText, round == 0)
		if forwardErr != nil {
			// Upstream failure before any byte committed; surface the classified error.
			// Do not silently fall through to Kiro pool — that would change provider.
			h.sendPublicForwardError(w, true, forwardErr.Public())
			return fail(forwardErr.status, forwardErr.message)
		}

		// Accumulate provider consumption. Nothing is filed with metrics or the
		// handler counters here: one client request must produce exactly one
		// metrics.Event and exactly one recordSuccess, both at the terminal point.
		// See forwardLocalConsumption for why per-round Events are wrong.
		consumed.addRound(forwardUsage, pinned.Provider, time.Since(roundStart).Milliseconds())
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
			h.recordForwardLocalRequest(ctx, consumed, pinned.Provider, *pinned, route, clientModel, apiKeyID)
			return h.renderForwardLocalFinal(ctx, w, origReq, agg, res.text, policy, requestID, apiKeyID, pinned.Provider.ID)
		}
		if len(external) > 0 {
			msg := fmt.Sprintf("mixed tool uses in one turn: web_search with %v is not supported in forwarded local mode", toolUseNames(external))
			h.sendClaudeError(w, 400, "invalid_request_error", msg)
			return fail(400, msg)
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
			finalStart := time.Now()
			finalRes, finalUsage, ferr := h.forwardRoundBuffered(ctx, pinned.Provider, *pinned, sess, strippedBody, clientModel, route, captureUserText, false)
			if ferr != nil {
				h.sendPublicForwardError(w, true, ferr.Public())
				return fail(ferr.status, ferr.message)
			}
			consumed.addRound(finalUsage, pinned.Provider, time.Since(finalStart).Milliseconds())
			agg.TotalInputTokens += finalRes.inputTokens
			agg.TotalOutputTokens += finalRes.outputTokens
			agg.FinalRound = KiroRoundResult{VisibleContent: finalRes.text, ToolUses: finalRes.toolUses}
			h.recordForwardLocalRequest(ctx, consumed, pinned.Provider, *pinned, route, clientModel, apiKeyID)
			return h.renderForwardLocalFinal(ctx, w, origReq, agg, finalRes.text, policy, requestID, apiKeyID, pinned.Provider.ID)
		}

		distinctBefore := countDistinctQueries(internal, cache)
		toolResults, sources, stats, invocations, execErr := (&kiroConversationRunner{caller: nil, executor: executor}).executeAll(ctx, internal, policy, cache, sourcesByQuery)
		if execErr != nil {
			// Search-side failure: the model rounds already consumed still count, and
			// the request is a failure. Never fails over to another provider.
			h.sendClaudeError(w, 500, "api_error", execErr.Error())
			return fail(500, "web_search: "+execErr.Error())
		}
		agg.SearchCalls += len(internal)
		agg.BackendExecutions += distinctBefore
		agg.CacheHits += len(internal) - distinctBefore
		agg.SearchRounds++
		agg.Sources = append(agg.Sources, sources...)
		agg.TavilyCredits += stats.TavilyCredits
		agg.Searches = append(agg.Searches, invocations...)
		agg.mergeBackendStats(stats)

		working = advanceForwardWorking(working, res, toolResults)
	}

	// Exhausted MaxRounds while still searching: one finalization round with tool stripped.
	stripped := stripForwardWebSearchTools(working)
	strippedBody, _ := json.Marshal(stripped)
	if tm := strings.TrimSpace(pinned.Target.TargetModel); tm != "" {
		strippedBody, _ = rewriteModelField(strippedBody, tm)
	}
	p2 := *pinned
	finalStart := time.Now()
	finalRes, finalUsage, ferr := h.forwardRoundBuffered(ctx, p2.Provider, p2, sess, strippedBody, clientModel, route, captureUserText, false)
	if ferr != nil {
		h.sendPublicForwardError(w, true, ferr.Public())
		return fail(ferr.status, ferr.message)
	}
	consumed.addRound(finalUsage, p2.Provider, time.Since(finalStart).Milliseconds())
	agg.TotalInputTokens += finalRes.inputTokens
	agg.TotalOutputTokens += finalRes.outputTokens
	agg.FinalRound = KiroRoundResult{VisibleContent: finalRes.text, ToolUses: finalRes.toolUses}
	h.recordForwardLocalRequest(ctx, consumed, p2.Provider, p2, route, clientModel, apiKeyID)
	return h.renderForwardLocalFinal(ctx, w, origReq, agg, finalRes.text, policy, requestID, apiKeyID, p2.Provider.ID)
}

// forwardRoundBuffered does one buffered (non-stream) POST to the pinned provider
// and parses content[] for text/tool_use. It records no client response itself;
// the caller owns the ResponseWriter. Returns usage for metrics.
//
// sess is the canonical client session identity resolved once by the caller; it
// is replayed onto every round's request so all continuation rounds of one
// logical request carry the same upstream session affinity.
func (h *Handler) forwardRoundBuffered(ctx context.Context, provider config.UpstreamProvider, rt config.ResolvedTarget, sess clientSession, body []byte, clientModel string, route *config.ModelRoute, captureUserText string, isFirstRound bool) (*forwardLocalRoundResult, usageCounts, *providerForwardError) {
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

	// Force stream:false on every round. This function's whole contract is a
	// buffered JSON round-trip, and it must not depend on the upstream's default:
	// ClaudeRequest.Stream is `omitempty`, so a serialized continuation drops the
	// field entirely, and an upstream that streams by default then answers with SSE
	// that parseClaudeContentForForwardLocal cannot read. Setting it here rather
	// than at each call site means no round can forget.
	body = forceNonStreamBody(body)

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
	// Replayed session identity, but nothing else from the top-level r headers:
	// the per-round body already carries the full Claude request including
	// beta/tool headers if needed, and anthropic-beta etc. are intentionally
	// not replayed here per round to keep the loop hermetic. Session affinity is
	// the one exception — all rounds of this ONE logical request must present
	// the same session to the pinned provider, or the upstream treats each
	// continuation as a new conversation.
	applyClientSessionAffinity(req, sess, provider)

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

	// An upstream that streamed anyway (ignoring stream:false) is a capability
	// mismatch, not malformed JSON. Say so, because "invalid character 'e'" — the
	// 'e' of "event:" — tells the operator nothing about the real cause.
	if isSSEResponse(resp.Header, b) {
		return nil, usageCounts{}, newProviderForwardError(502,
			"upstream streamed a response to a non-stream request; the local web_search strategy needs buffered rounds — configure this provider as native or unspecified")
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

// isSSEResponse reports whether a buffered read got an event stream instead of
// JSON, by content type or by the leading SSE field name.
func isSSEResponse(header http.Header, body []byte) bool {
	if strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/event-stream") {
		return true
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:"))
}

// forceNonStreamBody sets "stream": false on a request body, so a buffered round
// cannot receive SSE. Returns the body unchanged if it is not JSON — the caller
// would fail on it anyway, and a parse error is reported with better context
// downstream.
func forceNonStreamBody(body []byte) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if v, ok := m["stream"].(bool); ok && !v {
		return body // already explicit
	}
	m["stream"] = false
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
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
	return &forwardLocalRoundResult{text: text, toolUses: toolUses, raw: env.Content, stopReason: stop}, nil
}

// advanceForwardWorking appends one assistant+tool_result turn to working (Claude shape).
// advanceForwardWorking appends one provider round-trip to the INTERNAL provider
// transcript, in the provider's ordinary CLIENT-tool contract.
//
// Protocol boundary (do not conflate — see buildForwardLocalContentBlocks for the
// other side): under strategy=local the provider was handed a synthetic ORDINARY
// web_search client tool, so the continuation it receives must be the standard
// Anthropic client-tool shape:
//
//	assistant: [ ...verbatim structured blocks..., tool_use{id:X} ]
//	user:      [ tool_result{tool_use_id:X, content:...} ]
//
// It must NEVER be server_tool_use / web_search_tool_result — those are the
// CLIENT-facing native server-tool representation, and a provider that actually
// executed a native server tool would have been strategy=native instead.
//
// The assistant content is preserved STRUCTURALLY (res.raw, the upstream's own
// content[] verbatim) rather than flattened to text: flattening would drop the
// tool_use block whose id the following tool_result references, leaving a
// dangling tool_use_id the provider would reject. Text concatenation is only
// valid for display/memory, never for canonical conversation state.
func advanceForwardWorking(working ClaudeRequest, res *forwardLocalRoundResult, toolResults []KiroToolResult) ClaudeRequest {
	// Assistant turn: replay the provider's own structured content verbatim when
	// available, so every block (text, thinking, tool_use, and any block type this
	// proxy does not model) survives into the next round exactly as sent.
	var assistantBlocks []interface{}
	if len(res.raw) > 0 {
		assistantBlocks = make([]interface{}, 0, len(res.raw))
		for _, b := range res.raw {
			assistantBlocks = append(assistantBlocks, b)
		}
	} else {
		// Fallback (no raw content captured): rebuild the minimum the protocol
		// needs — text plus the structured tool_use blocks being answered.
		assistantBlocks = make([]interface{}, 0, 1+len(res.toolUses))
		if res.text != "" {
			assistantBlocks = append(assistantBlocks, map[string]interface{}{"type": "text", "text": res.text})
		}
		for _, tu := range res.toolUses {
			assistantBlocks = append(assistantBlocks, map[string]interface{}{
				"type": "tool_use", "id": tu.ToolUseID, "name": tu.Name, "input": tu.Input,
			})
		}
	}
	working.Messages = append(working.Messages, ClaudeMessage{Role: "assistant", Content: assistantBlocks})

	// User turn: one tool_result per executed call, keyed to the tool_use id it
	// answers. All content segments are joined — a search result body may span
	// several Text segments and truncating to the first would feed the model
	// partial evidence.
	userBlocks := make([]interface{}, 0, len(toolResults))
	for _, tr := range toolResults {
		var text string
		for _, c := range tr.Content {
			text += c.Text
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

// syntheticWebSearchTools rewrites native web_search ClaudeTools in a tool list
// into synthetic client-tool form. If no native tools are present, returns the
// original slice unchanged.
func syntheticWebSearchTools(tools []ClaudeTool) []ClaudeTool {
	if len(tools) == 0 {
		return tools
	}
	hasNative := false
	for _, t := range tools {
		if isNativeWebSearchTool(t) {
			hasNative = true
			break
		}
	}
	if !hasNative {
		return tools
	}
	synthetic := ClaudeTool{
		Name:        webSearchToolName,
		Description: "Search the web for up-to-date information.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "The web search query",
				},
			},
			"required": []interface{}{"query"},
		},
	}
	out := make([]ClaudeTool, 0, len(tools))
	for _, t := range tools {
		if isNativeWebSearchTool(t) {
			out = append(out, synthetic)
		} else {
			out = append(out, t)
		}
	}
	return out
}

// renderForwardLocalFinal is the single completion point for a successful local
// loop. It performs the logical-request accounting once and then hands the
// rendering to whichever wire format the CLIENT asked for: a buffered JSON body,
// or the equivalent SSE event sequence when the client sent stream=true.
//
// Streaming is a client-side concern only. The loop upstream is always buffered
// (tool uses must be inspected before anything is committed), so what streams
// here is a completed result replayed as well-formed events — not a live relay.
// Claude Code always streams, so this is the path real traffic takes.
func (h *Handler) renderForwardLocalFinal(ctx context.Context, w http.ResponseWriter, origReq ClaudeRequest, agg KiroRunResult, finalText string, policy WebSearchPolicy, requestID, apiKeyID, providerID string) bool {
	// Logical-request accounting happens exactly ONCE here, at the single point
	// where the whole loop has succeeded — not per provider round. The token
	// figures are the AGGREGATE across every round (provider consumption), while
	// the request counters advance by one (one client request). Calling this per
	// round would inflate totalRequests/successRequests by the round count.
	inTok, outTok := agg.TotalInputTokens, agg.TotalOutputTokens
	usageSource := apikey.UsageSourceUpstream
	if inTok == 0 && outTok == 0 {
		// No upstream usage signal across any round: charge one request unit so
		// per-key quota still advances, and mark it estimated rather than claiming
		// upstream-accurate zeros (same contract as forwardedUsageTokens).
		outTok = 1
		usageSource = apikey.UsageSourceEstimator
	}
	noteAPIKeyUsage(ctx, int64(inTok), int64(outTok), 0, usageSource, usageSource == apikey.UsageSourceEstimator)
	h.recordSuccessForApiKey(ctx, apiKeyID, inTok, outTok, 0)

	// Append deterministic Sources list BEFORE building content so the amended
	// final text (with citations) is what the client sees.
	if config.WebSearchAppendSources() && len(agg.Sources) > 0 {
		finalText = finalText + formatSourcesList(agg.Sources)
	}

	if origReq.Stream {
		h.renderForwardLocalFinalStream(w, origReq, agg, finalText)
	} else {
		h.renderForwardLocalFinalJSON(w, origReq, agg, finalText)
	}
	// Tool observability, once per logical request, with real backend attribution
	// (see recordWebSearchToolUsage). The failure path calls the same helper, and
	// the two terminal points are mutually exclusive.
	recordWebSearchToolUsage(agg, requestID, providerID)
	return true
}

// finishForwardLocalFailure closes out a local loop that died mid-flight: it files
// the ONE failure Event for the request and advances the handler's failure
// counters, then records whatever tool work had already been done.
//
// Consumption incurred before the failure is preserved on the Event (a loop that
// burned two rounds of tokens and then hit a 429 really did spend them), while
// the outcome stays a failure so no fake success is reported. Tool usage is
// emitted here too, from the same single-shot helper the success path uses, so an
// executed search is neither lost nor counted twice — the two terminal points are
// mutually exclusive.
func (h *Handler) finishForwardLocalFailure(ctx context.Context, consumed forwardLocalConsumption, agg KiroRunResult, provider config.UpstreamProvider, rt config.ResolvedTarget, route *config.ModelRoute, clientModel, apiKeyID, requestID string, status int, errMsg string) {
	routeID := ""
	if route != nil {
		routeID = route.ID
	}
	canceled := status == 499
	ev := metrics.Event{
		ClientModel:  clientModel,
		TargetModel:  strings.TrimSpace(rt.Target.TargetModel),
		RouteID:      routeID,
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		Endpoint:     "claude",
		ClientIP:     clientIPFromContext(ctx),
		ApiKeyID:     apiKeyID,
		RequestID:    requestIDFromContext(ctx),
		Status:       status,
		LatencyMs:    consumed.latencyMs,
		InputTokens:  consumed.inputTokens,
		OutputTokens: consumed.outputTokens,
		CostUSD:      consumed.costUSD,
		ModelRounds:  consumed.rounds,
		Stream:       consumed.stream,
		Canceled:     canceled,
		Ok:           false,
		ErrorMsg:     errMsg,
		Usage:        consumed.usage(),
	}
	metrics.Record(ev)
	if !canceled {
		// A client disconnect is not a failed request (same rule as the forward
		// path): it advances neither the success nor the failure counter.
		h.recordFailure()
	}
	recordWebSearchToolUsage(agg, requestID, provider.ID)
}

// recordWebSearchToolUsage emits tool observability for one logical request's
// local web_search work, exactly once, whatever the request's outcome.
//
// Backend attribution is decomposed so metrics.ToolUsage (one Backend per
// observation) can express a request that hit two backends without inflating any
// total: each backend that actually executed gets its own observation carrying
// only its own executions and credits, and the request-level figures (Uses,
// CacheHits) ride on the first one so toolCounter.requests still advances exactly
// once. When nothing executed (every call a cache hit) a single backendless
// observation carries the uses.
func recordWebSearchToolUsage(agg KiroRunResult, requestID, providerID string) {
	if agg.SearchCalls <= 0 {
		return
	}
	base := metrics.ToolUsage{
		TimeMs:     time.Now().UnixMilli(),
		RequestID:  requestID,
		ToolKind:   metrics.ToolKindWebSearch,
		Origin:     metrics.ToolOriginKiroOrchestrator,
		ProviderID: providerID,
	}
	if len(agg.ExecutionsByBackend) == 0 {
		u := base
		u.Uses = int64(agg.SearchCalls)
		u.Executions = int64(agg.BackendExecutions)
		u.CacheHits = int64(agg.CacheHits)
		u.Credits = int64(agg.TavilyCredits)
		metrics.RecordToolUsage(u)
		return
	}
	first := true
	credits := int64(agg.TavilyCredits)
	for _, name := range backendNamesInOrder(agg) {
		u := base
		u.Backend = name
		u.Executions = int64(agg.ExecutionsByBackend[name])
		if first {
			// Request-level figures ride on exactly one observation.
			u.Uses = int64(agg.SearchCalls)
			u.CacheHits = int64(agg.CacheHits)
			u.Credits = credits
			first = false
		}
		metrics.RecordToolUsage(u)
	}
}

// backendNamesInOrder lists the backends that executed, preferring the first-seen
// order the runner recorded so output is deterministic.
func backendNamesInOrder(agg KiroRunResult) []string {
	out := make([]string, 0, len(agg.ExecutionsByBackend))
	for _, name := range agg.Providers {
		if _, ok := agg.ExecutionsByBackend[name]; ok {
			out = append(out, name)
		}
	}
	for name := range agg.ExecutionsByBackend {
		if _, seen := indexOfString(out, name); !seen {
			out = append(out, name)
		}
	}
	return out
}

// forwardLocalUsageMap is the client-visible usage for a completed local loop:
// tokens summed across every round, plus the server-tool counter that makes a
// client display "Did N searches". Shared by both renderers so the JSON body and
// the SSE message_delta cannot disagree.
func forwardLocalUsageMap(agg KiroRunResult) map[string]interface{} {
	usage := map[string]interface{}{
		"input_tokens":  agg.TotalInputTokens,
		"output_tokens": agg.TotalOutputTokens,
	}
	if len(agg.Searches) > 0 {
		usage["server_tool_use"] = map[string]interface{}{"web_search_requests": len(agg.Searches)}
	}
	return usage
}

// renderForwardLocalFinalJSON writes the buffered response body.
func (h *Handler) renderForwardLocalFinalJSON(w http.ResponseWriter, origReq ClaudeRequest, agg KiroRunResult, finalText string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]interface{}{
		"id":          randomMessageID(),
		"type":        "message",
		"role":        "assistant",
		"content":     buildForwardLocalContentBlocks(agg, finalText),
		"model":       origReq.Model,
		"stop_reason": "end_turn",
		"usage":       forwardLocalUsageMap(agg),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warnf("[ForwardLocal] final encode failed: %v", err)
	}
}

// renderForwardLocalFinalStream replays the completed result as an Anthropic SSE
// sequence: message_start, then one content block per element of the same array
// the JSON renderer builds, then message_delta/message_stop.
//
// Block indices are assigned in emission order, and every block is opened and
// closed, because a client tracks them positionally. server_tool_use carries its
// full input in content_block_start (no input_json_delta), matching the pure
// path's synthesized stream; text is chunked so a client renders it
// progressively rather than in one jump.
func (h *Handler) renderForwardLocalFinalStream(w http.ResponseWriter, origReq ClaudeRequest, agg KiroRunResult, finalText string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flusher (a buffering middlebox or a test recorder): fall back to the
		// buffered body rather than emitting an SSE stream nothing will flush.
		h.renderForwardLocalFinalJSON(w, origReq, agg, finalText)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            randomMessageID(),
			"type":          "message",
			"role":          "assistant",
			"model":         origReq.Model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  agg.TotalInputTokens,
				"output_tokens": 0,
			},
		},
	})

	for idx, block := range buildForwardLocalContentBlocks(agg, finalText) {
		if block["type"] == "text" {
			text, _ := block["text"].(string)
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			for _, chunk := range chunkByRunes(text, webSearchSummaryChunkSize) {
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{"type": "text_delta", "text": chunk},
				})
			}
		} else {
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": block,
			})
		}
		h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": idx,
		})
	}

	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": forwardLocalUsageMap(agg),
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
}

// buildForwardLocalContentBlocks renders the CLIENT-facing content array: the
// synthetic Anthropic-native server_tool_use / web_search_tool_result pairs for
// the searches the gateway ran, followed by the final answer text.
//
// This is deliberately a DIFFERENT protocol from the internal provider
// continuation (see advanceForwardWorking): downstream the client sees native
// SERVER-tool blocks so Claude Code renders "Did N searches"; upstream the
// provider only ever sees ordinary CLIENT-tool tool_use/tool_result, because
// under strategy=local the provider is not executing an Anthropic server tool.
//
// Schema comes from the shared buildWebSearchNativeBlockMaps so this path cannot
// drift from the pure/MCP path or the Kiro runner path.
func buildForwardLocalContentBlocks(agg KiroRunResult, finalText string) []map[string]interface{} {
	out := buildWebSearchNativeBlockMaps(agg.Searches)
	out = append(out, map[string]interface{}{"type": "text", "text": finalText})
	return out
}

func randomMessageID() string {
	return "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")
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

// recordForwardLocalRequest files exactly ONE metrics.Event for the whole local
// loop, attributed to the pinned provider/route.
//
// One Event = one request/attempt outcome. metrics.Record increments the request
// counter of every aggregate it touches, so filing an Event per model round would
// make a single client request read as N provider/API requests and put the
// dashboard at odds with the handler's own counters.
//
// Provider consumption is not lost by collapsing to one Event: tokens, cost and
// latency are the sums across rounds, and Event.ModelRounds carries the round
// count as its own dimension (summed separately from requests). A three-round
// loop therefore reports requests=1, modelRounds=3, tokens=sum — which is what
// the operator needs to see.
//
// The logical client counters (totalRequests/successRequests/totalTokens) are
// advanced once more, in renderForwardLocalFinal via recordSuccessForApiKey.
// That is a different store (handler atomics + per-key ledger) and never calls
// metrics.Record, so the two cannot double-count each other.
func (h *Handler) recordForwardLocalRequest(ctx context.Context, consumed forwardLocalConsumption, provider config.UpstreamProvider, rt config.ResolvedTarget, route *config.ModelRoute, clientModel, apiKeyID string) {
	routeID := ""
	if route != nil {
		routeID = route.ID
	}
	metrics.Record(metrics.Event{
		ClientModel:  clientModel,
		TargetModel:  strings.TrimSpace(rt.Target.TargetModel),
		RouteID:      routeID,
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		Endpoint:     "claude",
		ClientIP:     clientIPFromContext(ctx),
		ApiKeyID:     apiKeyID,
		RequestID:    requestIDFromContext(ctx),
		Status:       200,
		LatencyMs:    consumed.latencyMs,
		InputTokens:  consumed.inputTokens,
		OutputTokens: consumed.outputTokens,
		CostUSD:      consumed.costUSD,
		ModelRounds:  consumed.rounds,
		Stream:       consumed.stream,
		Ok:           true,
		Usage:        consumed.usage(),
	})
}
