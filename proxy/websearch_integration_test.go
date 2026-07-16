package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/search"
)

// TestLiveRunnerFeedsRealSearchIntoContinuation is the closest end-to-end proof
// available without a live Kiro account. It drives the real ConversationRunner
// with:
//   - a scripted KiroRoundCaller that emits a genuine web_search tool_use on
//     round 1 (as the Kiro backend does) and a final text answer on round 2, and
//   - the REAL web_search executor backed by the REAL free-first orchestrator
//     (built through the public search.NewOrchestratorFromConfig seam) hitting
//     the live SearXNG instance.
//
// It asserts that the runner ran an actual internet search and fed a structured
// tool_result — built from live go.dev content — back into round 2's payload,
// linked to the exact tool_use ID, with the untrusted-data framing. This is the
// whole server-tool loop minus the Kiro HTTP hop.
//
//	SEARXNG_BASE_URL=http://localhost:18899 KIRO_WEBSEARCH_INTEGRATION=1 \
//	  go test ./proxy -run TestLiveRunnerFeedsRealSearchIntoContinuation -v
//
// The pure-search pipeline live tests (SearXNG/Tavily provider calls, the
// router→quality→rerank→cache pipeline) live in the search package's
// live_integration_test.go now that that logic is a standalone package.
func TestLiveRunnerFeedsRealSearchIntoContinuation(t *testing.T) {
	if os.Getenv("KIRO_WEBSEARCH_INTEGRATION") != "1" {
		t.Skip("set KIRO_WEBSEARCH_INTEGRATION=1 to run live web-search integration tests")
	}
	base := strings.TrimSpace(os.Getenv("SEARXNG_BASE_URL"))
	if base == "" {
		t.Skip("set SEARXNG_BASE_URL to run live SearXNG integration test")
	}

	// Point the config-driven orchestrator at the live SearXNG instance: free-first
	// means SearXNG alone (no API key) satisfies the provider pool.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	cfgJSON, _ := json.Marshal(map[string]interface{}{
		"webSearch": map[string]interface{}{
			"enabled": true,
			"searxng": map[string]interface{}{
				"baseUrl": base,
			},
		},
	})
	if err := os.WriteFile(cfgFile, cfgJSON, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	orch := search.NewOrchestratorFromConfig(nil)
	if orch == nil {
		t.Fatalf("expected a configured orchestrator (searxng base %q)", base)
	}
	executor := newWebSearchExecutor(orch)

	caller := &fakeRoundCaller{scripted: []scriptedRound{
		{result: searchRound("latest stable Go release version", "tool-live-1", 100, 10, 0.01)},
		{result: textRound("The latest stable Go release is documented on go.dev [1].", 130, 25, 0.02)},
	}}
	runner := newRunnerWithDeps(caller, executor)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	out, err := runner.Run(ctx, nil, basePayload(), testPolicy())
	if err != nil {
		t.Fatalf("runner failed: %v", err)
	}

	// The runner must have performed exactly one real search round and two Kiro
	// rounds (search + finalize), and surfaced live sources.
	if out.SearchRounds != 1 || out.SearchCalls != 1 {
		t.Fatalf("expected 1 search round/call, got rounds=%d calls=%d", out.SearchRounds, out.SearchCalls)
	}
	if caller.calls != 2 {
		t.Fatalf("expected 2 Kiro rounds, got %d", caller.calls)
	}
	if len(out.Sources) == 0 {
		t.Fatalf("expected live sources from the search")
	}

	// Round 2's payload must carry the structured tool_result built from the live
	// search, keyed to the round-1 tool_use ID, with the untrusted-data framing.
	if len(caller.seen) != 2 {
		t.Fatalf("expected 2 payloads seen, got %d", len(caller.seen))
	}
	p2 := caller.seen[1]
	uctx := p2.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if uctx == nil || len(uctx.ToolResults) != 1 {
		t.Fatalf("round 2 missing structured tool_result: %+v", uctx)
	}
	tr := uctx.ToolResults[0]
	if tr.ToolUseID != "tool-live-1" {
		t.Fatalf("tool_result not keyed to the live tool_use: %q", tr.ToolUseID)
	}
	body := ""
	if len(tr.Content) > 0 {
		body = tr.Content[0].Text
	}
	if !strings.Contains(body, "WEB_SEARCH_RESULTS") || !strings.Contains(body, "untrusted external web content") {
		t.Fatalf("tool_result missing untrusted-data framing: %q", body)
	}
	if !strings.Contains(body, "http") {
		t.Fatalf("tool_result carries no source URL from the live search: %q", body)
	}
	t.Logf("live runner E2E ok: %d sources, round-2 tool_result %d bytes keyed to %s",
		len(out.Sources), len(body), tr.ToolUseID)
}
