package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestPromptCacheTrackerComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 120)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	first := tracker.Compute("acct-1", profile)
	if first.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected first request to create cache tokens, got %+v", first)
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to have zero cache reads, got %+v", first)
	}

	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("expected repeated request to read cache tokens, got %+v", second)
	}
	if second.CacheCreationInputTokens != 0 {
		t.Fatalf("expected repeated request to avoid cache creation, got %+v", second)
	}
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, usage, true)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("expected billed input tokens 50, got %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("expected cache creation tokens 30, got %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("expected cache read tokens 20, got %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("expected typed cache creation map, got %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 || creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("unexpected ttl breakdown: %#v", creation)
	}
}

// TestPromptCacheStableAcrossBillingHeaderDrift verifies that Claude Code's
// per-request "x-anthropic-billing-header: cc_version=...; cch=...;" system
// block (whose content drifts on every request) does not break cache hits.
// The tracker should ignore that metadata when fingerprinting cached prefixes.
func TestPromptCacheStableAcrossBillingHeaderDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(billingHdr string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": billingHdr,
				},
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;")
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected no cache read on first request, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := build("x-anthropic-billing-header: cc_version=2.1.87.42; cch=bbbb; padding=xxyyzz;")
	profile2 := tracker.BuildClaudeProfile(req2, 2048)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after billing header drift, got %+v", second)
	}
}

func TestPromptCacheStableWhenBillingHeaderAppearsOrDisappears(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(includeBilling bool) *ClaudeRequest {
		system := []interface{}{}
		if includeBilling {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;",
			})
		}
		system = append(system, map[string]interface{}{
			"type": "text",
			"text": mainSystem,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	withBilling := tracker.BuildClaudeProfile(build(true), 2048)
	if withBilling == nil {
		t.Fatalf("profile with billing header should be built")
	}
	tracker.Update("acct-1", withBilling)

	withoutBilling := tracker.BuildClaudeProfile(build(false), 2048)
	if withoutBilling == nil {
		t.Fatalf("profile without billing header should be built")
	}
	result := tracker.Compute("acct-1", withoutBilling)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read when billing header disappears, got %+v", result)
	}
}

func TestCanonicalCacheValueIgnoresPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 0,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	second := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 1,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	if first != second {
		t.Fatalf("expected position keys to be ignored, got %q vs %q", first, second)
	}
}

func TestCanonicalCacheValuePreservesSemanticPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 1,
		},
	})
	second := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 2,
		},
	})
	if first == second {
		t.Fatalf("expected semantic block_index fields to remain fingerprinted")
	}
}

// TestPromptCacheImplicitBreakpointAtMessageEnd verifies that once any
// explicit cache_control breakpoint has been seen, subsequent message-end
// boundaries act as implicit breakpoints. This allows multi-turn conversations
// to hit earlier stored prefix fingerprints even when the newest messages
// lack explicit cache_control.
func TestPromptCacheImplicitBreakpointAtMessageEnd(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}

	// Round 1: single user message.
	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	// Round 2: conversation continues with new messages. The latest user
	// message has no explicit cache_control; it should still hit the stored
	// prefix via the implicit message-end breakpoint.
	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read via implicit message-end breakpoint, got %+v", result)
	}
}

func TestMinCacheableTokensForModel(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-sonnet-4.5", defaultMinCacheableTokens},
		{"claude-sonnet-4.6", defaultMinCacheableTokens},
		{"claude-sonnet-4", defaultMinCacheableTokens},
		{"claude-opus-4.5", highMinCacheableTokens},
		{"claude-opus-4.8-thinking", highMinCacheableTokens},
		{"claude-haiku-4.5", highMinCacheableTokens},
		{"claude-haiku-4.5-thinking", highMinCacheableTokens},
		{"CLAUDE-HAIKU-4.5", highMinCacheableTokens},
	}
	for _, tc := range cases {
		if got := minCacheableTokensForModel(tc.model); got != tc.want {
			t.Errorf("minCacheableTokensForModel(%q) = %d, want %d", tc.model, got, tc.want)
		}
	}
}

// TestHaikuPrefixBelowThresholdDoesNotHit locks the Haiku 4.5 = 4096 rule at the
// behavior level: a prefix that sits between the Sonnet threshold (1024) and the
// Haiku/Opus threshold (4096) must hit on Sonnet but miss on Haiku, because
// Anthropic does not cache sub-4096 prefixes for Haiku 4.5. Getting this wrong
// over-reports Haiku cache hits.
func TestHaikuPrefixBelowThresholdDoesNotHit(t *testing.T) {
	// Size the system prompt so its cumulative token estimate lands strictly
	// between the two thresholds. Assert the assumption so the test fails loudly
	// (not flakily) if the estimator changes.
	systemText := strings.Repeat("You are a concise assistant. ", 200)
	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}
	newReq := func(model string) *ClaudeRequest {
		return &ClaudeRequest{
			Model:    model,
			System:   baseSystem,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
		}
	}

	probe := newPromptCacheTracker(time.Hour).BuildClaudeProfile(newReq("claude-sonnet-4.5"), 0)
	if probe == nil || len(probe.Breakpoints) == 0 {
		t.Fatalf("probe profile should be built with a breakpoint")
	}
	prefixTokens := probe.Breakpoints[len(probe.Breakpoints)-1].CumulativeTokens
	if prefixTokens < defaultMinCacheableTokens || prefixTokens >= highMinCacheableTokens {
		t.Fatalf("test setup broken: prefix tokens %d must be in [%d, %d)",
			prefixTokens, defaultMinCacheableTokens, highMinCacheableTokens)
	}

	// Sonnet (threshold 1024): the prefix clears the bar, so a repeated request hits.
	sonnet := newPromptCacheTracker(time.Hour)
	profS := sonnet.BuildClaudeProfile(newReq("claude-sonnet-4.5"), prefixTokens)
	sonnet.Update("acct-1", profS)
	if got := sonnet.Compute("acct-1", profS); got.CacheReadInputTokens == 0 {
		t.Fatalf("sonnet: prefix of %d tokens should hit, got %+v", prefixTokens, got)
	}

	// Haiku 4.5 (threshold 4096): the same prefix is below the bar, so it never hits.
	haiku := newPromptCacheTracker(time.Hour)
	profH := haiku.BuildClaudeProfile(newReq("claude-haiku-4.5"), prefixTokens)
	haiku.Update("acct-1", profH)
	if got := haiku.Compute("acct-1", profH); got.CacheReadInputTokens != 0 {
		t.Fatalf("haiku: prefix of %d tokens is below 4096 and must not hit, got %+v", prefixTokens, got)
	}
}
