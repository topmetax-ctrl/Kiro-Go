package cachetrace

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strings"

	"kiro-go/metrics"
)

// sessionDomain keys the session digests apart from the content digests: a
// session id and a prompt that happen to hold the same bytes never share a digest.
const sessionDomain = "kiro-go/cache-trace/session/v1"

// Request is what the gateway knows about a request besides its body. The zero
// value is valid: the signals that need more than the body are then recorded as
// not observable and no session digest is made.
type Request struct {
	// Principal scopes the session digests, normally the API key id. The same
	// session id under two principals digests differently, so a digest cannot tie
	// one person's sessions to another's.
	Principal string
	// Ingress is nil when the client's headers were not available.
	Ingress *Ingress
	// Identify reads the body's metadata.user_id, given as the raw JSON value.
	// It is nil when the caller has no session parser. The gateway owns that
	// parser, so this package does not repeat its rules.
	Identify func(userID json.RawMessage) Identity
}

// Ingress is what the client's request headers said.
type Ingress struct {
	// AgentID: X-Claude-Code-Agent-Id or X-Claude-Code-Parent-Agent-Id was sent.
	AgentID bool
	// Beta is the Anthropic-Beta value as it is forwarded upstream.
	Beta string
}

// Identity is what a session parser made of metadata.user_id.
type Identity struct {
	// Shape is a metrics.CacheSignals.UserID value.
	Shape string
	// Parent is a tri-state: does the body name a parent session.
	Parent string
	// Session and ParentSession are the ids to digest; empty means unknown.
	Session, ParentSession string
}

// sessionDigest is the keyed digest of one session id under a principal. Session
// and parent ids both pass through it, so they are comparable by construction.
func (t *Tracer) sessionDigest(principal, id string) string {
	id = canonicalSessionID(id)
	if id == "" || len(t.sessKey) == 0 {
		return ""
	}
	m := hmac.New(sha256.New, t.sessKey)
	var n [8]byte
	for _, p := range []string{principal, id} {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		m.Write(n[:])
		m.Write([]byte(p))
	}
	return short(m.Sum(nil))
}

// canonicalSessionID lower-cases an id in canonical UUID form, so the same
// session spelled in two cases digests once. Any other id is used as sent.
func canonicalSessionID(id string) string {
	if len(id) != 36 {
		return id
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return id
			}
		case !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
			return id
		}
	}
	return strings.ToLower(id)
}

const (
	extendedTTLBeta = "extended-cache-ttl-2025-04-11"
	subagentMarker  = "cc_is_subagent=true"
)

// tri turns a yes/no that was checked in full into a present/absent value.
func tri(found bool) string {
	if found {
		return metrics.SignalPresent
	}
	return metrics.SignalAbsent
}

// signalsOf reads the classification signals. The body-derived ones follow the
// rules CLIProxyAPI v8.0.8 applies to the same request (claude_diagnostics.go),
// so they show what that hop could have seen; they are observations, and which
// of them it acts on is not recorded.
func signalsOf(req *messagesRequest, in Request) (*metrics.CacheSignals, Identity) {
	sig := &metrics.CacheSignals{
		ParentSession:   metrics.SignalNotObservable,
		AgentHeader:     metrics.SignalNotObservable,
		ExtendedTTLBeta: metrics.SignalNotObservable,
	}
	var id Identity
	if in.Identify != nil {
		var meta struct {
			UserID json.RawMessage `json:"user_id"`
		}
		_ = json.Unmarshal(req.Metadata, &meta) // a metadata that is not an object has no user_id
		id = in.Identify(meta.UserID)
		sig.UserID, sig.ParentSession = id.Shape, id.Parent
	}
	if in.Ingress != nil {
		sig.AgentHeader = tri(in.Ingress.AgentID)
		sig.ExtendedTTLBeta = tri(strings.Contains(in.Ingress.Beta, extendedTTLBeta))
	}
	text := systemTexts(req.System)
	sig.SubagentMarker = tri(strings.Contains(text.first, subagentMarker))
	sig.Probe = tri(isProbe(req))
	sig.TitleHelper = tri(isTitleHelper(req, text))
	return sig, id
}

// sysText is the text of a system prompt: the first block, which is all the
// subagent marker is looked for in, and every block.
type sysText struct {
	first string
	all   []string
}

// systemTexts reads a system prompt given as a string or as blocks. A block
// without a string text contributes an empty one.
func systemTexts(raw json.RawMessage) sysText {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return sysText{}
	}
	switch raw[0] {
	case '"':
		s := asString(raw)
		return sysText{first: s, all: []string{s}}
	case '[':
		var blocks []struct {
			Text json.RawMessage `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) != nil {
			return sysText{}
		}
		out := sysText{all: make([]string, len(blocks))}
		for i, b := range blocks {
			out.all[i] = asString(b.Text)
		}
		if len(out.all) > 0 {
			out.first = out.all[0]
		}
		return out
	}
	return sysText{}
}

// asString decodes a JSON string and reads anything else as no text.
func asString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// isProbe is a request of the shape a client sends to test a key: one token
// allowed, no tools, and a single short user message.
func isProbe(req *messagesRequest) bool {
	var max float64
	if json.Unmarshal(req.MaxTokens, &max) != nil || max != 1 {
		return false
	}
	if hasTools(req.Tools) {
		return false
	}
	if len(req.Messages) != 1 {
		return false
	}
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(req.Messages[0], &msg) != nil || msg.Role != "user" {
		return false
	}
	probeText := func(t string) bool { return t == "quota" || t == "test" || t == "." || t == "probe" }
	content := bytes.TrimSpace(msg.Content)
	if len(content) == 0 {
		return false
	}
	switch content[0] {
	case '"':
		return probeText(strings.TrimSpace(asString(content)))
	case '[':
		var parts []struct {
			Text         json.RawMessage `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if json.Unmarshal(content, &parts) != nil {
			return false
		}
		plain, matched := 0, false
		for _, p := range parts {
			t := strings.TrimSpace(asString(p.Text))
			if strings.Contains(t, "<system-reminder>") {
				continue
			}
			plain++
			if probeText(t) || (t == "Hi" && len(p.CacheControl) > 0) {
				matched = true
			}
		}
		return plain == 1 && matched
	}
	return false
}

// hasTools reports a tools field that offers anything: a non-empty array, or any
// value that is not an array (the upstream will reject it, but it is not "none").
func hasTools(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return true
	}
	return len(list) > 0
}

// A request that asks for a session title. With a structured-output schema it
// is one whose only property is "title"; without one, a system prompt that holds
// the instruction. Messages with the role "system" are not read: the Messages
// API rejects them.
var (
	titleInSystem = []string{"naming a coding session", "Return a short title", "Write the title in the predominant language"}
	titleInSchema = append([]string{"<session>"}, titleInSystem...)
)

func isTitleHelper(req *messagesRequest, system sysText) bool {
	var oc struct {
		Format struct {
			Schema struct {
				Properties json.RawMessage `json:"properties"`
			} `json:"schema"`
		} `json:"format"`
	}
	if len(req.OutputConfig) > 0 {
		_ = json.Unmarshal(req.OutputConfig, &oc)
	}
	if props := oc.Format.Schema.Properties; len(props) > 0 {
		var m map[string]json.RawMessage
		if json.Unmarshal(props, &m) != nil || len(m) != 1 {
			return false
		}
		if _, ok := m["title"]; !ok {
			return false
		}
		return anyContains(system.all, titleInSchema) || messagesContain(req.Messages, titleInSchema)
	}
	return anyContains(system.all, titleInSystem)
}

func anyContains(texts, needles []string) bool {
	for _, t := range texts {
		for _, n := range needles {
			if strings.Contains(t, n) {
				return true
			}
		}
	}
	return false
}

// messagesContain looks for a phrase in each message's text: a string content,
// or the text of each content block.
func messagesContain(messages []json.RawMessage, needles []string) bool {
	for _, m := range messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		content := bytes.TrimSpace(msg.Content)
		if len(content) == 0 {
			continue
		}
		switch content[0] {
		case '[':
			var parts []struct {
				Text json.RawMessage `json:"text"`
			}
			if json.Unmarshal(content, &parts) != nil {
				continue
			}
			for _, p := range parts {
				if anyContains([]string{asString(p.Text)}, needles) {
					return true
				}
			}
		case '"':
			if anyContains([]string{asString(content)}, needles) {
				return true
			}
		}
	}
	return false
}
