package proxy

import (
	"regexp"
	"strings"
)

// Redaction guards for the memory sidecar. These are applied inside a provider's
// Add (see Mem0HTTPProvider.Add) so they cannot be bypassed by the caller —
// including in "automatic" write mode. The policy is intentionally conservative:
// when in doubt, drop the candidate rather than risk persisting a secret or
// private source into long-term memory.

// diffHeaderRe matches a unified-diff / git-diff header, a strong signal the
// content is a source dump rather than a durable fact.
var diffHeaderRe = regexp.MustCompile(`(?m)^(diff --git |@@ -\d|--- a/|\+\+\+ b/)`)

// envAssignRe matches KEY=VALUE lines typical of a .env dump (uppercase key,
// value present). A couple of these in one message means it is a config/secret
// dump, not a fact worth remembering.
var envAssignRe = regexp.MustCompile(`(?m)^[A-Z][A-Z0-9_]{2,}=.+`)

// applyMemoryRedaction returns the messages that are safe to persist under the
// given policy. It (1) masks obvious credentials in every message when
// redactSecrets is set, and (2) drops any message that looks like a source dump
// when storeSourceCode is false. A message reduced to empty is dropped. The
// result may be empty, in which case the caller should skip the write entirely.
func applyMemoryRedaction(msgs []MemoryMessage, redactSecrets, storeSourceCode bool) []MemoryMessage {
	out := make([]MemoryMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		if redactSecrets {
			content = maskSecrets(content)
		}
		if !storeSourceCode && looksLikeSourceDump(content) {
			// Drop the whole message: a partial scrub of source is unreliable and
			// low-value as a memory anyway.
			continue
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		out = append(out, MemoryMessage{Role: m.Role, Content: content})
	}
	return out
}

// looksLikeSourceDump reports whether content is likely private source code,
// a diff, or a config/secret file — the classes of content that must not enter
// long-term memory when storeSourceCode is disabled. Signals (any one trips it):
//   - a git/unified diff header,
//   - a large fenced code block (>= 200 chars between fences),
//   - two or more KEY=VALUE lines (a .env-style dump).
func looksLikeSourceDump(s string) bool {
	if diffHeaderRe.MatchString(s) {
		return true
	}
	if largeFencedBlock(s) {
		return true
	}
	if len(envAssignRe.FindAllString(s, 2)) >= 2 {
		return true
	}
	return false
}

// largeFencedBlock reports whether s contains a Markdown fenced code block whose
// body is at least 200 characters — a heuristic for a real source paste rather
// than a short inline snippet.
func largeFencedBlock(s string) bool {
	const minBody = 200
	idx := strings.Index(s, "```")
	for idx >= 0 {
		rest := s[idx+3:]
		end := strings.Index(rest, "```")
		if end < 0 {
			return false // unterminated fence — treat as not a clean code block
		}
		if end >= minBody {
			return true
		}
		// Advance past this closing fence and keep scanning.
		next := strings.Index(rest[end+3:], "```")
		if next < 0 {
			return false
		}
		idx = idx + 3 + end + 3 + next
	}
	return false
}
