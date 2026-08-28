package providererr

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxParseBytes is how much of an upstream error body is read for parsing.
const MaxParseBytes = 64 << 10

// MaxDetailBytes is how much redacted diagnostic text is stored for admin.
const MaxDetailBytes = 16 << 10

// ParsedBody is the structured extract from an upstream error payload.
type ParsedBody struct {
	Code      string
	Message   string
	RequestID string
}

// loose is a JSON scalar that may arrive as a string OR a number, which is what
// upstream error envelopes actually do: several gateways send
// `{"error":{"code":401,...}}` with a numeric code where OpenAI sends
// `"model_not_found"`. Decoding those into a plain string field fails, and
// because ParseBody unmarshals the whole envelope in one pass, ONE numeric field
// used to lose the message and the code together — the parse fell through to the
// plain-text branch and handed the whole raw body back as the "message".
//
// A wrong type on one field must not cost the fields beside it, so each scalar
// decodes independently and a shape nobody predicted degrades to empty rather
// than poisoning the envelope.
type loose struct{ raw json.RawMessage }

func (l *loose) UnmarshalJSON(b []byte) error {
	l.raw = append(l.raw[:0], b...)
	return nil
}

// String renders the scalar as text: a JSON string unquoted, a number as written,
// anything else (object, array, null) as empty — those are not codes or messages,
// and rendering `{...}` into a "code" field would be worse than saying nothing.
func (l loose) String() string {
	trimmed := bytes.TrimSpace(l.raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(trimmed, &n) == nil {
		return n.String()
	}
	var b bool
	if json.Unmarshal(trimmed, &b) == nil {
		return strconv.FormatBool(b)
	}
	return ""
}

type boundedText struct {
	Text      string
	Truncated bool
}

// ParseBody understands OpenAI-style, generic JSON, HTML, and plain text.
// It never returns more than a short message; the raw (redacted) body is
// stored separately via BoundAndRedact.
func ParseBody(body []byte) ParsedBody {
	if len(body) == 0 {
		return ParsedBody{}
	}
	if len(body) > MaxParseBytes {
		body = body[:MaxParseBytes]
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ParsedBody{}
	}

	var env struct {
		Error     json.RawMessage `json:"error"`
		Message   string          `json:"message"`
		Detail    string          `json:"detail"`
		Code      loose           `json:"code"`
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		RequestId string          `json:"requestId"`
	}
	if json.Unmarshal(trimmed, &env) == nil {
		out := ParsedBody{
			Code:      env.Code.String(),
			Message:   strings.TrimSpace(env.Message),
			RequestID: strings.TrimSpace(env.RequestID),
		}
		if out.RequestID == "" {
			out.RequestID = strings.TrimSpace(env.RequestId)
		}
		if out.Message == "" {
			out.Message = strings.TrimSpace(env.Detail)
		}
		if len(env.Error) > 0 {
			var asString string
			if json.Unmarshal(env.Error, &asString) == nil && strings.TrimSpace(asString) != "" {
				if out.Message == "" {
					out.Message = strings.TrimSpace(asString)
				}
			} else {
				var asObject struct {
					Message   loose `json:"message"`
					Type      loose `json:"type"`
					Code      loose `json:"code"`
					RequestID loose `json:"request_id"`
				}
				if json.Unmarshal(env.Error, &asObject) == nil {
					if out.Message == "" {
						out.Message = asObject.Message.String()
					}
					if out.Code == "" {
						out.Code = asObject.Code.String()
					}
					if out.Code == "" {
						out.Code = asObject.Type.String()
					}
					if out.RequestID == "" {
						out.RequestID = asObject.RequestID.String()
					}
				}
			}
		}
		if out.Code == "" {
			out.Code = strings.TrimSpace(env.Type)
		}
		if out.Message != "" || out.Code != "" {
			return out
		}
	}

	s := string(trimmed)
	if looksLikeHTML(s) {
		return ParsedBody{Message: htmlSummary(s)}
	}
	return ParsedBody{Message: truncateRunes(strings.TrimSpace(s), 500)}
}

func looksLikeHTML(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(t, "<!doctype") || strings.HasPrefix(t, "<html") || strings.Contains(t, "<body")
}

func htmlSummary(s string) string {
	lower := strings.ToLower(s)
	for _, title := range []string{"502 bad gateway", "503 service unavailable", "504 gateway timeout", "500 internal server error", "403 forbidden", "401 unauthorized"} {
		if strings.Contains(lower, title) {
			return title
		}
	}
	return "html error page"
}

// BoundAndRedact redacts credentials then caps the diagnostic for storage.
func BoundAndRedact(body []byte) (boundedText, int) {
	s := string(body)
	return BoundAndRedactString(s)
}

func BoundAndRedactString(s string) (boundedText, int) {
	redacted := Redact(s)
	n := IncRedactedIfChanged(s, redacted)
	text, truncated := boundRedacted(redacted, len(s))
	return boundedText{Text: text, Truncated: truncated}, n
}

// boundRedacted caps already-redacted text at MaxDetailBytes. rawLen is the
// length BEFORE redaction, which is what decides "truncated" for a body the
// caller had already clipped at the parse cap.
//
// The cut is moved back off a partial rune: this text is stored and later
// rendered, and a body sliced mid-character shows up as a replacement glyph at
// the end of every long diagnostic.
func boundRedacted(redacted string, rawLen int) (string, bool) {
	if len(redacted) > MaxDetailBytes {
		return trimPartialRune(redacted[:MaxDetailBytes]), true
	}
	return redacted, rawLen > MaxDetailBytes || rawLen > MaxParseBytes
}

func trimPartialRune(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
