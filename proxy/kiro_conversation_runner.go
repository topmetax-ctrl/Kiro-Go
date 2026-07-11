package proxy

import (
	"context"
	"errors"
	"sync"

	"kiro-go/config"
	"kiro-go/logger"
)

// ConversationRunner orchestrates one logical Claude request across as many Kiro
// rounds as the model needs, executing web_search server tools in between. It is
// the single orchestration seam shared by the stream and non-stream handlers so
// both have identical semantics.
type ConversationRunner interface {
	Run(ctx context.Context, account *config.Account, original *KiroPayload, policy WebSearchPolicy) (KiroRunResult, error)
}

// KiroRunResult is the aggregate outcome of a whole logical request. FinalRound
// holds the round the client actually sees (its events are replayed on the
// stream path; its content is rendered on the non-stream path). Token/credit
// totals are summed across every round.
type KiroRunResult struct {
	FinalRound        KiroRoundResult
	TotalInputTokens  int
	TotalOutputTokens int
	TotalCredits      float64
	FinalContextPct   float64
	SearchCalls       int
	SearchRounds      int
	Sources           []SearchSource
	// TavilyCredits is the total Tavily credit spend for this request, tracked
	// separately from TotalCredits (which is Kiro credits only) so the two billing
	// domains never conflate. 0 when only the free provider (SearXNG) was used.
	TavilyCredits int
	// Providers records the distinct search providers that actually served a query
	// this request (e.g. ["searxng"], or ["searxng","tavily"] if a fallback fired),
	// for observability. Order is first-seen.
	Providers []string
}

// kiroConversationRunner is the production ConversationRunner.
type kiroConversationRunner struct {
	caller   KiroRoundCaller
	executor ServerToolExecutor
}

// NewKiroConversationRunner wires the production runner: a live round caller and
// a web_search executor backed by the free-first search orchestrator (SearXNG
// primary, Tavily optional fallback). The orchestrator is built from config; if
// no provider is usable it is nil and the executor surfaces a config error rather
// than silently passing an unresolved tool_use back to the client.
func NewKiroConversationRunner() ConversationRunner {
	return &kiroConversationRunner{
		caller:   NewKiroRoundCaller(),
		executor: newWebSearchExecutor(newSearchOrchestratorFromConfig()),
	}
}

// newRunnerWithDeps builds a runner from injected dependencies (tests).
func newRunnerWithDeps(caller KiroRoundCaller, executor ServerToolExecutor) *kiroConversationRunner {
	return &kiroConversationRunner{caller: caller, executor: executor}
}

// Run executes the round/search loop. Invariants:
//   - The input payload is never mutated; all work happens on a clone.
//   - Every round uses the same account (affinity).
//   - Search failures surface as typed errors and never fail the Kiro account.
//   - At most one active structured tool turn ever reaches Kiro per round.
func (r *kiroConversationRunner) Run(ctx context.Context, account *config.Account, original *KiroPayload, policy WebSearchPolicy) (KiroRunResult, error) {
	working := cloneKiroPayload(original)
	agg := KiroRunResult{}

	// Per-request query cache: a repeated normalized query does not burn a second
	// provider credit, but each tool_use still gets its own result.
	cache := map[string]KiroToolResult{}

	for round := 0; round < policy.MaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return agg, err
		}

		result, err := r.caller.CallRound(ctx, account, working)
		if err != nil {
			// Kiro-side error: bubble up so the handler can fail the account and
			// retry from the ORIGINAL payload on a different account.
			return agg, err
		}

		agg.TotalInputTokens += result.InputTokens
		agg.TotalOutputTokens += result.OutputTokens
		agg.TotalCredits += result.Credits
		agg.FinalContextPct = result.ContextUsagePct

		internal, external := partitionToolUses(result.ToolUses, r.executor)

		// No internal web_search calls: this round is terminal. Whatever it holds
		// (final text, or a genuine client tool_use) goes to the client as-is.
		if len(internal) == 0 {
			agg.FinalRound = result
			return agg, nil
		}

		// Mixed internal + external tools in one turn: we cannot execute half and
		// drop the rest without corrupting the turn. Refuse with a typed error.
		if len(external) > 0 {
			return agg, &MixedToolUseError{
				InternalNames: toolUseNames(internal),
				ExternalNames: toolUseNames(external),
			}
		}

		// Enforce the total-search budget across rounds.
		if agg.SearchCalls+len(internal) > policy.MaxSearches {
			logger.Debugf("[WebSearch] search budget reached (calls=%d + %d > max=%d); finalizing",
				agg.SearchCalls, len(internal), policy.MaxSearches)
			return r.finalize(ctx, account, working, result, agg, "max_searches")
		}

		toolResults, sources, credits, execErr := r.executeAll(ctx, internal, policy, cache)
		if execErr != nil {
			// A hard search error (auth/config/context). Do NOT fail the Kiro
			// account; the handler maps this to a search-specific status.
			return agg, execErr
		}
		agg.SearchCalls += len(internal)
		agg.SearchRounds++
		agg.Sources = append(agg.Sources, sources...)
		agg.TavilyCredits += credits

		working = advancePayload(working, result, toolResults)
	}

	// Exhausted MaxRounds while still searching: force one finalization round with
	// web_search removed so the model must answer from what it has.
	last, err := r.caller.CallRound(ctx, account, stripWebSearchTools(working))
	if err != nil {
		return agg, err
	}
	agg.TotalInputTokens += last.InputTokens
	agg.TotalOutputTokens += last.OutputTokens
	agg.TotalCredits += last.Credits
	agg.FinalContextPct = last.ContextUsagePct
	agg.FinalRound = last
	return agg, nil
}

// finalize runs one extra round with web_search stripped, so the model answers
// from the evidence gathered so far instead of looping. reason is logged.
func (r *kiroConversationRunner) finalize(ctx context.Context, account *config.Account, working *KiroPayload, lastSearchRound KiroRoundResult, agg KiroRunResult, reason string) (KiroRunResult, error) {
	// Advance past the round that requested the over-budget search, feeding a
	// budget-exhausted notice as the tool result so the linkage stays valid.
	internal, _ := partitionToolUses(lastSearchRound.ToolUses, r.executor)
	notices := make([]KiroToolResult, 0, len(internal))
	for _, call := range internal {
		notices = append(notices, KiroToolResult{
			ToolUseID: call.ToolUseID,
			Content:   []KiroResultContent{{Text: "WEB_SEARCH_ERROR: search budget exhausted (" + reason + "); answer with the information already gathered."}},
			Status:    "success",
		})
	}
	advanced := advancePayload(working, lastSearchRound, notices)

	final, err := r.caller.CallRound(ctx, account, stripWebSearchTools(advanced))
	if err != nil {
		return agg, err
	}
	agg.TotalInputTokens += final.InputTokens
	agg.TotalOutputTokens += final.OutputTokens
	agg.TotalCredits += final.Credits
	agg.FinalContextPct = final.ContextUsagePct
	agg.FinalRound = final
	return agg, nil
}

// executeAll runs a round's internal search calls and returns their tool_results
// in tool-use order (not completion order), plus the citation sources. Duplicate
// queries within the round — and repeats of a query already run this request —
// resolve from the per-request cache and never hit the provider twice. The unique
// searches run concurrently, bounded by policy.MaxConcurrentSearches, so a round
// that issues several searches does not serialize their latencies.
//
// On the first hard error (auth/config/context) the whole round fails: remaining
// in-flight searches are cancelled and the error is returned so the runner can
// decide retry/finalization. Empty results are NOT an error (the executor frames
// them as a soft, model-readable result).
// searchWork is one scheduled search execution: the tool_use it answers, the
// slot in the round's call list it fills, and the execution outputs written back
// by runSearches.
type searchWork struct {
	index      int
	call       KiroToolUse
	execResult KiroToolResult
	execMeta   ToolExecutionMetadata
}

func (r *kiroConversationRunner) executeAll(ctx context.Context, calls []KiroToolUse, policy WebSearchPolicy, cache map[string]KiroToolResult) ([]KiroToolResult, []SearchSource, int, error) {
	results := make([]KiroToolResult, len(calls))
	metas := make([]ToolExecutionMetadata, len(calls))

	// Collect the distinct queries that actually need a provider call. A query
	// already in the cache (from a prior round) is resolved locally; duplicate
	// queries within this round map to a single execution whose result is fanned
	// back out to every call that shares the query.
	var toRun []searchWork
	queryToRun := map[string]int{} // query key -> index into toRun
	for i, call := range calls {
		key := cacheKey(call)
		if key != "" {
			if _, ok := cache[key]; ok {
				continue // already have it from a prior round
			}
			if _, ok := queryToRun[key]; ok {
				continue // another call in this round already scheduled it
			}
			queryToRun[key] = len(toRun)
		}
		toRun = append(toRun, searchWork{index: i, call: call})
	}

	tavilyCredits := 0
	if len(toRun) > 0 {
		if err := r.runSearches(ctx, toRun, policy); err != nil {
			return nil, nil, 0, err
		}
		// Fold the freshly executed results into the per-request cache and the
		// index slots they were run from. Only fresh executions accrue provider
		// credits; a query resolved from cache spent nothing this round.
		for _, w := range toRun {
			results[w.index] = w.execResult
			metas[w.index] = w.execMeta
			tavilyCredits += w.execMeta.TavilyCredits
			if key := cacheKey(w.call); key != "" {
				cache[key] = w.execResult
			}
		}
	}

	// Assemble every call's result in order, re-keying cached/deduped results to
	// the exact ToolUseID they answer so the continuation linkage stays exact.
	var sources []SearchSource
	for i, call := range calls {
		if results[i].ToolUseID != "" {
			// Filled by a fresh execution above.
			sources = append(sources, metas[i].Sources...)
			continue
		}
		key := cacheKey(call)
		cached, ok := cache[key]
		if !ok || key == "" {
			// Should not happen: every call is either cached or was scheduled.
			return nil, nil, 0, &SearchConfigError{Reason: "internal: search result missing for tool_use"}
		}
		cp := cached
		cp.ToolUseID = call.ToolUseID
		results[i] = cp
	}
	return results, sources, tavilyCredits, nil
}

// runSearches executes the given work items concurrently, bounded by
// policy.MaxConcurrentSearches, writing each result back onto its work item. The
// first hard error cancels the rest and is returned.
func (r *kiroConversationRunner) runSearches(ctx context.Context, items []searchWork, policy WebSearchPolicy) error {
	limit := policy.MaxConcurrentSearches
	if limit <= 0 {
		limit = 1
	}
	if limit > len(items) {
		limit = len(items)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := range items {
		wg.Add(1)
		go func(w *searchWork) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if runCtx.Err() != nil {
				return
			}
			res, meta, err := r.executor.Execute(runCtx, w.call, policy)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel() // stop the rest
				}
				mu.Unlock()
				return
			}
			w.execResult = res
			w.execMeta = meta
		}(&items[i])
	}
	wg.Wait()
	return firstErr
}

// cacheKey is the normalized query for per-request dedup. Empty when no query.
func cacheKey(call KiroToolUse) string {
	return extractSearchQuery(call.Input)
}

// partitionToolUses splits a round's tool uses into those the executor handles
// (internal, e.g. web_search) and those destined for the client (external).
func partitionToolUses(toolUses []KiroToolUse, executor ServerToolExecutor) (internal, external []KiroToolUse) {
	for _, tu := range toolUses {
		if executor.CanHandle(tu) {
			internal = append(internal, tu)
		} else {
			external = append(external, tu)
		}
	}
	return internal, external
}

func toolUseNames(tus []KiroToolUse) []string {
	names := make([]string, 0, len(tus))
	for _, tu := range tus {
		names = append(names, tu.Name)
	}
	return names
}

// classifyRunError maps a runner error to whether the Kiro account should be
// failed. Search/config/mixed/loop errors are NOT account failures; anything
// else (a Kiro round error) is.
func classifyRunError(err error) (accountFailure bool) {
	if err == nil {
		return false
	}
	var cfgErr *SearchConfigError
	var provErr *SearchProviderError
	var mixed *MixedToolUseError
	var loop *LoopLimitError
	switch {
	case errors.As(err, &cfgErr),
		errors.As(err, &provErr),
		errors.As(err, &mixed),
		errors.As(err, &loop),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return false
	default:
		return true
	}
}
