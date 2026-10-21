package tokenopt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Protocol identifies the wire format of an inference request body.
type Protocol string

const (
	ProtocolClaude    Protocol = "claude"    // Anthropic /v1/messages
	ProtocolOpenAI    Protocol = "openai"    // OpenAI /v1/chat/completions
	ProtocolResponses Protocol = "responses" // OpenAI /v1/responses
)

// The codec's contract, and the reason it exists instead of a typed round-trip:
//
//   - Kiro-Go owns the request envelope. An engine is only ever asked for
//     replacement TEXT for specific tool-result strings ("slots"). Message order,
//     roles, tool ids, content-block structure, cache_control markers and every
//     field the proxy does not model are taken from the client's own bytes, never
//     from the engine's reply.
//   - Messages that are not edited keep their original bytes. An edited message is
//     re-encoded with only the slot string changed.
//   - An engine reply is accepted only if its message skeleton matches what was
//     sent (same count, same roles, same tool-call ids at every slot). Otherwise
//     the whole result is discarded.

var (
	errNoMessages = errors.New("body has no message array")
	// ErrStructure means the engine reply did not have the shape of what was sent.
	ErrStructure = errors.New("engine reply does not match the request structure")
)

// slot is one replaceable string: the text of a single tool result.
type slot struct {
	text string
	tool string // name of the tool that produced it; "" when the call is not visible

	// Location in the engine-facing view: the message index, the path to the
	// string inside that message, and the object that carries the tool-call id.
	viewMsg  int
	viewPath []any
	idPath   []any
	idKey    string
	id       string
	// blockType, when set, must match the type of the object at idPath in the reply.
	blockType string

	// Location in the client's original array.
	rawIdx  int
	rawPath []any
}

// document is a parsed request body exposing its slots.
type document struct {
	proto Protocol
	top   map[string]json.RawMessage
	field string // "messages" or "input"
	raw   []json.RawMessage
	// view is what the engine sees. For Claude and OpenAI it is the original
	// array; for Responses it is a chat-shaped rendering of the input items.
	view    []any
	viewRaw bool
	slots   []slot
}

func decodeNumber(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep numbers byte-exact instead of squeezing them through float64
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// encodeJSON marshals without HTML escaping, so "<" in code stays "<".
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// mightHaveToolResults is a cheap negative filter run before any parsing, so
// the common request with no tool output costs one scan and no allocation.
func mightHaveToolResults(proto Protocol, body []byte) bool {
	switch proto {
	case ProtocolClaude:
		return bytes.Contains(body, []byte("tool_result"))
	case ProtocolResponses:
		return bytes.Contains(body, []byte("function_call_output"))
	default:
		return bytes.Contains(body, []byte(`"tool"`))
	}
}

func parseDocument(proto Protocol, body []byte) (*document, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	field := "messages"
	if proto == ProtocolResponses {
		field = "input"
	}
	rawField, ok := top[field]
	if !ok {
		return nil, errNoMessages
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(rawField, &raw); err != nil {
		return nil, errNoMessages // e.g. a Responses `input` that is a plain string
	}
	decoded := make([]any, len(raw))
	for i, r := range raw {
		v, err := decodeNumber(r)
		if err != nil {
			return nil, err
		}
		decoded[i] = v
	}
	d := &document{proto: proto, top: top, field: field, raw: raw}
	switch proto {
	case ProtocolClaude:
		d.view, d.viewRaw = decoded, true
		d.findClaude(decoded)
	case ProtocolOpenAI:
		d.view, d.viewRaw = decoded, true
		d.findOpenAI(decoded)
	case ProtocolResponses:
		d.findResponses(decoded)
	default:
		return nil, fmt.Errorf("unsupported protocol %q", proto)
	}
	return d, nil
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asStr(v any) string         { s, _ := v.(string); return s }

func (d *document) findClaude(msgs []any) {
	tools := map[string]string{} // tool_use id -> tool name
	for i, m := range msgs {
		content, _ := asMap(m)["content"].([]any)
		for b, blk := range content {
			bm := asMap(blk)
			switch asStr(bm["type"]) {
			case "tool_use":
				tools[asStr(bm["id"])] = asStr(bm["name"])
			case "tool_result":
				// An error result is a diagnostic the model must read verbatim.
				if isErr, _ := bm["is_error"].(bool); isErr {
					continue
				}
				id := asStr(bm["tool_use_id"])
				base := slot{
					tool: tools[id], viewMsg: i, rawIdx: i,
					idPath: []any{"content", b}, idKey: "tool_use_id", id: id, blockType: "tool_result",
				}
				switch c := bm["content"].(type) {
				case string:
					s := base
					s.text, s.viewPath, s.rawPath = c, []any{"content", b, "content"}, []any{"content", b, "content"}
					d.slots = append(d.slots, s)
				case []any:
					for k, sb := range c {
						if sm := asMap(sb); asStr(sm["type"]) == "text" {
							if txt, ok := sm["text"].(string); ok {
								s := base
								p := []any{"content", b, "content", k, "text"}
								s.text, s.viewPath, s.rawPath = txt, p, p
								d.slots = append(d.slots, s)
							}
						}
					}
				}
			}
		}
	}
}

func (d *document) findOpenAI(msgs []any) {
	tools := map[string]string{}
	for i, m := range msgs {
		mm := asMap(m)
		switch asStr(mm["role"]) {
		case "assistant":
			calls, _ := mm["tool_calls"].([]any)
			for _, c := range calls {
				cm := asMap(c)
				tools[asStr(cm["id"])] = asStr(asMap(cm["function"])["name"])
			}
		case "tool":
			id := asStr(mm["tool_call_id"])
			base := slot{tool: tools[id], viewMsg: i, rawIdx: i, idPath: []any{}, idKey: "tool_call_id", id: id}
			switch c := mm["content"].(type) {
			case string:
				s := base
				s.text, s.viewPath, s.rawPath = c, []any{"content"}, []any{"content"}
				d.slots = append(d.slots, s)
			case []any:
				for k, part := range c {
					if pm := asMap(part); asStr(pm["type"]) == "text" {
						if txt, ok := pm["text"].(string); ok {
							s := base
							p := []any{"content", k, "text"}
							s.text, s.viewPath, s.rawPath = txt, p, p
							d.slots = append(d.slots, s)
						}
					}
				}
			}
		}
	}
}

// textOfParts flattens a Responses content value (string or typed parts) to text.
func textOfParts(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, p := range c {
			pm := asMap(p)
			if t, ok := pm["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

// findResponses renders the Responses input items as chat messages (the shape
// engines understand) and records, per tool output, where it lives in the
// original item. Items with no chat equivalent (reasoning, ...) are left out of
// the view; they are never edited and the rest of the body keeps them as-is.
func (d *document) findResponses(items []any) {
	tools := map[string]string{}
	for j, it := range items {
		im := asMap(it)
		typ := asStr(im["type"])
		if typ == "" && im["role"] != nil {
			typ = "message"
		}
		switch typ {
		case "message":
			d.view = append(d.view, map[string]any{"role": asStr(im["role"]), "content": textOfParts(im["content"])})
		case "function_call":
			id := asStr(im["call_id"])
			tools[id] = asStr(im["name"])
			d.view = append(d.view, map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": asStr(im["name"]), "arguments": asStr(im["arguments"])},
				}},
			})
		case "function_call_output":
			id := asStr(im["call_id"])
			out, isStr := im["output"].(string)
			if !isStr {
				d.view = append(d.view, map[string]any{"role": "tool", "tool_call_id": id, "content": textOfParts(im["output"])})
				continue
			}
			d.view = append(d.view, map[string]any{"role": "tool", "tool_call_id": id, "content": out})
			d.slots = append(d.slots, slot{
				text: out, tool: tools[id], viewMsg: len(d.view) - 1, viewPath: []any{"content"},
				idPath: []any{}, idKey: "tool_call_id", id: id, rawIdx: j, rawPath: []any{"output"},
			})
		}
	}
}

// engineMessages renders the view as the JSON array sent to an engine.
func (d *document) engineMessages() ([]byte, error) {
	if d.viewRaw {
		return joinRaw(d.raw), nil
	}
	return encodeJSON(d.view)
}

func joinRaw(raw []json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, r := range raw {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(r)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func getAt(node any, path []any) (any, bool) {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := node.(map[string]any)
			if !ok {
				return nil, false
			}
			if node, ok = m[k]; !ok {
				return nil, false
			}
		case int:
			a, ok := node.([]any)
			if !ok || k < 0 || k >= len(a) {
				return nil, false
			}
			node = a[k]
		default:
			return nil, false
		}
	}
	return node, true
}

func setAt(node any, path []any, v string) bool {
	if len(path) == 0 {
		return false
	}
	parent, ok := getAt(node, path[:len(path)-1])
	if !ok {
		return false
	}
	switch k := path[len(path)-1].(type) {
	case string:
		if m, ok := parent.(map[string]any); ok {
			m[k] = v
			return true
		}
	case int:
		if a, ok := parent.([]any); ok && k >= 0 && k < len(a) {
			a[k] = v
			return true
		}
	}
	return false
}

// apply writes edits (slot index -> new text) into the client's original body
// and returns the new body. Only the edited messages are re-encoded.
func (d *document) apply(edits map[int]string) ([]byte, error) {
	byMsg := map[int][]int{}
	for si := range edits {
		byMsg[d.slots[si].rawIdx] = append(byMsg[d.slots[si].rawIdx], si)
	}
	raw := append([]json.RawMessage(nil), d.raw...)
	for ri, sis := range byMsg {
		node, err := decodeNumber(raw[ri])
		if err != nil {
			return nil, err
		}
		for _, si := range sis {
			if !setAt(node, d.slots[si].rawPath, edits[si]) {
				return nil, fmt.Errorf("slot %d: path vanished", si)
			}
		}
		if raw[ri], err = encodeJSON(node); err != nil {
			return nil, err
		}
	}
	top := make(map[string]json.RawMessage, len(d.top))
	for k, v := range d.top {
		top[k] = v
	}
	top[d.field] = joinRaw(raw)
	return encodeJSON(top)
}

// extract validates an engine reply against what was sent and returns its
// replacement text for each wanted slot. Anything that does not line up is an
// error: a reply that dropped, reordered or re-labelled messages cannot be
// trusted to have left the tool-call structure alone.
func (d *document) extract(reply []any, want []int) (map[int]string, error) {
	if len(reply) != len(d.view) {
		return nil, fmt.Errorf("%w: %d messages sent, %d returned", ErrStructure, len(d.view), len(reply))
	}
	for i := range d.view {
		if asStr(asMap(d.view[i])["role"]) != asStr(asMap(reply[i])["role"]) {
			return nil, fmt.Errorf("%w: role changed at message %d", ErrStructure, i)
		}
	}
	out := make(map[int]string, len(want))
	for _, si := range want {
		s := d.slots[si]
		idObj, ok := getAt(reply[s.viewMsg], s.idPath)
		if !ok {
			return nil, fmt.Errorf("%w: slot %d container missing", ErrStructure, si)
		}
		if s.id != "" && asStr(asMap(idObj)[s.idKey]) != s.id {
			return nil, fmt.Errorf("%w: tool-call id changed at message %d", ErrStructure, s.viewMsg)
		}
		if s.blockType != "" && asStr(asMap(idObj)["type"]) != s.blockType {
			return nil, fmt.Errorf("%w: block type changed at message %d", ErrStructure, s.viewMsg)
		}
		v, ok := getAt(reply[s.viewMsg], s.viewPath)
		txt, isStr := v.(string)
		if !ok || !isStr {
			return nil, fmt.Errorf("%w: slot %d is no longer a string", ErrStructure, si)
		}
		out[si] = txt
	}
	return out, nil
}

// adoptable decides whether replacement text may be used for a slot. The rules
// are per slot and content-only, so the decision is deterministic: a result
// that is not strictly smaller, is empty, or is not valid UTF-8 is discarded.
func adoptable(old, replacement string) bool {
	return replacement != "" && len(replacement) < len(old) && utf8.ValidString(replacement)
}

// exactBytesTools are tools whose output the model must see byte-for-byte: file
// contents it will later patch, and edit confirmations. Compressing them would
// make a following Edit's old_string fail to match. Matching is by lower-cased
// name with any "mcp__server__" prefix removed.
var exactBytesTools = map[string]bool{
	"read": true, "edit": true, "write": true, "multiedit": true,
	"notebookedit": true, "notebookread": true,
	"read_file": true, "write_file": true, "edit_file": true, "replace_in_file": true,
	"view": true, "open_file": true, "create_file": true, "apply_patch": true,
	"str_replace_editor": true, "str_replace_based_edit_tool": true,
}

func exactBytesTool(name string) bool {
	name = strings.ToLower(name)
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	return exactBytesTools[name]
}

// ExactBytesToolNames lists the protected names, for the admin capability view.
func ExactBytesToolNames() []string {
	names := make([]string, 0, len(exactBytesTools))
	for n := range exactBytesTools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
