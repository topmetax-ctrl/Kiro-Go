package providererr

import (
	"bytes"
	"encoding/json"
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
		Code      string          `json:"code"`
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		RequestId string          `json:"requestId"`
	}
	if json.Unmarshal(trimmed, &env) == nil {
		out := ParsedBody{
			Code:      strings.TrimSpace(env.Code),
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
					Message   string `json:"message"`
					Type      string `json:"type"`
					Code      string `json:"code"`
					RequestID string `json:"request_id"`
				}
				if json.Unmarshal(env.Error, &asObject) == nil {
					if out.Message == "" {
						out.Message = strings.TrimSpace(asObject.Message)
					}
					if out.Code == "" {
						out.Code = strings.TrimSpace(asObject.Code)
					}
					if out.Code == "" {
						out.Code = strings.TrimSpace(asObject.Type)
					}
					if out.RequestID == "" {
						out.RequestID = strings.TrimSpace(asObject.RequestID)
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
	n := IncRedactedIfChanged(s, Redact(s))
	redacted := Redact(s)
	if len(redacted) > MaxDetailBytes {
		return boundedText{Text: redacted[:MaxDetailBytes], Truncated: true}, n
	}
	return boundedText{Text: redacted, Truncated: len(s) > MaxDetailBytes || len(s) > MaxParseBytes}, n
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
