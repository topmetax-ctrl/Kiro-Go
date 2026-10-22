package cachetrace

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"kiro-go/metrics"
)

var testTracer = New([]byte("test-secret-test-secret-test-secret"))

// body builds a Messages request. mark places a breakpoint on the last block of
// message index mark (-1 for none); msgs are plain text turns.
func body(t testing.TB, msgs []string, mark int, mutate func(map[string]any)) []byte {
	t.Helper()
	var ms []any
	for i, text := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		blk := map[string]any{"type": "text", "text": text}
		if i == mark {
			blk["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		ms = append(ms, map[string]any{"role": role, "content": []any{blk}})
	}
	req := map[string]any{
		"model":    "claude-sonnet-5-5",
		"system":   []any{map[string]any{"type": "text", "text": "system prompt"}},
		"tools":    []any{map[string]any{"name": "Bash", "description": "run"}},
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1000},
		"messages": ms,
	}
	if mutate != nil {
		mutate(req)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func turns(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("turn %d with some text", i)
	}
	return out
}

func prefixAt(tr *metrics.CacheTrace, k int) string {
	want := fmt.Sprintf("%d:", k)
	for _, p := range tr.Prefix {
		if strings.HasPrefix(p, want) {
			return p
		}
	}
	return ""
}

func TestTraceIsStableAndKeyed(t *testing.T) {
	b := body(t, turns(5), 4, nil)
	a, c := testTracer.Trace(b, "off"), testTracer.Trace(b, "off")
	if a == nil {
		t.Fatal("no trace for a valid body")
	}
	ja, _ := json.Marshal(a)
	jc, _ := json.Marshal(c)
	if string(ja) != string(jc) {
		t.Fatalf("same body, different traces:\n%s\n%s", ja, jc)
	}
	other := New([]byte("another-secret-another-secret-xx")).Trace(b, "off")
	if other.Conv == a.Conv || other.Tools == a.Tools || other.System == a.System {
		t.Fatalf("a different key must give different digests: %+v vs %+v", other, a)
	}
	// Digests carry no recoverable text.
	if strings.Contains(string(ja), "system prompt") || strings.Contains(string(ja), "turn 0") {
		t.Fatalf("trace leaks content: %s", ja)
	}
	if a.Msgs != 5 || a.Policy != "off" {
		t.Fatalf("msgs/policy = %d/%q", a.Msgs, a.Policy)
	}
	for _, d := range []string{a.Conv, a.Tools, a.System, a.Settings} {
		if len(d) != digestBytes*2 {
			t.Fatalf("digest %q is not %d hex chars", d, digestBytes*2)
		}
	}
}

func TestMarkersDoNotChangeDigests(t *testing.T) {
	msgs := turns(6)
	plain := testTracer.Trace(body(t, msgs, -1, nil), "")
	for _, mark := range []int{0, 3, 5} {
		got := testTracer.Trace(body(t, msgs, mark, nil), "")
		if got.Conv != plain.Conv || got.Tools != plain.Tools || got.System != plain.System {
			t.Fatalf("mark on %d changed an anchor digest", mark)
		}
		if strings.Join(got.Prefix, ",") != strings.Join(plain.Prefix, ",") {
			t.Fatalf("mark on %d changed the prefix chain:\n%v\n%v", mark, got.Prefix, plain.Prefix)
		}
	}
	// Markers on tools and system digest the same too.
	marked := testTracer.Trace(body(t, msgs, -1, func(r map[string]any) {
		r["tools"].([]any)[0].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
		r["system"].([]any)[0].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral"}
	}), "")
	if marked.Tools != plain.Tools || marked.System != plain.System {
		t.Fatal("a breakpoint on tools or system changed their digest")
	}
}

func TestNextTurnExtendsPreviousPrefix(t *testing.T) {
	msgs := turns(9)
	prev := testTracer.Trace(body(t, msgs[:5], 4, nil), "")
	next := testTracer.Trace(body(t, msgs[:7], 6, nil), "")
	want := prefixAt(prev, 5)
	if want == "" || prefixAt(next, 5) != want {
		t.Fatalf("next turn must list the previous request's full prefix %q in its checkpoints %v", want, next.Prefix)
	}
	if prev.Conv != next.Conv {
		t.Fatal("one conversation must keep one anchor")
	}
}

func TestDivergenceIsLocatedByCheckpoints(t *testing.T) {
	base := turns(40)
	changed := append([]string(nil), base...)
	changed[20] = "an old tool result that was rewritten"
	a := testTracer.Trace(body(t, base, 39, nil), "")
	b := testTracer.Trace(body(t, changed, 39, nil), "")
	if a.Conv != b.Conv {
		t.Fatal("editing message 20 must not move the anchor")
	}
	// Checkpoints before the edit agree, from the edit on they differ.
	for _, k := range []int{16} {
		if prefixAt(a, k) == "" || prefixAt(a, k) != prefixAt(b, k) {
			t.Fatalf("prefix %d should match: %q vs %q", k, prefixAt(a, k), prefixAt(b, k))
		}
	}
	for _, k := range []int{32, 40} {
		if prefixAt(a, k) == prefixAt(b, k) {
			t.Fatalf("prefix %d should differ after the edit", k)
		}
	}
}

func TestEachPartHasItsOwnDigest(t *testing.T) {
	msgs := turns(4)
	base := testTracer.Trace(body(t, msgs, 3, nil), "")

	tools := testTracer.Trace(body(t, msgs, 3, func(r map[string]any) {
		r["tools"].([]any)[0].(map[string]any)["description"] = "changed"
	}), "")
	if tools.Tools == base.Tools || tools.System != base.System || tools.Settings != base.Settings || tools.Conv != base.Conv {
		t.Fatalf("tools change must move Tools only: %+v", tools)
	}
	if prefixAt(tools, 4) == prefixAt(base, 4) {
		t.Fatal("a tools change must break the prefix chain")
	}

	system := testTracer.Trace(body(t, msgs, 3, func(r map[string]any) {
		r["system"].([]any)[0].(map[string]any)["text"] = "changed"
	}), "")
	if system.System == base.System || system.Tools != base.Tools || system.Settings != base.Settings {
		t.Fatalf("system change must move System only: %+v", system)
	}

	thinking := testTracer.Trace(body(t, msgs, 3, func(r map[string]any) {
		r["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 2000}
	}), "")
	if thinking.Settings == base.Settings || thinking.Tools != base.Tools || thinking.System != base.System {
		t.Fatalf("thinking change must move Settings only: %+v", thinking)
	}
	if prefixAt(thinking, 4) != prefixAt(base, 4) {
		t.Fatal("settings are not part of the prefix chain")
	}

	for name, mut := range map[string]func(map[string]any){
		"effort":      func(r map[string]any) { r["output_config"] = map[string]any{"effort": "high"} },
		"tool_choice": func(r map[string]any) { r["tool_choice"] = map[string]any{"type": "auto"} },
		"model":       func(r map[string]any) { r["model"] = "claude-opus-5-5" },
	} {
		got := testTracer.Trace(body(t, msgs, 3, mut), "")
		if got.Settings == base.Settings {
			t.Fatalf("%s must move Settings", name)
		}
	}
}

func TestAbsentToolsAndSystemAreOmitted(t *testing.T) {
	tr := testTracer.Trace(body(t, turns(2), -1, func(r map[string]any) {
		delete(r, "tools")
		delete(r, "system")
	}), "")
	if tr == nil || tr.Tools != "" || tr.System != "" {
		t.Fatalf("absent parts must stay empty: %+v", tr)
	}
}

func TestMarksListBreakpointsAndTTL(t *testing.T) {
	tr := testTracer.Trace(body(t, turns(5), 4, func(r map[string]any) {
		r["system"].([]any)[0].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
		r["tools"].([]any)[0].(map[string]any)["cache_control"] = nil
	}), "")
	if got := strings.Join(tr.Marks, ","); got != "s0:1h,m4" {
		t.Fatalf("marks = %q, want s0:1h,m4 (null is no breakpoint)", got)
	}
}

func TestNotAMessagesBodyYieldsNoTrace(t *testing.T) {
	for _, b := range []string{``, `not json`, `{}`, `{"messages":[]}`, `{"messages":"x"}`, `[1,2]`} {
		if tr := testTracer.Trace([]byte(b), ""); tr != nil {
			t.Fatalf("body %q gave a trace %+v", b, tr)
		}
	}
	var nilTracer *Tracer
	if nilTracer.Trace(body(t, turns(1), -1, nil), "") != nil || New(nil) != nil || (&Tracer{}).Trace(body(t, turns(1), -1, nil), "") != nil {
		t.Fatal("a missing key must disable tracing, not hash with an empty key")
	}
}

func TestStripMarks(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"none", `{"a":1}`, `{"a":1}`},
		{"last member", `{"type":"text","cache_control":{"type":"ephemeral"}}`, `{"type":"text"}`},
		{"first member", `{"cache_control":{"type":"ephemeral"},"type":"text"}`, `{"type":"text"}`},
		{"middle member", `{"a":1,"cache_control":{"type":"ephemeral","ttl":"1h"},"b":2}`, `{"a":1,"b":2}`},
		{"sole member", `{"cache_control":{"type":"ephemeral"}}`, `{}`},
		{"null", `{"a":1,"cache_control":null}`, `{"a":1}`},
		{"spaced marker", `{"a":1,"cache_control" : { "type": "ephemeral" },"b":2}`, `{"a":1,"b":2}`},
		{"two blocks", `[{"a":1,"cache_control":{"type":"ephemeral"}},{"b":2,"cache_control":{"type":"ephemeral"}}]`, `[{"a":1},{"b":2}]`},
		{"mentioned in text", `{"text":"say \"cache_control\":{\"type\":\"ephemeral\"} here"}`, `{"text":"say \"cache_control\":{\"type\":\"ephemeral\"} here"}`},
		{"as a value", `{"kind":"cache_control","x":1}`, `{"kind":"cache_control","x":1}`},
		{"nested left alone", `{"cache_control":{"a":{"b":1}}}`, `{"cache_control":{"a":{"b":1}}}`},
		{"truncated", `{"cache_control":{"type"`, `{"cache_control":{"type"`},
		{"brace in string", `{"a":1,"cache_control":{"type":"e}x"}}`, `{"a":1}`},
	}
	for _, c := range cases {
		if got := string(stripMarks([]byte(c.in))); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestLongHistoryKeepsCheckpointsBounded(t *testing.T) {
	for _, n := range []int{1, 3, 4, 15, 16, 17, 100, 400, 2000} {
		set := checkpoints(n)
		if !set[n] {
			t.Fatalf("n=%d: newest boundary must always be listed", n)
		}
		if len(set) > tailCheckpoints+maxStrides {
			t.Fatalf("n=%d: %d checkpoints exceed the bound", n, len(set))
		}
		for k := range set {
			if k < 1 || k > n {
				t.Fatalf("n=%d: checkpoint %d out of range", n, k)
			}
		}
	}
	tr := testTracer.Trace(body(t, turns(400), 399, nil), "")
	if len(tr.Prefix) > tailCheckpoints+maxStrides {
		t.Fatalf("trace lists %d checkpoints", len(tr.Prefix))
	}
}

func TestConcurrentUseIsConsistent(t *testing.T) {
	b := body(t, turns(60), 59, nil)
	want, _ := json.Marshal(testTracer.Trace(b, "p"))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				got, _ := json.Marshal(testTracer.Trace(b, "p"))
				if string(got) != string(want) {
					t.Errorf("concurrent trace differs")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// An unmodified conversation replayed one turn at a time must chain: every
// request lists its predecessor's full prefix.
func TestChainAcrossManyTurns(t *testing.T) {
	msgs := turns(80)
	var prevFull string
	for n := 1; n <= 79; n += 2 {
		tr := testTracer.Trace(body(t, msgs[:n], n-1, nil), "")
		if prevFull != "" {
			found := false
			for _, p := range tr.Prefix {
				if p == prevFull {
					found = true
				}
			}
			if !found {
				t.Fatalf("n=%d does not list previous full prefix %s in %v", n, prevFull, tr.Prefix)
			}
		}
		prevFull = prefixAt(tr, n)
	}
}

func BenchmarkTraceLargeBody(b *testing.B) {
	msgs := make([]string, 300)
	for i := range msgs {
		msgs[i] = strings.Repeat("x", 6000)
	}
	bd := body(b, msgs, 299, nil)
	b.SetBytes(int64(len(bd)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if testTracer.Trace(bd, "") == nil {
			b.Fatal("nil trace")
		}
	}
}

// A trace is stored on every forward event of a 5000-entry ring, so its size
// must stay bounded however long the conversation gets.
func TestTraceSizeIsBounded(t *testing.T) {
	for _, n := range []int{3, 60, 400, 3000} {
		tr := testTracer.Trace(body(t, turns(n), n-1, func(r map[string]any) {
			r["system"].([]any)[0].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
		}), "on+headroom-cache+ultracave+ponytail-full")
		raw, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("n=%d: %d bytes", n, len(raw))
		if len(raw) > 1200 {
			t.Fatalf("n=%d: trace is %d bytes", n, len(raw))
		}
	}
}
