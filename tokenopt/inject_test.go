package tokenopt

import (
	"reflect"
	"strings"
	"testing"
)

func TestInjectClaudeSystemShapes(t *testing.T) {
	const add = "BLOCK"
	cases := []struct {
		name, body string
		check      func(t *testing.T, sys any)
	}{
		{"absent", `{"model":"m","messages":[]}`, func(t *testing.T, s any) {
			if s != add {
				t.Fatalf("system = %v", s)
			}
		}},
		{"null", `{"model":"m","system":null,"messages":[]}`, func(t *testing.T, s any) {
			if s != add {
				t.Fatalf("system = %v", s)
			}
		}},
		{"string appended, never prepended", `{"model":"m","system":"mine","messages":[]}`, func(t *testing.T, s any) {
			if s != "mine\n\n"+add {
				t.Fatalf("system = %q", s)
			}
		}},
		{"blocks keep their cache_control; one part is added after", `{"model":"m","system":[{"type":"text","text":"mine","cache_control":{"type":"ephemeral"}}],"messages":[]}`, func(t *testing.T, s any) {
			parts := s.([]any)
			if len(parts) != 2 {
				t.Fatalf("parts = %v", parts)
			}
			first := parts[0].(map[string]any)
			if first["text"] != "mine" || first["cache_control"] == nil {
				t.Fatalf("client's own block was disturbed: %v", first)
			}
			last := parts[1].(map[string]any)
			if last["text"] != add || last["cache_control"] != nil {
				t.Fatalf("appended block wrong: %v", last)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := InjectSystem(ProtocolClaude, []byte(c.body), add)
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, decodeBody(t, out)["system"])
		})
	}
}

func TestInjectKeepsEveryOtherFieldIntact(t *testing.T) {
	body := claudeBody(claudeTurn{"Bash", "toolu_1", big("a", 5), false})
	out, err := InjectSystem(ProtocolClaude, body, "BLOCK")
	if err != nil {
		t.Fatal(err)
	}
	want := decodeBody(t, body)
	want["system"] = "You are helpful.\n\nBLOCK"
	if got := decodeBody(t, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("injection changed more than system:\n got %v\nwant %v", got, want)
	}
}

func TestInjectOpenAI(t *testing.T) {
	const add = "BLOCK"
	t.Run("extends the first system message", func(t *testing.T) {
		out, err := InjectSystem(ProtocolOpenAI, []byte(`{"model":"m","seed":3,"messages":[{"role":"system","content":"mine"},{"role":"user","content":"hi"}]}`), add)
		if err != nil {
			t.Fatal(err)
		}
		got := decodeBody(t, out)
		msgs := got["messages"].([]any)
		if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "mine\n\n"+add {
			t.Fatalf("messages = %v", msgs)
		}
		if got["seed"].(interface{ String() string }).String() != "3" {
			t.Fatalf("unknown field lost")
		}
	})
	t.Run("developer role counts as the system message", func(t *testing.T) {
		out, _ := InjectSystem(ProtocolOpenAI, []byte(`{"messages":[{"role":"developer","content":[{"type":"text","text":"mine"}]},{"role":"user","content":"hi"}]}`), add)
		parts := decodeBody(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if len(parts) != 2 || parts[1].(map[string]any)["text"] != add {
			t.Fatalf("parts = %v", parts)
		}
	})
	t.Run("no system message: one is inserted at the front", func(t *testing.T) {
		out, _ := InjectSystem(ProtocolOpenAI, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), add)
		msgs := decodeBody(t, out)["messages"].([]any)
		if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != add {
			t.Fatalf("messages = %v", msgs)
		}
	})
}

func TestInjectResponsesInstructions(t *testing.T) {
	const add = "BLOCK"
	cases := map[string]string{
		`{"model":"m","input":"x"}`:                           add,
		`{"model":"m","instructions":null,"input":"x"}`:       add,
		`{"model":"m","instructions":"mine","input":"x"}`:     "mine\n\n" + add,
		`{"model":"m","instructions":"  ","input":"x"}`:       add,
		`{"model":"m","previous_response_id":"r","input":[]}`: add,
	}
	for body, want := range cases {
		out, err := InjectSystem(ProtocolResponses, []byte(body), add)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		got := decodeBody(t, out)
		if got["instructions"] != want {
			t.Errorf("%s: instructions = %v, want %q", body, got["instructions"], want)
		}
		if strings.Contains(body, "previous_response_id") && got["previous_response_id"] != "r" {
			t.Errorf("continuation id lost")
		}
	}
}

// Whatever goes wrong, the caller must be able to send the client's own bytes.
func TestInjectFailureReturnsTheOriginalBody(t *testing.T) {
	bad := []byte(`{"model":"m","instructions":42,"input":"x"}`)
	out, err := InjectSystem(ProtocolResponses, bad, "BLOCK")
	if err == nil || string(out) != string(bad) {
		t.Fatalf("want error and untouched body, got %v / %s", err, out)
	}
	notJSON := []byte(`not json`)
	if out, err = InjectSystem(ProtocolClaude, notJSON, "BLOCK"); err == nil || string(out) != string(notJSON) {
		t.Fatalf("want error and untouched body, got %v / %s", err, out)
	}
	if out, err = InjectSystem(ProtocolClaude, []byte(`{"messages":[]}`), "  \n"); err != nil || string(out) != `{"messages":[]}` {
		t.Fatalf("blank text must be a no-op, got %v / %s", err, out)
	}
	if _, err = InjectSystem(ProtocolOpenAI, []byte(`{"model":"m"}`), "BLOCK"); err == nil {
		t.Fatal("openai body without messages must error")
	}
}

func TestSystemTextForDedup(t *testing.T) {
	if got := SystemText(ProtocolClaude, []byte(`{"system":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]}`)); !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Errorf("claude blocks = %q", got)
	}
	if got := SystemText(ProtocolOpenAI, []byte(`{"messages":[{"role":"system","content":"sysA"},{"role":"user","content":"u"},{"role":"developer","content":"devB"}]}`)); !strings.Contains(got, "sysA") || !strings.Contains(got, "devB") || strings.Contains(got, "u\n") {
		t.Errorf("openai = %q", got)
	}
	if got := SystemText(ProtocolResponses, []byte(`{"instructions":"insC","input":[{"role":"developer","content":"devD"},{"role":"user","content":"u"}]}`)); !strings.Contains(got, "insC") || !strings.Contains(got, "devD") {
		t.Errorf("responses = %q", got)
	}
	if SystemText(ProtocolClaude, []byte(`garbage`)) != "" {
		t.Error("unparseable body must yield empty text")
	}
}
