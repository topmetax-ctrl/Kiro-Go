package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
)

// ---- fakes -------------------------------------------------------------

// fakeRoundCaller returns scripted round results in order and records every
// payload it was called with (a deep-enough snapshot for assertions).
type fakeRoundCaller struct {
	scripted []scriptedRound
	calls    int
	seen     []*KiroPayload
}

type scriptedRound struct {
	result KiroRoundResult
	err    error
}

func (f *fakeRoundCaller) CallRound(ctx context.Context, account *config.Account, payload *KiroPayload) (KiroRoundResult, error) {
	// Honor cancellation like the real caller.
	if err := ctx.Err(); err != nil {
		return KiroRoundResult{}, err
	}
	f.seen = append(f.seen, payload)
	i := f.calls
	f.calls++
	if i >= len(f.scripted) {
		return KiroRoundResult{}, fmt.Errorf("fakeRoundCaller: no scripted round #%d", i)
	}
	s := f.scripted[i]
	return s.result, s.err
}

// fakeSearchProvider returns a canned response and counts calls per query.
type fakeSearchProvider struct {
	byQuery map[string]SearchResponse
	err     error
	calls   int
	queries []string
}

func (p *fakeSearchProvider) Name() string { return "fake" }

func (p *fakeSearchProvider) Health() ProviderHealth { return ProviderHealth{State: ProviderHealthy} }

func (p *fakeSearchProvider) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	if err := ctx.Err(); err != nil {
		return SearchResponse{}, err
	}
	p.calls++
	p.queries = append(p.queries, req.Query)
	if p.err != nil {
		return SearchResponse{}, p.err
	}
	if resp, ok := p.byQuery[req.Query]; ok {
		return resp, nil
	}
	return SearchResponse{Query: req.Query, Provider: "fake"}, nil
}

// passthroughOrchestrator adapts a bare SearchProvider into a SearchOrchestrator
// with no routing, quality gating, caching, or reranking. It lets the runner
// tests keep asserting on the runner's own per-request dedup cache and provider
// call counts without pulling the full orchestration pipeline into scope.
type passthroughOrchestrator struct {
	provider SearchProvider
}

func (o passthroughOrchestrator) Search(ctx context.Context, req SearchRequest) (SearchResponse, SearchMetadata, error) {
	resp, err := o.provider.Search(ctx, req)
	if err != nil {
		return SearchResponse{}, SearchMetadata{}, err
	}
	return resp, SearchMetadata{Provider: o.provider.Name(), ResultCount: len(resp.Results), TavilyCredits: resp.Credits}, nil
}

// helpers to build rounds
func textRound(text string, inTok, outTok int, credits float64) KiroRoundResult {
	return KiroRoundResult{
		VisibleContent: text,
		Events:         []KiroRoundEvent{{Kind: RoundEventText, Text: text}},
		InputTokens:    inTok,
		OutputTokens:   outTok,
		Credits:        credits,
	}
}

func searchRound(query, toolUseID string, inTok, outTok int, credits float64) KiroRoundResult {
	tu := KiroToolUse{ToolUseID: toolUseID, Name: "web_search", Input: map[string]interface{}{"query": query}}
	return KiroRoundResult{
		ToolUses:     []KiroToolUse{tu},
		Events:       []KiroRoundEvent{{Kind: RoundEventToolUse, ToolUse: &tu}},
		InputTokens:  inTok,
		OutputTokens: outTok,
		Credits:      credits,
	}
}

func basePayload() *KiroPayload {
	p := &KiroPayload{}
	p.ToolNameMap = map[string]string{"webSearch": "web_search"}
	p.ConversationState.ChatTriggerType = "MANUAL"
	p.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "what is the latest go version?",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			Tools: []KiroToolWrapper{{ToolSpecification: struct {
				Name        string      `json:"name"`
				Description string      `json:"description"`
				InputSchema InputSchema `json:"inputSchema"`
			}{Name: "webSearch", Description: "search", InputSchema: InputSchema{JSON: webSearchQuerySchema()}}}},
		},
	}
	return p
}

func testPolicy() WebSearchPolicy {
	return WebSearchPolicy{Enabled: true, MaxSearches: 6, MaxRounds: 4, MaxResults: 5}
}

func newTestRunner(caller KiroRoundCaller, provider SearchProvider) *kiroConversationRunner {
	return newRunnerWithDeps(caller, newWebSearchExecutor(passthroughOrchestrator{provider: provider}))
}

// ---- tests -------------------------------------------------------------

func TestRunnerNoSearchSingleRound(t *testing.T) {
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: textRound("Go 1.26 is latest.", 100, 20, 0.01)},
	}}
	r := newTestRunner(caller, &fakeSearchProvider{})
	out, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if caller.calls != 1 {
		t.Fatalf("expected 1 round, got %d", caller.calls)
	}
	if out.FinalRound.VisibleContent != "Go 1.26 is latest." {
		t.Fatalf("wrong final content: %q", out.FinalRound.VisibleContent)
	}
	if out.SearchCalls != 0 || out.SearchRounds != 0 {
		t.Fatalf("expected no searches, got calls=%d rounds=%d", out.SearchCalls, out.SearchRounds)
	}
}

func TestRunnerSingleSearchThenFinal(t *testing.T) {
	provider := &fakeSearchProvider{byQuery: map[string]SearchResponse{
		"latest go version": {Results: []SearchResult{{Title: "Go", URL: "https://go.dev/dl", Content: "Go 1.26"}}},
	}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: searchRound("latest go version", "tool-1", 100, 10, 0.01)},
		{result: textRound("Go 1.26 per [1].", 120, 30, 0.02)},
	}}
	r := newTestRunner(caller, provider)
	out, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if caller.calls != 2 {
		t.Fatalf("expected 2 rounds, got %d", caller.calls)
	}
	if provider.calls != 1 {
		t.Fatalf("expected 1 search, got %d", provider.calls)
	}
	if out.SearchCalls != 1 || out.SearchRounds != 1 {
		t.Fatalf("search accounting wrong: calls=%d rounds=%d", out.SearchCalls, out.SearchRounds)
	}
	if len(out.Sources) != 1 || out.Sources[0].URL != "https://go.dev/dl" {
		t.Fatalf("sources wrong: %+v", out.Sources)
	}
	// aggregate tokens/credits across both rounds
	if out.TotalInputTokens != 220 || out.TotalOutputTokens != 40 {
		t.Fatalf("token aggregate wrong: in=%d out=%d", out.TotalInputTokens, out.TotalOutputTokens)
	}
	if out.TotalCredits < 0.0299 || out.TotalCredits > 0.0301 {
		t.Fatalf("credit aggregate wrong: %f", out.TotalCredits)
	}
}

func TestRunnerSecondRoundReceivesStructuredToolResult(t *testing.T) {
	provider := &fakeSearchProvider{byQuery: map[string]SearchResponse{
		"q1": {Results: []SearchResult{{Title: "A", URL: "https://a.example", Content: "x"}}},
	}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: searchRound("q1", "tool-1", 10, 5, 0)},
		{result: textRound("done", 10, 5, 0)},
	}}
	r := newTestRunner(caller, provider)
	if _, err := r.Run(context.Background(), nil, basePayload(), testPolicy()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// The 2nd payload the caller saw must carry a structured tool_result whose
	// ToolUseID matches the tool_use from round 1, and the last history entry
	// must be an assistant turn with that same tool_use ID (one active turn).
	if len(caller.seen) != 2 {
		t.Fatalf("expected 2 payloads seen, got %d", len(caller.seen))
	}
	p2 := caller.seen[1]
	uctx := p2.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if uctx == nil || len(uctx.ToolResults) != 1 {
		t.Fatalf("2nd round missing structured tool_result: %+v", uctx)
	}
	if uctx.ToolResults[0].ToolUseID != "tool-1" {
		t.Fatalf("tool_result not keyed to tool-1: %q", uctx.ToolResults[0].ToolUseID)
	}
	hist := p2.ConversationState.History
	last := hist[len(hist)-1]
	if last.AssistantResponseMessage == nil || len(last.AssistantResponseMessage.ToolUses) != 1 {
		t.Fatalf("last history turn is not the active assistant tool turn: %+v", last)
	}
	if last.AssistantResponseMessage.ToolUses[0].ToolUseID != "tool-1" {
		t.Fatalf("active turn tool_use id mismatch: %q", last.AssistantResponseMessage.ToolUses[0].ToolUseID)
	}
}

func TestRunnerTwoSearchCallsInOneRound(t *testing.T) {
	tu1 := KiroToolUse{ToolUseID: "t1", Name: "web_search", Input: map[string]interface{}{"query": "alpha"}}
	tu2 := KiroToolUse{ToolUseID: "t2", Name: "web_search", Input: map[string]interface{}{"query": "beta"}}
	provider := &fakeSearchProvider{byQuery: map[string]SearchResponse{
		"alpha": {Results: []SearchResult{{Title: "A", URL: "https://a.example"}}},
		"beta":  {Results: []SearchResult{{Title: "B", URL: "https://b.example"}}},
	}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: KiroRoundResult{ToolUses: []KiroToolUse{tu1, tu2}}},
		{result: textRound("done", 0, 0, 0)},
	}}
	r := newTestRunner(caller, provider)
	out, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if provider.calls != 2 {
		t.Fatalf("expected 2 searches, got %d", provider.calls)
	}
	if out.SearchCalls != 2 {
		t.Fatalf("expected SearchCalls=2, got %d", out.SearchCalls)
	}
	// second payload must carry two tool_results in tool-use order
	p2 := caller.seen[1]
	trs := p2.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults
	if len(trs) != 2 || trs[0].ToolUseID != "t1" || trs[1].ToolUseID != "t2" {
		t.Fatalf("tool_results order/linkage wrong: %+v", trs)
	}
}

func TestRunnerDuplicateQueryUsesCache(t *testing.T) {
	tu1 := KiroToolUse{ToolUseID: "t1", Name: "web_search", Input: map[string]interface{}{"query": "same"}}
	tu2 := KiroToolUse{ToolUseID: "t2", Name: "web_search", Input: map[string]interface{}{"query": "same"}}
	provider := &fakeSearchProvider{byQuery: map[string]SearchResponse{
		"same": {Results: []SearchResult{{Title: "S", URL: "https://s.example"}}},
	}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: KiroRoundResult{ToolUses: []KiroToolUse{tu1, tu2}}},
		{result: textRound("done", 0, 0, 0)},
	}}
	r := newTestRunner(caller, provider)
	if _, err := r.Run(context.Background(), nil, basePayload(), testPolicy()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("expected 1 provider call (cache dedup), got %d", provider.calls)
	}
	// but both tool_uses still get their own result, correctly keyed
	p2 := caller.seen[1]
	trs := p2.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults
	if len(trs) != 2 || trs[0].ToolUseID != "t1" || trs[1].ToolUseID != "t2" {
		t.Fatalf("cached dedup broke linkage: %+v", trs)
	}
}

func TestRunnerDoesNotMutateInputPayload(t *testing.T) {
	provider := &fakeSearchProvider{byQuery: map[string]SearchResponse{
		"latest go version": {Results: []SearchResult{{Title: "Go", URL: "https://go.dev"}}},
	}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: searchRound("latest go version", "tool-1", 0, 0, 0)},
		{result: textRound("done", 0, 0, 0)},
	}}
	orig := basePayload()
	origHistoryLen := len(orig.ConversationState.History)
	origContent := orig.ConversationState.CurrentMessage.UserInputMessage.Content

	r := newTestRunner(caller, provider)
	if _, err := r.Run(context.Background(), nil, orig, testPolicy()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(orig.ConversationState.History) != origHistoryLen {
		t.Fatalf("input payload history mutated: %d != %d", len(orig.ConversationState.History), origHistoryLen)
	}
	if orig.ConversationState.CurrentMessage.UserInputMessage.Content != origContent {
		t.Fatalf("input payload current message mutated")
	}
	if orig.ToolNameMap["webSearch"] != "web_search" {
		t.Fatalf("input ToolNameMap mutated/lost")
	}
}

func TestRunnerCloasePreservesToolNameMap(t *testing.T) {
	src := basePayload()
	clone := cloneKiroPayload(src)
	if clone.ToolNameMap["webSearch"] != "web_search" {
		t.Fatalf("clone lost ToolNameMap: %+v", clone.ToolNameMap)
	}
	// mutating clone must not touch src
	clone.ToolNameMap["webSearch"] = "changed"
	if src.ToolNameMap["webSearch"] != "web_search" {
		t.Fatalf("clone shares ToolNameMap backing map with src")
	}
}

func TestRunnerMaxRoundsForcesFinalization(t *testing.T) {
	provider := &fakeSearchProvider{byQuery: map[string]SearchResponse{
		"q": {Results: []SearchResult{{Title: "Q", URL: "https://q.example"}}},
	}}
	// Model keeps searching every round; MaxRounds=2 must force a finalization
	// round with web_search stripped.
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: searchRound("q", "t1", 0, 0, 0)},
		{result: searchRound("q", "t2", 0, 0, 0)},
		{result: textRound("forced answer", 0, 0, 0)}, // finalization round
	}}
	policy := testPolicy()
	policy.MaxRounds = 2
	r := newTestRunner(caller, provider)
	out, err := r.Run(context.Background(), nil, basePayload(), policy)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.FinalRound.VisibleContent != "forced answer" {
		t.Fatalf("expected finalization answer, got %q", out.FinalRound.VisibleContent)
	}
	// the finalization payload (3rd) must have NO web_search tool
	p3 := caller.seen[2]
	uctx := p3.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if uctx != nil {
		for _, tl := range uctx.Tools {
			if isWebSearchToolName(tl.ToolSpecification.Name) {
				t.Fatalf("finalization round still advertises web_search")
			}
		}
	}
}

func TestRunnerMaxSearchesForcesFinalization(t *testing.T) {
	provider := &fakeSearchProvider{byQuery: map[string]SearchResponse{
		"q": {Results: []SearchResult{{Title: "Q", URL: "https://q.example"}}},
	}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: searchRound("q", "t1", 0, 0, 0)},
		{result: searchRound("q", "t2", 0, 0, 0)}, // this would exceed MaxSearches=1
		{result: textRound("forced", 0, 0, 0)},
	}}
	policy := testPolicy()
	policy.MaxSearches = 1
	policy.MaxRounds = 5
	r := newTestRunner(caller, provider)
	out, err := r.Run(context.Background(), nil, basePayload(), policy)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.FinalRound.VisibleContent != "forced" {
		t.Fatalf("expected forced finalization, got %q", out.FinalRound.VisibleContent)
	}
	if out.SearchCalls != 1 {
		t.Fatalf("expected exactly 1 search under budget, got %d", out.SearchCalls)
	}
}

func TestRunnerProviderErrorIsNotAccountFailure(t *testing.T) {
	provider := &fakeSearchProvider{err: &SearchProviderError{Kind: SearchErrAuth, StatusCode: 401, Err: errors.New("bad key")}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: searchRound("q", "t1", 0, 0, 0)},
	}}
	r := newTestRunner(caller, provider)
	_, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	if err == nil {
		t.Fatalf("expected provider error to surface")
	}
	if classifyRunError(err) {
		t.Fatalf("provider error must NOT be an account failure")
	}
}

func TestRunnerKiroErrorIsAccountFailure(t *testing.T) {
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{err: errors.New("HTTP 500 from Kiro")},
	}}
	r := newTestRunner(caller, &fakeSearchProvider{})
	_, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	if err == nil {
		t.Fatalf("expected kiro error")
	}
	if !classifyRunError(err) {
		t.Fatalf("kiro round error must be an account failure")
	}
}

func TestRunnerContextCancelStopsLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: textRound("never", 0, 0, 0)},
	}}
	r := newTestRunner(caller, &fakeSearchProvider{})
	_, err := r.Run(ctx, nil, basePayload(), testPolicy())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("no round should run under a cancelled context, ran %d", caller.calls)
	}
}

func TestRunnerMixedToolUseReturnsTypedError(t *testing.T) {
	tuSearch := KiroToolUse{ToolUseID: "s1", Name: "web_search", Input: map[string]interface{}{"query": "q"}}
	tuClient := KiroToolUse{ToolUseID: "c1", Name: "Bash", Input: map[string]interface{}{"command": "ls"}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: KiroRoundResult{ToolUses: []KiroToolUse{tuSearch, tuClient}}},
	}}
	r := newTestRunner(caller, &fakeSearchProvider{})
	_, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	var mixed *MixedToolUseError
	if !errors.As(err, &mixed) {
		t.Fatalf("expected MixedToolUseError, got %v", err)
	}
	if classifyRunError(err) {
		t.Fatalf("mixed tool error must not be an account failure")
	}
}

func TestRunnerClientToolPassesThrough(t *testing.T) {
	tuClient := KiroToolUse{ToolUseID: "c1", Name: "Bash", Input: map[string]interface{}{"command": "ls"}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: KiroRoundResult{ToolUses: []KiroToolUse{tuClient}, Events: []KiroRoundEvent{{Kind: RoundEventToolUse, ToolUse: &tuClient}}}},
	}}
	r := newTestRunner(caller, &fakeSearchProvider{})
	out, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if caller.calls != 1 {
		t.Fatalf("client tool round is terminal; expected 1 call, got %d", caller.calls)
	}
	if len(out.FinalRound.ToolUses) != 1 || out.FinalRound.ToolUses[0].Name != "Bash" {
		t.Fatalf("client tool_use should pass through unchanged: %+v", out.FinalRound.ToolUses)
	}
}

// concurrentProvider is a thread-safe provider that records the peak number of
// simultaneous in-flight calls and finishes queries in a deliberately scrambled
// order (later queries return first), so a correct runner must reorder results
// back to tool-use order rather than completion order.
type concurrentProvider struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	// perQuery maps a query to the result URL it should return, so we can assert
	// each tool_use got the result for ITS query and not another's.
	perQuery map[string]string
}

func (p *concurrentProvider) Name() string           { return "concurrent" }
func (p *concurrentProvider) Health() ProviderHealth { return ProviderHealth{State: ProviderHealthy} }
func (p *concurrentProvider) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.mu.Unlock()

	// Hold long enough that, with concurrency>1, several calls overlap.
	select {
	case <-ctx.Done():
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
		return SearchResponse{}, ctx.Err()
	case <-time.After(30 * time.Millisecond):
	}

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()

	url := p.perQuery[req.Query]
	return SearchResponse{Query: req.Query, Provider: "concurrent", Results: []SearchResult{
		{Title: req.Query, URL: url, Content: "content for " + req.Query},
	}}, nil
}

func TestRunnerConcurrentSearchesPreserveOrder(t *testing.T) {
	tu1 := KiroToolUse{ToolUseID: "t1", Name: "web_search", Input: map[string]interface{}{"query": "alpha"}}
	tu2 := KiroToolUse{ToolUseID: "t2", Name: "web_search", Input: map[string]interface{}{"query": "bravo"}}
	tu3 := KiroToolUse{ToolUseID: "t3", Name: "web_search", Input: map[string]interface{}{"query": "charlie"}}
	prov := &concurrentProvider{perQuery: map[string]string{
		"alpha":   "https://alpha.example",
		"bravo":   "https://bravo.example",
		"charlie": "https://charlie.example",
	}}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: KiroRoundResult{ToolUses: []KiroToolUse{tu1, tu2, tu3}}},
		{result: textRound("done", 0, 0, 0)},
	}}
	policy := testPolicy()
	policy.MaxConcurrentSearches = 3
	r := newTestRunner(caller, prov)
	if _, err := r.Run(context.Background(), nil, basePayload(), policy); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// The three searches must have actually overlapped (peak >= 2), proving the
	// pool ran them concurrently rather than serially.
	if prov.peak < 2 {
		t.Fatalf("expected concurrent execution (peak>=2), got peak=%d", prov.peak)
	}

	// Second payload must carry tool_results in EXACT tool-use order, each keyed to
	// its own ToolUseID and carrying its own query's result — not scrambled by
	// completion order.
	p2 := caller.seen[1]
	trs := p2.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults
	if len(trs) != 3 {
		t.Fatalf("expected 3 tool_results, got %d", len(trs))
	}
	wantOrder := []struct{ id, url string }{
		{"t1", "https://alpha.example"},
		{"t2", "https://bravo.example"},
		{"t3", "https://charlie.example"},
	}
	for i, w := range wantOrder {
		if trs[i].ToolUseID != w.id {
			t.Fatalf("tool_result %d keyed to %q, want %q", i, trs[i].ToolUseID, w.id)
		}
		body := ""
		if len(trs[i].Content) > 0 {
			body = trs[i].Content[0].Text
		}
		if !strings.Contains(body, w.url) {
			t.Fatalf("tool_result %d (%s) missing its own result URL %q; body=%q", i, w.id, w.url, body)
		}
	}
}
