package proxy

import (
	"context"
	"testing"

	"kiro-go/search"
)

// TestRunnerDedupCountsExecutions proves the Uses vs Executions vs CacheHits
// split the tool metrics rely on: two web_search calls with the same query in
// one round run ONE backend execution.
func TestRunnerDedupCountsExecutions(t *testing.T) {
	provider := &fakeSearchProvider{byQuery: map[string]search.Response{
		"go latest": {Results: []search.Result{{Title: "Go", URL: "https://go.dev/dl", Content: "Go 1.26"}}},
	}}
	// Model issues two tool_uses with the same query in one round.
	round := KiroRoundResult{
		ToolUses: []KiroToolUse{
			{ToolUseID: "tool-1", Name: "web_search", Input: map[string]interface{}{"query": "go latest"}},
			{ToolUseID: "tool-2", Name: "web_search", Input: map[string]interface{}{"query": "go latest"}},
		},
		InputTokens: 100, OutputTokens: 10, Credits: 0.01,
	}
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: round},
		{result: textRound("Go 1.26.", 130, 20, 0.01)},
	}}
	r := newTestRunner(caller, provider)
	out, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.SearchCalls != 2 {
		t.Fatalf("SearchCalls = %d, want 2 (one per tool_use)", out.SearchCalls)
	}
	if out.BackendExecutions != 1 {
		t.Fatalf("BackendExecutions = %d, want 1 (one provider call for deduped query)", out.BackendExecutions)
	}
	if out.CacheHits != 1 {
		t.Fatalf("CacheHits = %d, want 1 (second call served from per-request cache)", out.CacheHits)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
}

// TestRunnerNoSearchesLeavesCountsZero guards the invariant that a request
// without web_search emits no tool usage.
func TestRunnerNoSearchesLeavesCountsZero(t *testing.T) {
	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: textRound("plain answer", 100, 20, 0.01)},
	}}
	r := newTestRunner(caller, &fakeSearchProvider{})
	out, err := r.Run(context.Background(), nil, basePayload(), testPolicy())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.SearchCalls != 0 || out.BackendExecutions != 0 || out.CacheHits != 0 {
		t.Fatalf("zero-search request reported calls=%d execs=%d cache=%d",
			out.SearchCalls, out.BackendExecutions, out.CacheHits)
	}
}
