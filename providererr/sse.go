package providererr

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Dialect selects the on-wire SSE error shape after a stream has started.
type Dialect int

const (
	DialectClaude Dialect = iota
	DialectOpenAI
	DialectResponses
)

// RewriteSSEFrame replaces a provider error SSE frame with a public one.
// Non-error frames are returned unchanged. Returns rewritten=true when the
// frame was an error event.
func RewriteSSEFrame(frame []byte, pub PublicError, d Dialect) (out []byte, rewritten bool) {
	eventName, data := splitSSE(frame)
	if !isErrorFrame(eventName, data) {
		return frame, false
	}
	return EncodeSSEError(pub, d), true
}

func splitSSE(frame []byte) (event, data string) {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			chunk := strings.TrimSpace(string(line[len("data:"):]))
			if data != "" {
				data += "\n"
			}
			data += chunk
		}
	}
	return event, data
}

func isErrorFrame(event, data string) bool {
	if strings.EqualFold(event, "error") {
		return true
	}
	if data == "" || data == "[DONE]" {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &obj) != nil {
		return false
	}
	if _, ok := obj["error"]; ok {
		if _, hasChoices := obj["choices"]; hasChoices {
			return false
		}
		if t, ok := obj["type"]; ok {
			typ := strings.Trim(string(t), `"`)
			if typ == "content_block_delta" || typ == "message_delta" || typ == "content_block_start" {
				return false
			}
			if typ == "error" || typ == "response.failed" {
				return true
			}
		}
		return true
	}
	if t, ok := obj["type"]; ok {
		typ := strings.Trim(string(t), `"`)
		return typ == "error" || typ == "response.failed"
	}
	return false
}

// EncodeSSEError is the protocol-correct terminal error frame.
func EncodeSSEError(pub PublicError, d Dialect) []byte {
	msg := pub.MessageOrDefault()
	switch d {
	case DialectClaude:
		payload, _ := json.Marshal(map[string]interface{}{
			"type": "error",
			"error": map[string]string{
				"type":    EnvelopeType(pub.Code, true),
				"message": msg,
			},
			"request_id": pub.RequestID,
		})
		return []byte("event: error\ndata: " + string(payload) + "\n\n")
	case DialectResponses:
		payload, _ := json.Marshal(map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"status": "failed",
				"error": map[string]string{
					"type":    EnvelopeType(pub.Code, false),
					"message": msg,
					"code":    pub.Code,
				},
			},
		})
		return []byte("data: " + string(payload) + "\n\n")
	default:
		payload, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message":    msg,
				"type":       EnvelopeType(pub.Code, false),
				"code":       pub.Code,
				"request_id": pub.RequestID,
			},
		})
		return []byte("data: " + string(payload) + "\n\n")
	}
}
