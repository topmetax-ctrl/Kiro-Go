package tokenopt

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(nil)
	t.Cleanup(m.Close)
	return m
}

func cfgFor(f *fakeHeadroom, mut func(*config.TokenOptimizationConfig)) config.TokenOptimizationConfig {
	c := config.TokenOptimizationConfig{
		Mode: "on", InputCompression: "headroom",
		Headroom: config.HeadroomConfig{BaseURL: f.server.URL, TimeoutMs: 1500, MinBytes: 256},
	}
	if mut != nil {
		mut(&c)
	}
	return c.Resolved()
}

func claudeReq(body []byte) InputRequest {
	return InputRequest{Protocol: ProtocolClaude, Body: body, Model: "claude-test", Principal: "key-1"}
}

func counter(m *Manager, engine, outcome, reason string) int64 {
	for _, c := range m.mx.snapshot().Counters {
		if c.Engine == engine && c.Outcome == outcome && c.Reason == reason {
			return c.Count
		}
	}
	return 0
}

// resultText reads the text of the tool result carried by message idx.
func resultText(t *testing.T, body []byte, idx int) string {
	t.Helper()
	msgs := decodeBody(t, body)["messages"].([]any)
	return msgs[idx].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
}

func TestOffInactiveAndRTKNeverTouchOrCallTheEngine(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})

	for name, cfg := range map[string]config.TokenOptimizationConfig{
		"mode off":             cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Mode = "off" }),
		"engine none":          cfgFor(f, func(c *config.TokenOptimizationConfig) { c.InputCompression = "none" }),
		"rtk (no gateway API)": cfgFor(f, func(c *config.TokenOptimizationConfig) { c.InputCompression = "rtk" }),
		"zero config":          config.TokenOptimizationConfig{}.Resolved(),
	} {
		t.Run(name, func(t *testing.T) {
			res := m.OptimizeInput(context.Background(), cfg, claudeReq(body))
			if res.Changed || string(res.Body) != string(body) {
				t.Fatalf("body was altered: changed=%v", res.Changed)
			}
		})
	}
	if f.callCount() != 0 {
		t.Fatalf("engine was called %d times", f.callCount())
	}
}

func TestOnCompressesToolResultsAndKeepsTheEnvelope(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)
	big1, big2 := big("a", 50), big("b", 60)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big1, false}, claudeTurn{"Grep", "toolu_2", big2, false})

	res := m.OptimizeInput(context.Background(), cfgFor(f, nil), claudeReq(body))
	if !res.Changed || res.Outcome != OutcomeApplied {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Body) >= len(body) {
		t.Fatalf("body did not shrink: %d -> %d", len(body), len(res.Body))
	}

	// Everything except the two slot strings is exactly the client's request.
	want := decodeBody(t, body)
	msgs := want["messages"].([]any)
	msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = squeeze(big1)
	msgs[4].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = squeeze(big2)
	if got := decodeBody(t, res.Body); !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Fatalf("envelope changed.\n got %s\nwant %s", gj, wj)
	}

	snap := m.mx.snapshot()
	if snap.Replaced != 2 || snap.BytesBefore <= snap.BytesAfter || snap.EstTokensSaved <= 0 || snap.EngineTokensSaved != int64(f.tokensSaved) {
		t.Fatalf("metrics = %+v", snap)
	}
	if len(snap.Recent) != 1 || snap.Recent[0].Outcome != OutcomeApplied || snap.Recent[0].Replaced != 2 {
		t.Fatalf("recent = %+v", snap.Recent)
	}
}

// The client re-sends the same history every turn. Replaying the first answer
// is what keeps the cached prefix byte-identical and spares the engine.
func TestMemoReplaysWithoutCallingTheEngine(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)
	cfg := cfgFor(f, nil)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})

	first := m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	second := m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	if f.callCount() != 1 {
		t.Fatalf("engine calls = %d, want 1: the second turn must replay", f.callCount())
	}
	if string(first.Body) != string(second.Body) || !second.Changed || second.Reason != ReasonFromMemo {
		t.Fatalf("replay differs from the first answer (reason %q)", second.Reason)
	}

	// Another caller never reuses (or influences) this caller's entries.
	other := claudeReq(body)
	other.Principal = "key-2"
	m.OptimizeInput(context.Background(), cfg, other)
	if f.callCount() != 2 {
		t.Fatalf("engine calls = %d, want 2: memo must be scoped per principal", f.callCount())
	}
}

func TestGrowingConversationKeepsThePrefixAndOnlyAsksAboutNewResults(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)
	cfg := cfgFor(f, nil) // cache mode
	turn1 := claudeTurn{"Bash", "toolu_1", big("a", 50), false}
	turn2 := claudeTurn{"Bash", "toolu_2", big("b", 50), false}

	r1 := m.OptimizeInput(context.Background(), cfg, claudeReq(claudeBody(turn1)))
	r2 := m.OptimizeInput(context.Background(), cfg, claudeReq(claudeBody(turn1, turn2)))

	// Messages 0..2 (everything turn 1 sent) are byte-identical in turn 2.
	d1, _ := parseDocument(ProtocolClaude, r1.Body)
	d2, _ := parseDocument(ProtocolClaude, r2.Body)
	for i := 0; i < 3; i++ {
		if string(d1.raw[i]) != string(d2.raw[i]) {
			t.Errorf("prefix message %d changed between turns:\n%s\n%s", i, d1.raw[i], d2.raw[i])
		}
	}

	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("engine calls = %d, want 2", len(calls))
	}
	c2 := calls[1]
	// The engine is told to leave the 4 messages before the new result alone, and
	// to compress the new one right away.
	if c2.Config["frozen_message_count"].(json.Number).String() != "4" || c2.Config["protect_recent"].(json.Number).String() != "0" {
		t.Errorf("turn 2 config = %v", c2.Config)
	}
	// It sees the conversation as the provider will: turn 1's result already compressed.
	seen := c2.Messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if seen != squeeze(big("a", 50)) {
		t.Errorf("engine was shown %q instead of the replayed compression", seen)
	}
}

func TestTokenModeAsksFreshAndDoesNotRememberNoChange(t *testing.T) {
	f := newFakeHeadroom(t)
	f.compress = func(msgs []any) []any { return msgs } // engine declines (e.g. its recency guard)
	m := newMgr(t)
	cfg := cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Headroom.Mode = "token" })
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})

	m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	if f.callCount() != 2 {
		t.Fatalf("engine calls = %d, want 2: in token mode an unchanged result may compress later", f.callCount())
	}
	for _, c := range f.calls() {
		if len(c.Config) != 0 {
			t.Errorf("token mode must not freeze or override the engine: %v", c.Config)
		}
	}
}

func TestCacheModeSettlesAnUnchangedResult(t *testing.T) {
	f := newFakeHeadroom(t)
	f.compress = func(msgs []any) []any { return msgs }
	m := newMgr(t)
	cfg := cfgFor(f, nil)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})

	m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	res := m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	if f.callCount() != 1 || res.Changed {
		t.Fatalf("calls=%d changed=%v: a settled result must stay as sent and not be re-asked", f.callCount(), res.Changed)
	}
}

// Even an engine that compresses everything must not be able to touch file
// reads, error output or results too small to be worth it.
func TestProtectedAndSmallResultsAreNeverReplaced(t *testing.T) {
	f := newFakeHeadroom(t)
	f.compress = func(msgs []any) []any { // eager engine: squeezes every tool result
		var walk func(n any)
		walk = func(n any) {
			switch v := n.(type) {
			case []any:
				for _, e := range v {
					walk(e)
				}
			case map[string]any:
				if v["type"] == "tool_result" {
					v["content"] = "X"
				}
				for _, e := range v {
					walk(e)
				}
			}
		}
		walk(msgs)
		return msgs
	}
	m := newMgr(t)
	read, small, errOut, bash := big("r", 50), "tiny result", big("e", 50), big("b", 50)
	body := claudeBody(
		claudeTurn{"Read", "toolu_1", read, false},
		claudeTurn{"Bash", "toolu_2", small, false},
		claudeTurn{"Bash", "toolu_3", errOut, true},
		claudeTurn{"Bash", "toolu_4", bash, false},
	)
	res := m.OptimizeInput(context.Background(), cfgFor(f, nil), claudeReq(body))
	if !res.Changed {
		t.Fatal("the compressible result was not compressed")
	}
	for idx, want := range map[int]string{2: read, 4: small, 6: errOut, 8: "X"} {
		if got := resultText(t, res.Body, idx); got != want && !(want == "X" && got == "X") {
			t.Errorf("result at message %d = %.30q, want %.30q", idx, got, want)
		}
	}
}

func TestEngineFailureSendsTheOriginalBytes(t *testing.T) {
	for name, set := range map[string]func(f *fakeHeadroom){
		"server error":    func(f *fakeHeadroom) { f.status = 500 },
		"rejected":        func(f *fakeHeadroom) { f.status = 401 },
		"malformed reply": func(f *fakeHeadroom) { f.raw = "garbage" },
		"timeout":         func(f *fakeHeadroom) { f.delay = 400 * time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeHeadroom(t)
			set(f)
			m := newMgr(t)
			body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
			cfg := cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Headroom.TimeoutMs = 150 })

			res := m.OptimizeInput(context.Background(), cfg, claudeReq(body))
			if res.Changed || string(res.Body) != string(body) {
				t.Fatalf("a failed engine must leave the request byte-identical")
			}
			if res.Outcome != OutcomeError {
				t.Fatalf("outcome = %q, want error", res.Outcome)
			}
		})
	}
}

func TestUnconfiguredEngineFailsOpen(t *testing.T) {
	m := newMgr(t)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
	cfg := config.TokenOptimizationConfig{Mode: "on", InputCompression: "headroom"}.Resolved() // no URL
	if res := m.OptimizeInput(context.Background(), cfg, claudeReq(body)); res.Changed || res.Reason != ReasonUnconfigured {
		t.Fatalf("res = %+v", res)
	}
}

func TestCircuitBreakerOpensThenRecovers(t *testing.T) {
	f := newFakeHeadroom(t)
	f.status = 500
	m := newMgr(t)
	clock := time.Now()
	m.br.now = func() time.Time { return clock }
	cfg := cfgFor(f, nil)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})

	for i := 0; i < 3; i++ {
		m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	}
	if m.br.state() != "open" {
		t.Fatalf("breaker = %s after 3 failures", m.br.state())
	}
	before := f.callCount()
	if res := m.OptimizeInput(context.Background(), cfg, claudeReq(body)); res.Reason != ReasonCircuitOpen || res.Changed {
		t.Fatalf("res = %+v", res)
	}
	if f.callCount() != before {
		t.Fatal("an open circuit must not call the engine")
	}

	// Cooldown passes and the engine has recovered: one trial call closes it.
	clock = clock.Add(31 * time.Second)
	f.mu.Lock()
	f.status = 0
	f.mu.Unlock()
	if res := m.OptimizeInput(context.Background(), cfg, claudeReq(body)); !res.Changed {
		t.Fatalf("half-open trial did not go through: %+v", res)
	}
	if m.br.state() != "closed" {
		t.Fatalf("breaker = %s after a good trial", m.br.state())
	}
}

func TestCallerCancellationDoesNotCountAgainstTheEngine(t *testing.T) {
	f := newFakeHeadroom(t)
	f.delay = 400 * time.Millisecond
	m := newMgr(t)
	cfg := cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Headroom.TimeoutMs = 5000 })
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		res := m.OptimizeInput(ctx, cfg, claudeReq(body))
		cancel()
		if res.Changed {
			t.Fatal("canceled request was altered")
		}
	}
	if m.br.state() != "closed" {
		t.Fatalf("breaker = %s: clients hanging up must never open it", m.br.state())
	}
}

func TestStructuralMismatchIsRefusedAndCountsAsAFailure(t *testing.T) {
	cases := map[string]func(msgs []any) []any{
		"message dropped": func(msgs []any) []any { return msgs[:len(msgs)-1] },
		"tool id changed": func(msgs []any) []any {
			msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["tool_use_id"] = "toolu_other"
			return compressLargeToolResults(msgs)
		},
		"roles swapped": func(msgs []any) []any { msgs[1], msgs[2] = msgs[2], msgs[1]; return msgs },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHeadroom(t)
			f.compress = mutate
			m := newMgr(t)
			body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
			res := m.OptimizeInput(context.Background(), cfgFor(f, nil), claudeReq(body))
			if res.Changed || string(res.Body) != string(body) || res.Reason != string(KindStructure) {
				t.Fatalf("res = %+v", res)
			}
		})
	}
}

// A reply that claims savings but returns nothing usable must not be counted as
// a saving: the request was not actually made smaller.
func TestPhantomSavingsAreNotCounted(t *testing.T) {
	f := newFakeHeadroom(t)
	f.compress = func(msgs []any) []any { return msgs }
	f.tokensSaved = 900
	m := newMgr(t)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
	res := m.OptimizeInput(context.Background(), cfgFor(f, nil), claudeReq(body))
	if res.Changed {
		t.Fatal("nothing was replaced, yet the body changed")
	}
	if snap := m.mx.snapshot(); snap.PhantomSavings != 1 || snap.EngineTokensSaved != 0 || snap.Replaced != 0 {
		t.Fatalf("metrics = %+v", snap)
	}
}

func TestUnusableReplacementsAreNotAdopted(t *testing.T) {
	for name, repl := range map[string]func(old string) string{
		"longer": func(old string) string { return old + "more" },
		"empty":  func(string) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeHeadroom(t)
			f.compress = func(msgs []any) []any {
				msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = repl(big("a", 50))
				return msgs
			}
			m := newMgr(t)
			body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
			if res := m.OptimizeInput(context.Background(), cfgFor(f, nil), claudeReq(body)); res.Changed {
				t.Fatalf("an unusable replacement was adopted")
			}
		})
	}
}

func TestObserveNeverChangesTheRequestButMeasures(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)
	cfg := cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Mode = "observe" })
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})

	res := m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	if res.Changed || string(res.Body) != string(body) {
		t.Fatal("observe mode altered the request")
	}
	deadline := time.Now().Add(3 * time.Second)
	for counter(m, EngineHeadroom, OutcomeObserved, "") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	snap := m.mx.snapshot()
	if counter(m, EngineHeadroom, OutcomeObserved, "") != 1 || snap.ObservedBytesSaved <= 0 {
		t.Fatalf("nothing observed: %+v", snap)
	}
	// Observing is not applying: no savings are claimed, no memo entries are written.
	if snap.Replaced != 0 || snap.BytesBefore != 0 {
		t.Fatalf("observe mode claimed applied savings: %+v", snap)
	}
	if n, _ := m.memo.stats(); n != 0 {
		t.Fatalf("observe mode wrote %d memo entries", n)
	}
}

func TestObserveIsBoundedAndDoesNotBlock(t *testing.T) {
	f := newFakeHeadroom(t)
	f.delay = 300 * time.Millisecond
	m := newMgr(t)
	cfg := cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Mode = "observe" })

	start := time.Now()
	for i := 0; i < 6; i++ {
		body := claudeBody(claudeTurn{"Bash", "toolu_1", big(strings.Repeat("z", i+1), 50), false})
		m.OptimizeInput(context.Background(), cfg, claudeReq(body))
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("observe blocked the request path for %s", time.Since(start))
	}
	if busy := counter(m, EngineHeadroom, OutcomeSkipped, ReasonObserveBusy); busy < 3 {
		t.Fatalf("observe_busy = %d, want the excess samples dropped", busy)
	}
}

func TestOpenAIAndResponsesBodiesThroughTheManager(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)
	cfg := cfgFor(f, nil)
	text := big("o", 50)

	openai := []byte(`{"model":"m","seed":9,"messages":[{"role":"user","content":"go"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"run","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":` + mustString(text) + `}]}`)
	res := m.OptimizeInput(context.Background(), cfg, InputRequest{Protocol: ProtocolOpenAI, Body: openai, Model: "m", Principal: "k"})
	got := decodeBody(t, res.Body)
	if !res.Changed || got["messages"].([]any)[2].(map[string]any)["content"] != squeeze(text) || got["seed"].(json.Number).String() != "9" {
		t.Fatalf("openai: %s", res.Body)
	}

	text2 := big("p", 50) // different content, or the memo would replay the first answer
	responses := []byte(`{"model":"m","previous_response_id":"resp_1","input":[
	  {"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":` + mustString(text2) + `}]}`)
	res = m.OptimizeInput(context.Background(), cfg, InputRequest{Protocol: ProtocolResponses, Body: responses, Model: "m", Principal: "k"})
	got = decodeBody(t, res.Body)
	items := got["input"].([]any)
	if !res.Changed || items[1].(map[string]any)["output"] != squeeze(text2) || got["previous_response_id"] != "resp_1" {
		t.Fatalf("responses: %s", res.Body)
	}
	// The engine was shown a chat-shaped rendering, not raw Responses items.
	seen := f.calls()[1].Messages
	if seen[0].(map[string]any)["role"] != "assistant" || seen[1].(map[string]any)["role"] != "tool" {
		t.Fatalf("engine saw %v", seen)
	}
}

func mustString(s string) string { b, _ := json.Marshal(s); return string(b) }

// An optional integration must never take a request down with it.
func TestPanicInsideTheOptimizerIsContained(t *testing.T) {
	f := newFakeHeadroom(t)
	m := NewManager(func(string) int { panic("estimator exploded") })
	t.Cleanup(m.Close)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
	res := m.OptimizeInput(context.Background(), cfgFor(f, nil), claudeReq(body))
	if res.Changed || string(res.Body) != string(body) || res.Reason != "panic" {
		t.Fatalf("res = %+v", res)
	}
}

// Settings change under live traffic; requests must stay coherent. Run with -race.
func TestConcurrentSettingsChangesAndRequests(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
	cfgs := []config.TokenOptimizationConfig{
		cfgFor(f, nil),
		cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Mode = "off" }),
		cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Mode = "observe" }),
		cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Headroom.Mode = "token" }),
		cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Headroom.TimeoutMs = 700 }),
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				res := m.OptimizeInput(context.Background(), cfgs[(g+i)%len(cfgs)], claudeReq(body))
				if !json.Valid(res.Body) {
					t.Errorf("invalid JSON produced")
					return
				}
				m.Status(cfgs[(g+i)%len(cfgs)], nil)
			}
		}(g)
	}
	wg.Wait()
}

func TestPrepareBehavior(t *testing.T) {
	m := newMgr(t)
	on := config.TokenOptimizationConfig{Mode: "on", Caveman: "caveman", Ponytail: "full"}.Resolved()

	if b := m.PrepareBehavior(config.TokenOptimizationConfig{Mode: "off", Caveman: "caveman"}.Resolved(), ""); !b.Empty() {
		t.Error("mode off must inject nothing")
	}
	if b := m.PrepareBehavior(config.TokenOptimizationConfig{Mode: "observe", Caveman: "caveman", Ponytail: "full"}.Resolved(), ""); !b.Empty() {
		t.Error("observe must never inject behavior-changing prompts")
	}
	b := m.PrepareBehavior(on, "You are helpful.")
	if b.Caveman != "caveman" || b.Ponytail != "full" || counter(m, EngineCaveman, OutcomeApplied, "") != 1 || counter(m, EnginePonytail, OutcomeApplied, "") != 1 {
		t.Fatalf("behavior = %+v", b)
	}
	b = m.PrepareBehavior(on, "…Respect: Respond terse like smart caveman…")
	if b.Caveman != "" || b.Ponytail != "full" || counter(m, EngineCaveman, OutcomeSkipped, ReasonAlreadyPresent) != 1 {
		t.Fatalf("dedup failed: %+v", b)
	}
}

func TestStatusStates(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)

	st := m.Status(config.TokenOptimizationConfig{}.Resolved(), nil)
	if st.Headroom.State != "disabled" || st.Headroom.Active || st.RTK.GatewayLevel || st.Caveman.State != "inactive" {
		t.Fatalf("zero config status = %+v", st)
	}
	if !st.AssetsVerified || len(st.Assets) != 3 || len(st.ExactBytesTools) == 0 {
		t.Fatalf("assets = verified:%v n:%d", st.AssetsVerified, len(st.Assets))
	}

	cfg := cfgFor(f, nil)
	if got := m.Status(cfg, nil).Headroom; got.State != "unverified" || !got.Active {
		t.Fatalf("unverified: %+v", got)
	}
	m.VerifyHeadroom(context.Background(), cfg.Headroom, true)
	if got := m.Status(cfg, nil).Headroom; got.State != "connected" || !got.Active || got.LastCheck == nil || got.LastCheck.Version == "" {
		t.Fatalf("connected: %+v", got)
	}
	m.VerifyHeadroom(context.Background(), config.HeadroomConfig{BaseURL: "http://127.0.0.1:1", TimeoutMs: 300}, true)
	if got := m.Status(cfg, nil).Headroom; got.State != "disconnected" {
		t.Fatalf("disconnected: %+v", got)
	}
	unconf := config.TokenOptimizationConfig{Mode: "on", InputCompression: "headroom"}.Resolved()
	if got := m.Status(unconf, nil).Headroom; got.State != "unconfigured" || got.Active {
		t.Fatalf("unconfigured: %+v", got)
	}
	obs := cfgFor(f, func(c *config.TokenOptimizationConfig) { c.Mode = "observe" })
	m.VerifyHeadroom(context.Background(), cfg.Headroom, true)
	if got := m.Status(obs, nil).Headroom; !got.Observing || got.Active {
		t.Fatalf("observe: %+v", got)
	}

	// Behaviors report active only when the engine mode is on, and observe is not observable.
	b := config.TokenOptimizationConfig{Mode: "on", Caveman: "ultracave", Ponytail: "lite"}.Resolved()
	if got := m.Status(b, nil); got.Caveman.State != "active" || got.Ponytail.State != "active" || got.Caveman.Asset == nil || got.Caveman.Observable {
		t.Fatalf("on: %+v %+v", got.Caveman, got.Ponytail)
	}
	b.Mode = "observe"
	if got := m.Status(b, nil); got.Caveman.State != "inactive" || got.Ponytail.Observable {
		t.Fatalf("observe: %+v %+v", got.Caveman, got.Ponytail)
	}
}

func TestVerifyDoesNotOverwriteStatusUnlessPersisted(t *testing.T) {
	f := newFakeHeadroom(t)
	m := newMgr(t)
	m.VerifyHeadroom(context.Background(), config.HeadroomConfig{BaseURL: f.server.URL, TimeoutMs: 1000}, false)
	if h, _ := m.LastHealth(); h != nil {
		t.Fatal("a probe of unsaved values must not become the stored status")
	}
	m.VerifyHeadroom(context.Background(), config.HeadroomConfig{BaseURL: f.server.URL, TimeoutMs: 1000}, true)
	if h, _ := m.LastHealth(); h == nil || !h.CompressOK {
		t.Fatalf("persisted probe missing: %+v", h)
	}
}

func TestRTKStatusAndRouting(t *testing.T) {
	m := newMgr(t)
	providers := []ProviderRef{
		{Name: "via-rtk", BaseURL: "http://localhost:8081/v1", Enabled: true},
		{Name: "disabled-via-rtk", BaseURL: "http://127.0.0.1:8081", Enabled: false},
		{Name: "elsewhere", BaseURL: "http://127.0.0.1:9999", Enabled: true},
		{Name: "direct", BaseURL: "https://api.example.com", Enabled: true},
	}
	cfg := config.TokenOptimizationConfig{Mode: "on", InputCompression: "rtk", RTK: config.RTKConfig{ProxyURL: "http://127.0.0.1:8081"}}.Resolved()
	st := m.Status(cfg, providers).RTK
	if !reflect.DeepEqual(st.RoutedProviders, []string{"via-rtk"}) {
		t.Fatalf("routed = %v (localhost and 127.0.0.1 are the same endpoint; disabled providers do not count)", st.RoutedProviders)
	}
	if st.GatewayLevel || st.State != "unverified" || !st.Selected {
		t.Fatalf("rtk = %+v", st)
	}
	if got := m.Status(config.TokenOptimizationConfig{Mode: "on", InputCompression: "rtk"}.Resolved(), providers).RTK; got.State != "client_side" || len(got.RoutedProviders) != 0 {
		t.Fatalf("no proxy configured: %+v", got)
	}
}

func TestMemoEvictsByCountAndBytesAndScopesKeys(t *testing.T) {
	c := newMemo(2, 1<<20)
	k := func(s string) [32]byte { return memoKey("p", "fp", s) }
	c.put(k("a"), "1")
	c.put(k("b"), "2")
	c.get(k("a")) // a is now most recent
	c.put(k("c"), "3")
	if _, ok := c.get(k("b")); ok {
		t.Error("least recently used entry survived")
	}
	if _, ok := c.get(k("a")); !ok {
		t.Error("recently used entry was evicted")
	}

	by := newMemo(100, 10)
	by.put(k("x"), strings.Repeat("v", 6))
	by.put(k("y"), strings.Repeat("v", 6))
	if _, bytes := by.stats(); bytes > 10 {
		t.Errorf("bytes = %d, want <= 10", bytes)
	}

	if memoKey("p1", "fp", "t") == memoKey("p2", "fp", "t") || memoKey("p", "fp1", "t") == memoKey("p", "fp2", "t") {
		t.Error("keys must differ by principal and by engine settings")
	}
	if memoKey("ab", "c", "t") == memoKey("a", "bc", "t") {
		t.Error("key fields must be unambiguously delimited")
	}
}

// A panic while a half-open trial permit is held must not strand the breaker.
func TestPanicReturnsTheBreakerPermit(t *testing.T) {
	f := newFakeHeadroom(t)
	m := NewManager(func(string) int { panic("estimator exploded") })
	t.Cleanup(m.Close)
	clock := time.Now()
	m.br.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ { // open the circuit
		m.br.failure()
	}
	clock = clock.Add(31 * time.Second) // half-open: exactly one trial is allowed

	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
	if res := m.OptimizeInput(context.Background(), cfgFor(f, nil), claudeReq(body)); res.Reason != "panic" {
		t.Fatalf("res = %+v, want the panic contained", res)
	}
	if !m.br.allow() {
		t.Fatal("the breaker never lets another trial through after a panic held the permit")
	}
}
