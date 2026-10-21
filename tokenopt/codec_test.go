package tokenopt

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeSlotDiscovery(t *testing.T) {
	body := claudeBody(
		claudeTurn{"Bash", "toolu_1", big("a", 50), false},
		claudeTurn{"Read", "toolu_2", big("b", 50), false},
		claudeTurn{"Bash", "toolu_3", "boom: exit 1", true}, // an error result must be left alone
	)
	d, err := parseDocument(ProtocolClaude, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.slots) != 2 {
		t.Fatalf("slots = %d, want 2 (the is_error result is never a slot)", len(d.slots))
	}
	if d.slots[0].tool != "Bash" || d.slots[0].id != "toolu_1" || d.slots[0].viewMsg != 2 {
		t.Errorf("slot 0 = %+v", d.slots[0])
	}
	if d.slots[1].tool != "Read" || d.slots[1].id != "toolu_2" || d.slots[1].viewMsg != 4 {
		t.Errorf("slot 1 = %+v", d.slots[1])
	}
}

func TestClaudeToolResultTextBlocksAreSlots(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Grep","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
	     {"type":"text","text":"first"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"second"}]}]}]}`)
	d, err := parseDocument(ProtocolClaude, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.slots) != 2 || d.slots[0].text != "first" || d.slots[1].text != "second" {
		t.Fatalf("slots = %+v, want the two text blocks only (never the image)", d.slots)
	}
	out, err := d.apply(map[int]string{1: "2nd"})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeBody(t, out)
	blocks := got["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "first" || blocks[2].(map[string]any)["text"] != "2nd" {
		t.Fatalf("wrong block edited: %v", blocks)
	}
	if blocks[1].(map[string]any)["source"].(map[string]any)["data"] != "AAAA" {
		t.Fatalf("image block was disturbed: %v", blocks[1])
	}
}

// apply must change exactly the slot strings and nothing else: unknown
// top-level fields, numeric literals, other messages, ids, markers.
func TestApplyChangesOnlyTheSlotString(t *testing.T) {
	body := claudeBody(
		claudeTurn{"Bash", "toolu_1", big("a", 50), false},
		claudeTurn{"Bash", "toolu_2", big("b", 50), false},
	)
	d, err := parseDocument(ProtocolClaude, body)
	if err != nil {
		t.Fatal(err)
	}
	const short = "S <&> \"q\"   ✓"
	out, err := d.apply(map[int]string{1: short})
	if err != nil {
		t.Fatal(err)
	}

	want := decodeBody(t, body)
	want["messages"].([]any)[4].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = short
	if got := decodeBody(t, out); !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Fatalf("apply changed more than the slot.\n got: %s\nwant: %s", gj, wj)
	}

	// Untouched messages keep their exact bytes.
	d2, err := parseDocument(ProtocolClaude, out)
	if err != nil {
		t.Fatal(err)
	}
	for i := range d.raw {
		if i != 4 && string(d.raw[i]) != string(d2.raw[i]) {
			t.Errorf("untouched message %d changed:\n%s\n%s", i, d.raw[i], d2.raw[i])
		}
	}
	// Text we write is not HTML-escaped ("<" stays "<"). The fixture itself was
	// built with json.Marshal, so untouched messages legitimately keep their own
	// < escapes: they are carried over byte for byte.
	if !strings.Contains(string(out), `S <&> `) {
		t.Errorf("replacement text was HTML-escaped on the way out: %s", out)
	}
	if !strings.Contains(string(out), `<`) {
		t.Errorf("untouched messages should keep their original bytes, escapes included")
	}
}

func TestApplyKeepsNumbersAndUnknownFieldsByteExact(t *testing.T) {
	text := big("x", 40)
	raw := `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` +
		`"` + strings.ReplaceAll(text, "\n", `\n`) + `","big":12345678901234567890,"f":1.50,"exp":1e3}]}],"unknown":{"z":1,"a":[1.0,2.50]}}`
	d, err := parseDocument(ProtocolClaude, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.apply(map[int]string{0: "tiny"})
	if err != nil {
		t.Fatal(err)
	}
	for _, lit := range []string{`12345678901234567890`, `1.50`, `1e3`, `"unknown":{"z":1,"a":[1.0,2.50]}`} {
		if !strings.Contains(string(out), lit) {
			t.Errorf("literal %s did not survive: %s", lit, out)
		}
	}
}

func TestOpenAIToolMessagesAreSlots(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
	  {"role":"system","content":"sys"},
	  {"role":"user","content":"go"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"run","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"` + strings.Repeat("o", 300) + `"},
	  {"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"` + strings.Repeat("p", 300) + `"}]}],
	  "logprobs":true,"seed":7}`)
	d, err := parseDocument(ProtocolOpenAI, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.slots) != 2 || d.slots[0].tool != "run" || d.slots[1].tool != "read_file" {
		t.Fatalf("slots = %+v", d.slots)
	}
	if got := candidates(d, 100); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("candidates = %v, want only the non-file-tool result", got)
	}
	out, err := d.apply(map[int]string{0: "short", 1: "also short"})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeBody(t, out)
	msgs := got["messages"].([]any)
	if msgs[3].(map[string]any)["content"] != "short" || msgs[3].(map[string]any)["tool_call_id"] != "call_1" {
		t.Errorf("tool message wrong: %v", msgs[3])
	}
	if msgs[4].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "also short" {
		t.Errorf("tool parts wrong: %v", msgs[4])
	}
	if got["seed"].(json.Number).String() != "7" || got["logprobs"] != true {
		t.Errorf("unknown top-level fields lost: %v", got)
	}
}

func TestResponsesInputIsRenderedAndMappedBack(t *testing.T) {
	body := []byte(`{"model":"m","previous_response_id":"resp_1","store":true,"input":[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	  {"type":"reasoning","id":"rs_1","summary":[]},
	  {"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
	  {"type":"function_call_output","call_id":"c1","output":"` + strings.Repeat("z", 300) + `"},
	  {"type":"function_call_output","call_id":"c2","output":[{"type":"output_text","text":"parts"}]}]}`)
	d, err := parseDocument(ProtocolResponses, body)
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, m := range d.view {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "tool", "tool"}) {
		t.Fatalf("view roles = %v (reasoning has no chat form and is left out)", roles)
	}
	if len(d.slots) != 1 || d.slots[0].tool != "shell" || d.slots[0].id != "c1" || d.slots[0].viewMsg != 2 || d.slots[0].rawIdx != 3 {
		t.Fatalf("slots = %+v (only a string output is a slot)", d.slots)
	}
	out, err := d.apply(map[int]string{0: "tiny"})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeBody(t, out)
	items := got["input"].([]any)
	if items[3].(map[string]any)["output"] != "tiny" || items[3].(map[string]any)["call_id"] != "c1" {
		t.Errorf("output not written back to its item: %v", items[3])
	}
	if items[1].(map[string]any)["id"] != "rs_1" {
		t.Errorf("reasoning item lost: %v", items[1])
	}
	if got["previous_response_id"] != "resp_1" || got["store"] != true {
		t.Errorf("continuation fields lost: %v", got)
	}
}

func TestResponsesStringInputHasNothingToCompress(t *testing.T) {
	if _, err := parseDocument(ProtocolResponses, []byte(`{"model":"m","input":"just text"}`)); err == nil {
		t.Fatal("a plain-string input has no items and must not parse as a document")
	}
}

// extract is the guard between an engine's reply and the client's request. Every
// way a reply can disagree with what was sent must be refused.
func TestExtractRefusesAnyStructuralDisagreement(t *testing.T) {
	body := claudeBody(
		claudeTurn{"Bash", "toolu_1", big("a", 50), false},
		claudeTurn{"Bash", "toolu_2", big("b", 50), false},
	)
	d, err := parseDocument(ProtocolClaude, body)
	if err != nil {
		t.Fatal(err)
	}
	fresh := func() []any { return deepCopy(d.view).([]any) }
	block := func(r []any, msg int) map[string]any {
		return r[msg].(map[string]any)["content"].([]any)[0].(map[string]any)
	}

	good := fresh()
	block(good, 2)["content"] = "S1"
	block(good, 4)["content"] = "S2"
	texts, err := d.extract(good, []int{0, 1})
	if err != nil || texts[0] != "S1" || texts[1] != "S2" {
		t.Fatalf("valid reply refused or misread: %v %v", texts, err)
	}

	cases := map[string]func(r []any) []any{
		"message dropped":    func(r []any) []any { return r[:len(r)-1] },
		"message added":      func(r []any) []any { return append(r, r[0]) },
		"messages reordered": func(r []any) []any { r[1], r[2] = r[2], r[1]; return r },
		"role changed":       func(r []any) []any { r[1].(map[string]any)["role"] = "user"; return r },
		"tool id changed":    func(r []any) []any { block(r, 2)["tool_use_id"] = "toolu_other"; return r },
		"block type changed": func(r []any) []any { block(r, 2)["type"] = "text"; return r },
		"slot not a string":  func(r []any) []any { block(r, 2)["content"] = 42; return r },
		"slot removed":       func(r []any) []any { delete(block(r, 2), "content"); return r },
		"content flattened":  func(r []any) []any { r[2].(map[string]any)["content"] = "text"; return r },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := d.extract(mutate(fresh()), []int{0, 1}); !errors.Is(err, ErrStructure) {
				t.Fatalf("err = %v, want ErrStructure", err)
			}
		})
	}
}

// The engine may rewrite any text it likes OUTSIDE the slots; none of it may
// reach the client's request, because only slot text is ever read back.
func TestEngineChangesOutsideSlotsAreIgnored(t *testing.T) {
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 50), false})
	d, _ := parseDocument(ProtocolClaude, body)
	reply := deepCopy(d.view).([]any)
	reply[0].(map[string]any)["content"] = "ENGINE REWROTE THE USER MESSAGE"
	reply[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = "S"

	texts, err := d.extract(reply, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.apply(map[int]string{0: texts[0]})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "ENGINE REWROTE") {
		t.Fatal("engine-authored text outside a slot leaked into the request")
	}
}

func TestAdoptableRules(t *testing.T) {
	old := strings.Repeat("a", 100)
	tests := []struct {
		name, repl string
		want       bool
	}{
		{"shorter", "short", true},
		{"identical", old, false},
		{"longer", old + "b", false},
		{"empty", "", false},
		{"invalid utf8", "bad\xff\xfe", false},
	}
	for _, tt := range tests {
		if got := adoptable(old, tt.repl); got != tt.want {
			t.Errorf("%s: adoptable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestExactBytesToolNames(t *testing.T) {
	for _, n := range []string{"Read", "Edit", "Write", "MultiEdit", "NotebookEdit", "read_file", "mcp__fs__read_file", "str_replace_based_edit_tool", "READ"} {
		if !exactBytesTool(n) {
			t.Errorf("%q must be protected", n)
		}
	}
	for _, n := range []string{"", "Bash", "Grep", "Glob", "WebFetch", "run_tests", "readme_summarizer"} {
		if exactBytesTool(n) {
			t.Errorf("%q must be compressible", n)
		}
	}
}

func TestMightHaveToolResultsIsAcheapNegativeFilter(t *testing.T) {
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	for _, p := range []Protocol{ProtocolClaude, ProtocolOpenAI, ProtocolResponses} {
		if mightHaveToolResults(p, plain) {
			t.Errorf("%s: a plain chat must be rejected before parsing", p)
		}
	}
	if !mightHaveToolResults(ProtocolClaude, claudeBody(claudeTurn{"Bash", "t", "x", false})) {
		t.Error("claude body with a tool_result was filtered out")
	}
}
