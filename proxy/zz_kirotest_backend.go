//go:build kirotest

// Package proxy — kirotest fake backend.
//
// This file is compiled ONLY under `-tags kirotest`. It is never part of a
// normal `go build ./...` artifact, so it adds zero attack surface (no env
// hook, no open port, no way to redirect the proxy at an arbitrary host) to a
// shipped binary. It exists purely so a locally-run server can be driven
// end-to-end without touching real AWS/Kiro or any real credential.
//
// It installs kiroTransportWrapper (a nil-in-normal-builds seam in kiro.go) so
// that EVERY Kiro HTTP client built by InitKiroHttpClient — including the one
// rebuilt at runtime by NewHandler → applyProxyConfig — routes through an
// in-process RoundTripper that synthesizes responses:
//   - streaming (generateAssistantResponse / SendMessage): a scripted AWS
//     event-stream reproducing "Case A" — upstream in=1200, out=300,
//     credit=1.25, contextUsage=4%. With a 1M-window model that 4% becomes a
//     40000-token context-occupancy override, exactly the legacy client number.
//   - REST (getUsageLimits / ListAvailableModels / GetUserInfo / profiles): one
//     benign 200 JSON body (union of every decoded shape) so the background
//     refresh/models loops keep the fake account ACTIVE instead of banning it.
//
// Using the wrapper seam rather than storing clients in init() is what makes
// this robust: init() runs before NewHandler's runtime InitKiroHttpClient call,
// so an init-stored client would be clobbered; a wrapper is reapplied on every
// InitKiroHttpClient invocation.
package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
)

func init() {
	kiroTransportWrapper = func(http.RoundTripper) http.RoundTripper {
		return ktRoundTripper{}
	}
	// Reapply now so the client built by kiro.go's own init() (which ran before
	// this one and therefore saw a nil wrapper) also gets wrapped.
	InitKiroHttpClient("")
}

type ktRoundTripper struct{}

func (ktRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	if strings.Contains(path, "generateAssistantResponse") || strings.Contains(path, "SendMessage") {
		var buf bytes.Buffer
		// Assistant text.
		buf.Write(ktEventFrame("assistantResponseEvent", map[string]interface{}{
			"content": "Hello from the kirotest fake backend.",
		}))
		// Real upstream token usage: input=1200, output=300 (the ACCURATE numbers
		// internal accounting should record post-Phase-A).
		buf.Write(ktEventFrame("usageEvent", map[string]interface{}{
			"usage": map[string]interface{}{
				"inputTokens":  1200,
				"outputTokens": 300,
			},
		}))
		// Billed credit — flows straight to meteringEvent.usage, unchanged by Phase A.
		buf.Write(ktEventFrame("meteringEvent", map[string]interface{}{
			"usage": 1.25,
		}))
		// Context occupancy 4%. On a 1M-window model this becomes the legacy
		// client-visible input number (40000), the bug Phase A leaves visible in
		// legacy mode but stops feeding into internal accounting.
		buf.Write(ktEventFrame("contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 4.0,
		}))
		// Terminal metadataEvent. Without it the stream-integrity classifier
		// (classifyStreamIntegrity) reads a stop-reason-less stream as truncated
		// and every fixture-backed turn fails as errUpstreamTruncatedResponse.
		buf.Write(ktEventFrame("metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
		return &http.Response{
			StatusCode: 200,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
			Body:       io.NopCloser(&buf),
			Request:    req,
		}, nil
	}
	// REST refresh calls (getUsageLimits / ListAvailableModels / GetUserInfo /
	// ListAvailableProfiles). Return one 200 JSON body whose fields are the union
	// of every response shape those calls decode; each decoder ignores the fields
	// it does not know. A benign 200 keeps the fake account ACTIVE — a non-200
	// would trip the background refresh/models loops into banning it, emptying the
	// pool. Usage limit is left effectively unlimited so the account is never
	// quota-blocked, and the model list carries the models a test may request.
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(ktRestBody)),
		Request:    req,
	}, nil
}

// ktRestBody is the union JSON body returned for every REST refresh call.
const ktRestBody = `{
  "userInfo": {"email": "kirotest@example.invalid", "userId": "kirotest-user"},
  "subscriptionInfo": {"subscriptionTitle": "Kiro Pro (kirotest)", "subscriptionName": "PRO"},
  "nextDateReset": 0,
  "usageBreakdownList": [],
  "models": [
    {"modelId": "claude-sonnet-4.5"},
    {"modelId": "claude-sonnet-4.6"},
    {"modelId": "claude-opus-4.8"},
    {"modelId": "gpt-4o"}
  ]
}`

// ktEventFrame builds one AWS event-stream frame the same way the test helper
// awsEventStreamFrame does: prelude + one :event-type string header + JSON
// payload + trailing message CRC.
//
// Both CRCs must be real. parseEventStreamTracked validates the prelude CRC and
// the message CRC and rejects a mismatch as errInvalidKiroEventStream, so a
// zero-filled checksum would make every scripted frame look like a corrupt
// stream instead of exercising the decode path.
func ktEventFrame(eventType string, payload map[string]interface{}) []byte {
	payloadBytes, _ := json.Marshal(payload)

	headerName := ":event-type"
	headerValue := []byte(eventType)
	headers := make([]byte, 0, 1+len(headerName)+1+2+len(headerValue))
	headers = append(headers, byte(len(headerName)))
	headers = append(headers, []byte(headerName)...)
	headers = append(headers, byte(7)) // value type 7 = UTF-8 string
	headers = append(headers, byte(len(headerValue)>>8), byte(len(headerValue)))
	headers = append(headers, headerValue...)

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}
