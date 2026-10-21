package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// Claude Code can probe a "thinking display updates" mode: together with the beta
// flag `anthropic-beta: thinking-display-updates-<date>` it sends
// thinking.display="updates". The gateway renders no streamed thinking updates and
// relays to providers whose support for the value is unknown, so the value is
// dropped at ingress instead of being rejected. Rejecting it surfaced as a hard
// "400 thinking.display must be one of ..." whenever the client's one-shot
// fallback was already spent. Dropping it is exactly what that fallback does:
// thinking.type, budget_tokens and every other field stay as the client sent them.
const (
	thinkingDisplayUpdates           = "updates"
	thinkingDisplayUpdatesBetaPrefix = "thinking-display-updates-"
)

// dropThinkingDisplayUpdates removes thinking.display="updates" from the parsed
// request AND from the raw body (which the forward path relays verbatim), and
// strips the matching beta flag from the inbound anthropic-beta header (which the
// forward path also relays). It returns the body to use from here on.
//
// Everything else is left byte-for-byte alone: requests with display summarized /
// omitted / unset, requests without thinking, and unrecognized values (which the
// validator must still reject). thinking.type "disabled" is also left alone so
// the validator reports it; the client only ever sends "updates" with adaptive or
// enabled thinking.
func dropThinkingDisplayUpdates(r *http.Request, body []byte, req *ClaudeRequest) []byte {
	thinking := req.Thinking
	if thinking == nil || strings.ToLower(strings.TrimSpace(thinking.Display)) != thinkingDisplayUpdates {
		return body
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "adaptive", "enabled":
	default:
		return body
	}

	thinking.Display = ""
	dropAnthropicBetaFlags(r.Header, thinkingDisplayUpdatesBetaPrefix)
	return removeRawThinkingDisplay(body)
}

// removeRawThinkingDisplay deletes thinking.display from a request body without
// disturbing the rest of it: every other member is carried as raw JSON, so
// messages, tools and unknown fields survive untouched. Only top-level key order
// (sorted) and insignificant whitespace change. The body already parsed as a
// ClaudeRequest, so a failure here is not expected; the original body is returned
// unchanged if it happens.
func removeRawThinkingDisplay(body []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	var thinking map[string]json.RawMessage
	if err := json.Unmarshal(top["thinking"], &thinking); err != nil {
		return body
	}
	delete(thinking, "display")
	top["thinking"] = marshalRawJSON(thinking)
	if top["thinking"] == nil {
		return body
	}
	if out := marshalRawJSON(top); out != nil {
		return out
	}
	return body
}

// marshalRawJSON marshals v without HTML escaping, so "<" ">" "&" inside carried
// raw members (system reminders, tool schemas) are not rewritten as < etc.
// It returns nil on failure.
func marshalRawJSON(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// dropAnthropicBetaFlags removes every anthropic-beta flag starting with prefix
// (case-insensitive) from h, keeping the other flags in their original order. The
// header is left alone when no flag matches, and deleted when nothing else remains.
func dropAnthropicBetaFlags(h http.Header, prefix string) {
	values := h.Values("Anthropic-Beta")
	if len(values) == 0 {
		return
	}
	var kept []string
	dropped := false
	for _, value := range values {
		for _, flag := range strings.Split(value, ",") {
			flag = strings.TrimSpace(flag)
			if flag == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(flag), prefix) {
				dropped = true
				continue
			}
			kept = append(kept, flag)
		}
	}
	if !dropped {
		return
	}
	if len(kept) == 0 {
		h.Del("Anthropic-Beta")
		return
	}
	h.Set("Anthropic-Beta", strings.Join(kept, ","))
}
