package tokenopt

import (
	"encoding/json"
	"fmt"
	"strings"
)

// System-prompt injection for requests that are relayed as raw JSON (external
// providers). Only the system field changes; every other byte of the client's
// request is carried over. The Kiro-native path does not use this: there the
// translator appends the block after its own prompt filters (see proxy).
//
// Text is always APPENDED (or the field created), never prepended, so the
// client's own system text, and any cache_control breakpoint on it, keeps its
// position and the provider's prefix cache stays valid up to that point. The
// appended text is identical on every request for a given setting, so it adds
// a stable suffix rather than churn.

const blockSep = "\n\n"

// systemMessageRoles are the roles OpenAI-style APIs use for instructions.
func isSystemRole(role string) bool { return role == "system" || role == "developer" }

// SystemText returns the instruction text already present in a raw body, used
// to detect that a client has Caveman or Ponytail installed. It is best-effort:
// an unparseable body yields "".
func SystemText(proto Protocol, body []byte) string {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return ""
	}
	switch proto {
	case ProtocolClaude:
		return textOfRaw(top["system"])
	case ProtocolResponses:
		var sb strings.Builder
		sb.WriteString(textOfRaw(top["instructions"]))
		var items []map[string]json.RawMessage
		if json.Unmarshal(top["input"], &items) == nil {
			for _, it := range items {
				if isSystemRole(strings.Trim(string(it["role"]), `"`)) {
					sb.WriteString("\n" + textOfRaw(it["content"]))
				}
			}
		}
		return sb.String()
	default:
		var msgs []map[string]json.RawMessage
		if json.Unmarshal(top["messages"], &msgs) != nil {
			return ""
		}
		var sb strings.Builder
		for _, m := range msgs {
			if isSystemRole(strings.Trim(string(m["role"]), `"`)) {
				sb.WriteString(textOfRaw(m["content"]) + "\n")
			}
		}
		return sb.String()
	}
}

// textOfRaw flattens a JSON string or an array of {text} parts to plain text.
func textOfRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text + "\n")
		}
		return sb.String()
	}
	return ""
}

// InjectSystem appends text to the system prompt of a raw request body and
// returns the new body. On any problem it returns the original body and the
// error, so the caller sends the request exactly as the client wrote it.
func InjectSystem(proto Protocol, body []byte, text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return body, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, err
	}
	var err error
	switch proto {
	case ProtocolClaude:
		err = injectClaude(top, text)
	case ProtocolResponses:
		err = injectResponses(top, text)
	case ProtocolOpenAI:
		err = injectOpenAI(top, text)
	default:
		err = fmt.Errorf("unsupported protocol %q", proto)
	}
	if err != nil {
		return body, err
	}
	out, err := encodeJSON(top)
	if err != nil {
		return body, err
	}
	return out, nil
}

func mustJSON(v any) json.RawMessage {
	b, err := encodeJSON(v)
	if err != nil {
		panic(err) // v is always a string or a small literal map here
	}
	return b
}

// appendToContent appends text to a JSON content value: a string is extended, a
// parts array gets one more text part, anything else is replaced by the text.
func appendToContent(content json.RawMessage, text string, partType string) json.RawMessage {
	var s string
	if len(content) > 0 && json.Unmarshal(content, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return mustJSON(text)
		}
		return mustJSON(s + blockSep + text)
	}
	var parts []json.RawMessage
	if len(content) > 0 && json.Unmarshal(content, &parts) == nil {
		parts = append(parts, mustJSON(map[string]any{"type": partType, "text": text}))
		return joinRaw(parts)
	}
	return mustJSON(text)
}

func injectClaude(top map[string]json.RawMessage, text string) error {
	top["system"] = appendToContent(top["system"], text, "text")
	return nil
}

func injectResponses(top map[string]json.RawMessage, text string) error {
	var s string
	if raw := top["instructions"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("instructions is not a string")
		}
	}
	if strings.TrimSpace(s) == "" {
		top["instructions"] = mustJSON(text)
	} else {
		top["instructions"] = mustJSON(s + blockSep + text)
	}
	return nil
}

func injectOpenAI(top map[string]json.RawMessage, text string) error {
	var msgs []json.RawMessage
	if err := json.Unmarshal(top["messages"], &msgs); err != nil {
		return errNoMessages
	}
	for i, raw := range msgs {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || !isSystemRole(strings.Trim(string(m["role"]), `"`)) {
			continue
		}
		// Extend the first system/developer message in place.
		m["content"] = appendToContent(m["content"], text, "text")
		out, err := encodeJSON(m)
		if err != nil {
			return err
		}
		msgs[i] = out
		top["messages"] = joinRaw(msgs)
		return nil
	}
	// No instructions at all: add one at the front.
	sys := mustJSON(map[string]any{"role": "system", "content": text})
	top["messages"] = joinRaw(append([]json.RawMessage{sys}, msgs...))
	return nil
}
