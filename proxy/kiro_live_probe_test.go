package proxy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

// ============================================================================
// LIVE upstream probe. Skipped unless KIRO_LIVE_PROBE=1 and KIRO_LIVE_KEY is set.
//
// These tests spend real quota on a real Kiro account, so they never run as part
// of the normal suite. They exist to answer one question with evidence rather
// than inference: does the upstream accept structured tool pairs in history?
//
// kiro_history_repair.go argues from the Kiro IDE's own bundled validator that it
// does — and that the HTTP 400 "Improperly formed request" reported in commit
// 72da572 came from broken pairing, not from the presence of structured tool
// turns. That is a strong argument but still an argument. This probe checks it
// against the actual service.
//
// Run:
//
//	KIRO_LIVE_PROBE=1 KIRO_LIVE_KEY=ksk_... go test ./proxy -run TestLive -v
// ============================================================================

func liveProbeAccount(t *testing.T) *config.Account {
	t.Helper()
	if os.Getenv("KIRO_LIVE_PROBE") != "1" {
		t.Skip("live probe disabled (set KIRO_LIVE_PROBE=1 to spend real quota)")
	}
	key := strings.TrimSpace(os.Getenv("KIRO_LIVE_KEY"))
	if key == "" {
		t.Skip("KIRO_LIVE_KEY not set")
	}
	region := strings.TrimSpace(os.Getenv("KIRO_LIVE_REGION"))
	if region == "" {
		region = "us-east-1"
	}
	acct := &config.Account{
		ID:         "live-probe",
		KiroApiKey: key,
		Region:     region,
	}
	if err := config.NormalizeAPIKeyAccount(acct); err != nil {
		t.Fatalf("normalize api key account: %v", err)
	}
	return acct
}

// liveResult captures what the upstream did with one payload.
type liveResult struct {
	Text       string
	ToolUses   []KiroToolUse
	StopReason string
	Err        error
}

func (r liveResult) String() string {
	if r.Err != nil {
		return "ERROR: " + r.Err.Error()
	}
	names := make([]string, 0, len(r.ToolUses))
	for _, tu := range r.ToolUses {
		names = append(names, tu.Name)
	}
	text := strings.ReplaceAll(r.Text, "\n", " ")
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return fmt.Sprintf("stop=%s tools=%v text=%q", r.StopReason, names, text)
}

// runLive sends one payload and collects the response.
func runLive(t *testing.T, acct *config.Account, payload *KiroPayload) liveResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var res liveResult
	cb := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if !isThinking {
				res.Text += text
			}
		},
		OnToolUse:    func(tu KiroToolUse) { res.ToolUses = append(res.ToolUses, tu) },
		OnStopReason: func(reason string) { res.StopReason = reason },
		OnError:      func(err error) { res.Err = err },
	}
	if err := CallKiroAPIContext(ctx, acct, payload, cb); err != nil {
		res.Err = err
	}
	return res
}

// infraFailure classifies an error as "nothing to do with payload shape" —
// authentication, quota, or transport. These must never be read as evidence about
// the conversation rules: a 403 for a dead key looks like a rejection but says
// nothing about what upstream thinks of the payload.
func infraFailure(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, probe := range []struct{ needle, label string }{
		// Suspension must be probed before the generic auth needles: AWS reports it
		// as a 403 AccessDeniedException, so a plain "auth" label would send the
		// reader looking for a bad key when the account is actually locked and only
		// AWS support can restore it.
		{"TEMPORARILY_SUSPENDED", "account temporarily suspended by AWS (contact support)"},
		{"temporarily suspended", "account temporarily suspended by AWS (contact support)"},
		{"unusual user activity", "account locked for unusual activity"},
		{"suspicious activity", "anti-abuse throttle"},
		{"bearer token", "auth (invalid/expired bearer token)"},
		{"AccessDeniedException", "auth (access denied)"},
		{"403", "auth (forbidden)"},
		{"401", "auth (unauthorized)"},
		{"quota exhausted", "quota exhausted"},
		{"429", "rate limited / quota"},
		{"ThrottlingException", "throttled"},
		{"context deadline exceeded", "timeout"},
		{"connection refused", "transport"},
		{"no such host", "transport"},
		{"EOF", "transport"},
	} {
		if strings.Contains(msg, probe.needle) {
			return probe.label
		}
	}
	return ""
}

// skipOnInfraError skips the test when err is an infrastructure failure, so a dead
// key or an exhausted quota reports "no verdict" instead of masquerading as a
// verdict about the payload — in either direction.
func skipOnInfraError(t *testing.T, err error) {
	t.Helper()
	if kind := infraFailure(err); kind != "" {
		t.Skipf("no shape verdict — %s: %v", kind, err)
	}
}

// echoTool is a trivially-callable tool spec, used so the model has something to
// invoke without side effects.
func echoTool() KiroToolWrapper {
	var w KiroToolWrapper
	w.ToolSpecification.Name = "get_time"
	w.ToolSpecification.Description = "Returns the current time for a city."
	w.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"city": map[string]interface{}{"type": "string", "description": "City name"},
		},
		"required": []interface{}{"city"},
	}}
	return w
}

// buildLivePayloadWithPairs builds a payload whose history contains n COMPLETE
// structured tool pairs (assistant toolUses followed by user toolResults), then a
// final user question. This is the exact shape commit 72da572 claimed upstream
// rejects.
func buildLivePayloadWithPairs(n int, region string) *KiroPayload {
	p := &KiroPayload{}
	cs := &p.ConversationState
	cs.ChatTriggerType = "MANUAL"
	cs.ConversationID = fmt.Sprintf("live-probe-pairs-%d", n)

	hist := []KiroHistoryMessage{{UserInputMessage: &KiroUserInputMessage{
		Content: "You can call get_time. Keep every reply to one short sentence.",
		Origin:  "AI_EDITOR",
	}}}
	hist = append(hist, KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
		Content: "Understood.",
	}})

	cities := []string{"Hanoi", "Tokyo", "Paris", "Cairo", "Lima", "Oslo", "Delhi", "Perth",
		"Rome", "Doha", "Kiev", "Bern", "Suva", "Male", "Apia", "Riga"}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("probe_tool_%d", i)
		city := cities[i%len(cities)]
		// Each cycle is user question → assistant toolUse → user toolResult →
		// assistant answer, so history stays strictly alternating and ends on an
		// assistant turn (currentMessage supplies the final user turn).
		hist = append(hist,
			KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
				Content: fmt.Sprintf("What time is it in %s?", city),
				Origin:  "AI_EDITOR",
			}},
			KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
				Content: "",
				ToolUses: []KiroToolUse{{
					ToolUseID: id,
					Name:      "get_time",
					Input:     map[string]interface{}{"city": city},
				}},
			}},
			KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
				Origin: "AI_EDITOR",
				UserInputMessageContext: &UserInputMessageContext{
					ToolResults: []KiroToolResult{{
						ToolUseID: id,
						Content:   []KiroResultContent{{Text: fmt.Sprintf("%s: 12:0%d", city, i%10)}},
						Status:    "success",
					}},
				},
			}},
			KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
				Content: fmt.Sprintf("It is 12:0%d in %s.", i%10, city),
			}},
		)
	}
	cs.History = hist
	cs.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "Now what time is it in Berlin? Use the tool.",
		Origin:  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			Tools: []KiroToolWrapper{echoTool()},
		},
	}
	return p
}

// TestLiveStructuredToolPairsAreAccepted is the decisive experiment. If the
// premise of commit 72da572 were right, these requests would fail with HTTP 400
// "Improperly formed request" as soon as history carries structured tool pairs.
//
// It escalates the pair count and stops at the first failure, so a real limit (if
// one exists) is reported rather than guessed, and quota is not burned past the
// point of information.
func TestLiveStructuredToolPairsAreAccepted(t *testing.T) {
	acct := liveProbeAccount(t)

	for _, n := range []int{1, 2, 3, 5, 8} {
		payload := buildLivePayloadWithPairs(n, acct.Region)

		// Sanity: the payload we are about to send must satisfy the ported rules,
		// otherwise a 400 would tell us nothing about the premise under test.
		if v := validateKiroConversation(mergedConversation(payload)); len(v) > 0 {
			t.Fatalf("probe payload with %d pairs is itself invalid: %s", n, formatViolations(v))
		}

		res := runLive(t, acct, payload)
		t.Logf("pairs=%-2d history=%-3d → %s", n, len(payload.ConversationState.History), res)

		if res.Err != nil {
			msg := res.Err.Error()
			if strings.Contains(msg, "Improperly formed") || strings.Contains(msg, "400") {
				t.Fatalf("upstream REJECTED %d structured pairs: %v\n"+
					"This would contradict kiro_history_repair.go and mean structured\n"+
					"pairs really are capped; the repair strategy would need revisiting.", n, res.Err)
			}
			if kind := infraFailure(res.Err); kind != "" {
				t.Skipf("probe stopped at %d pairs — %s, not a shape verdict: %v", n, kind, res.Err)
			}
			t.Fatalf("probe failed at %d pairs for an unrelated reason: %v", n, res.Err)
		}
	}
}

// TestLiveBrokenPairingIsRejected is the other half of the claim: that broken
// pairing — not structured tool turns — is what upstream actually rejects.
//
// It deliberately sends an assistant tool call with NO answering results turn
// (the TOOL_USES_AND_RESULTS violation) and expects a failure. If this request
// SUCCEEDS, then upstream is more permissive than the IDE validator implies and
// the repair is stricter than strictly necessary — worth knowing, but harmless.
func TestLiveBrokenPairingIsRejected(t *testing.T) {
	acct := liveProbeAccount(t)

	p := &KiroPayload{}
	cs := &p.ConversationState
	cs.ChatTriggerType = "MANUAL"
	cs.ConversationID = "live-probe-broken-pair"
	cs.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "What time is it in Hanoi?", Origin: "AI_EDITOR"}},
		// Assistant calls a tool; the next turn does NOT answer it.
		{AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content: "",
			ToolUses: []KiroToolUse{{
				ToolUseID: "orphan_call_1",
				Name:      "get_time",
				Input:     map[string]interface{}{"city": "Hanoi"},
			}},
		}},
		{UserInputMessage: &KiroUserInputMessage{Content: "Never mind.", Origin: "AI_EDITOR"}},
	}
	cs.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "Say OK.",
		Origin:  "AI_EDITOR",
	}

	// Confirm this is the violation we intend to send.
	rules := ruleSet(validateKiroConversation(mergedConversation(p)))
	if !rules[ruleToolUsesAndResults] {
		t.Fatalf("probe payload was supposed to violate %s, got %v", ruleToolUsesAndResults, rules)
	}

	res := runLive(t, acct, p)
	t.Logf("broken pairing → %s", res)

	if res.Err == nil {
		t.Logf("NOTE: upstream ACCEPTED a broken tool pair. The IDE validator is " +
			"stricter than the service; repairing is still correct but not strictly required here.")
		return
	}
	// An auth or quota failure says nothing about pairing. Without this guard the
	// test passes on a dead credential and looks like evidence.
	if kind := infraFailure(res.Err); kind != "" {
		t.Skipf("no shape verdict — %s: %v", kind, res.Err)
	}
	t.Logf("upstream rejected the broken pair as expected: %v", res.Err)
}

// TestLiveRepairMakesBrokenPairingSendable ties the fix to the observed behavior:
// the same broken conversation, after repairKiroPayload, is accepted.
func TestLiveRepairMakesBrokenPairingSendable(t *testing.T) {
	acct := liveProbeAccount(t)

	build := func() *KiroPayload {
		p := &KiroPayload{}
		cs := &p.ConversationState
		cs.ChatTriggerType = "MANUAL"
		cs.ConversationID = "live-probe-repaired"
		cs.History = []KiroHistoryMessage{
			{UserInputMessage: &KiroUserInputMessage{Content: "What time is it in Hanoi?", Origin: "AI_EDITOR"}},
			{AssistantResponseMessage: &KiroAssistantResponseMessage{
				Content: "",
				ToolUses: []KiroToolUse{{
					ToolUseID: "orphan_call_1",
					Name:      "get_time",
					Input:     map[string]interface{}{"city": "Hanoi"},
				}},
			}},
			{UserInputMessage: &KiroUserInputMessage{Content: "Never mind, just say OK.", Origin: "AI_EDITOR"}},
		}
		cs.CurrentMessage.UserInputMessage = KiroUserInputMessage{
			Content: "Say OK.",
			Origin:  "AI_EDITOR",
		}
		return p
	}

	p := build()
	repairKiroPayload(p)
	if v := validateKiroConversation(mergedConversation(p)); len(v) > 0 {
		t.Fatalf("repair left violations: %s", formatViolations(v))
	}

	res := runLive(t, acct, p)
	t.Logf("repaired → %s", res)
	if res.Err != nil {
		skipOnInfraError(t, res.Err)
		t.Fatalf("repaired payload was rejected: %v", res.Err)
	}
}

// TestLiveEndToEndClaudeToolLoop exercises the real translation path: a Claude
// request carrying a completed tool cycle plus a follow-up that needs another
// tool call. This is the shape that was failing in production — the model saw
// thousands of "announce, then stop" examples and imitated them.
//
// A pass means the translated payload is accepted AND the model issues a real
// structured tool call instead of narrating one.
func TestLiveEndToEndClaudeToolLoop(t *testing.T) {
	acct := liveProbeAccount(t)

	req := &ClaudeRequest{
		Model:     "claude-sonnet-4.5",
		MaxTokens: 512,
		System:    "You have a get_time tool. Always use it for time questions. Keep replies to one sentence.",
		Tools: []ClaudeTool{{
			Name:        "get_time",
			Description: "Returns the current time for a city.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"city": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"city"},
			},
		}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "What time is it in Hanoi?"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "c1", "name": "get_time",
					"input": map[string]interface{}{"city": "Hanoi"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "c1", "content": "Hanoi: 14:05"},
			}},
			{Role: "assistant", Content: "It is 14:05 in Hanoi."},
			{Role: "user", Content: "And in Tokyo?"},
		},
	}

	payload := ClaudeToKiro(req, false)
	if v := validateKiroConversation(mergedConversation(payload)); len(v) > 0 {
		t.Fatalf("translated payload is invalid: %s\n%s",
			formatViolations(v), dumpConversation(mergedConversation(payload)))
	}

	// The historical tool pair must have survived translation structurally.
	uses := 0
	for _, h := range payload.ConversationState.History {
		if h.AssistantResponseMessage != nil {
			uses += len(h.AssistantResponseMessage.ToolUses)
		}
	}
	if uses == 0 {
		t.Fatalf("translation stripped the historical structured tool call")
	}

	res := runLive(t, acct, payload)
	t.Logf("claude tool loop → %s", res)
	if res.Err != nil {
		skipOnInfraError(t, res.Err)
		t.Fatalf("upstream rejected the translated payload: %v", res.Err)
	}
	if len(res.ToolUses) == 0 {
		t.Errorf("model did not issue a structured tool call; text was %q\n"+
			"(this is the production symptom: it states intent and ends the turn)", res.Text)
	}
}
