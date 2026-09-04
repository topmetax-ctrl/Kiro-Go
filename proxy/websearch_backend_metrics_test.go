package proxy

import (
	"testing"

	"kiro-go/metrics"
)

// Tool metrics must answer "did a real search backend run?", not merely "was the
// tool used?". These pin the decomposition: per-backend executions, and
// request-level figures (uses/cacheHits) counted exactly once even when a request
// hit two backends.

func TestRecordWebSearchToolUsageAttributesBackend(t *testing.T) {
	metrics.ResetToolStats()
	agg := KiroRunResult{
		SearchCalls:         3,
		BackendExecutions:   2,
		CacheHits:           1,
		ExecutionsByBackend: map[string]int{"searxng": 2},
		Providers:           []string{"searxng"},
	}
	recordWebSearchToolUsage(agg, "req-1", "prov-x")

	st := metrics.ToolStatsFor(metrics.ToolKindWebSearch)
	if st.Uses != 3 {
		t.Errorf("uses = %d, want 3", st.Uses)
	}
	if st.Executions != 2 {
		t.Errorf("executions = %d, want 2", st.Executions)
	}
	if st.CacheHits != 1 {
		t.Errorf("cacheHits = %d, want 1", st.CacheHits)
	}
	if st.ByBackend["searxng"] != 2 {
		t.Errorf("byBackend[searxng] = %d, want 2 (the acceptance signal that SearXNG ran)", st.ByBackend["searxng"])
	}
	if st.Requests != 1 {
		t.Errorf("requests = %d, want 1 (one logical request used the tool)", st.Requests)
	}
}

// Two backends in one request (a fallback fired): each gets its own executions,
// but uses/cacheHits/requests must not be doubled.
func TestRecordWebSearchToolUsageTwoBackendsCountsUsesOnce(t *testing.T) {
	metrics.ResetToolStats()
	agg := KiroRunResult{
		SearchCalls:         2,
		BackendExecutions:   2,
		ExecutionsByBackend: map[string]int{"searxng": 1, "tavily": 1},
		Providers:           []string{"searxng", "tavily"},
		TavilyCredits:       1,
	}
	recordWebSearchToolUsage(agg, "req-2", "prov-x")

	st := metrics.ToolStatsFor(metrics.ToolKindWebSearch)
	if st.Uses != 2 {
		t.Errorf("uses = %d, want 2 (not doubled per backend)", st.Uses)
	}
	if st.Executions != 2 {
		t.Errorf("executions = %d, want 2", st.Executions)
	}
	if st.ByBackend["searxng"] != 1 || st.ByBackend["tavily"] != 1 {
		t.Errorf("byBackend = %v, want searxng=1 tavily=1", st.ByBackend)
	}
	if st.Credits != 1 {
		t.Errorf("credits = %d, want 1 (Tavily spend counted once)", st.Credits)
	}
}

// A request served entirely from the dedup/cache executed no backend: uses are
// still reported, and no backend is credited with an execution it did not run.
func TestRecordWebSearchToolUsageAllCacheHitsCreditsNoBackend(t *testing.T) {
	metrics.ResetToolStats()
	agg := KiroRunResult{SearchCalls: 2, BackendExecutions: 0, CacheHits: 2}
	recordWebSearchToolUsage(agg, "req-3", "prov-x")

	st := metrics.ToolStatsFor(metrics.ToolKindWebSearch)
	if st.Uses != 2 || st.CacheHits != 2 {
		t.Errorf("uses/cacheHits = %d/%d, want 2/2", st.Uses, st.CacheHits)
	}
	if st.Executions != 0 {
		t.Errorf("executions = %d, want 0", st.Executions)
	}
	if len(st.ByBackend) != 0 {
		t.Errorf("byBackend = %v, want empty (nothing executed)", st.ByBackend)
	}
}

// TestSearchRoundStatsRecordsOnlyFreshExecutions pins the runner-side source of
// the backend figures: dedup/cache hits never reach addExecution, so ByBackend
// only ever counts real provider calls.
func TestSearchRoundStatsRecordsOnlyFreshExecutions(t *testing.T) {
	var st searchRoundStats
	st.addExecution("searxng", 0)
	st.addExecution("searxng", 0)
	st.addExecution("tavily", 2)

	if st.ByBackend["searxng"] != 2 {
		t.Errorf("searxng = %d, want 2", st.ByBackend["searxng"])
	}
	if st.ByBackend["tavily"] != 1 {
		t.Errorf("tavily = %d, want 1", st.ByBackend["tavily"])
	}
	if st.TavilyCredits != 2 {
		t.Errorf("credits = %d, want 2", st.TavilyCredits)
	}
	if len(st.Providers) != 2 || st.Providers[0] != "searxng" || st.Providers[1] != "tavily" {
		t.Errorf("providers = %v, want [searxng tavily] in first-seen order", st.Providers)
	}
}

// mergeBackendStats must accumulate across rounds rather than overwrite, so a
// request whose rounds hit different backends reports both.
func TestMergeBackendStatsAccumulatesAcrossRounds(t *testing.T) {
	var agg KiroRunResult
	var r1, r2 searchRoundStats
	r1.addExecution("searxng", 0)
	r2.addExecution("searxng", 0)
	r2.addExecution("tavily", 3)
	agg.mergeBackendStats(r1)
	agg.mergeBackendStats(r2)

	if agg.ExecutionsByBackend["searxng"] != 2 {
		t.Errorf("searxng = %d, want 2 across rounds", agg.ExecutionsByBackend["searxng"])
	}
	if agg.ExecutionsByBackend["tavily"] != 1 {
		t.Errorf("tavily = %d, want 1", agg.ExecutionsByBackend["tavily"])
	}
	if len(agg.Providers) != 2 {
		t.Errorf("providers = %v, want 2 distinct", agg.Providers)
	}
}
