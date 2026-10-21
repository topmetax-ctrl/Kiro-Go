package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

const (
	displayUpdatesBeta = "thinking-display-updates-2026-08-18"
	otherBetas         = "claude-code-20250219,effort-2025-11-24"
)

// claudeBodyWithThinking builds a compact request body. Members the normalizer
// must carry through untouched (HTML characters, an unknown field, a number
// beyond float64 precision) are included on purpose.
func claudeBodyWithThinking(model, thinkingJSON string) string {
	thinking := ""
	if thinkingJSON != "" {
		thinking = `"thinking":` + thinkingJSON + `,`
	}
	return `{"model":"` + model + `",` + thinking +
		`"max_tokens":4096,"x_unknown":{"a":[1,2,3],"big":12345678901234567890},` +
		`"messages":[{"role":"user","content":"<system-reminder>a & b</system-reminder>"}]}`
}

func parseClaudeRequestForTest(t *testing.T, body, beta string) (*http.Request, ClaudeRequest) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	if beta != "" {
		r.Header.Set("Anthropic-Beta", beta)
	}
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	return r, req
}

func TestDropThinkingDisplayUpdates(t *testing.T) {
	tests := []struct {
		name         string
		thinking     string
		beta         string
		wantType     string
		wantBudget   int
		wantBeta     string
		wantBetaGone bool
	}{
		{name: "adaptive", thinking: `{"type":"adaptive","display":"updates"}`,
			beta: "claude-code-20250219," + displayUpdatesBeta + ",effort-2025-11-24", wantType: "adaptive", wantBeta: otherBetas},
		{name: "enabled keeps its budget", thinking: `{"type":"enabled","budget_tokens":2048,"display":"updates"}`,
			beta: displayUpdatesBeta + "," + otherBetas, wantType: "enabled", wantBudget: 2048, wantBeta: otherBetas},
		{name: "case and spacing are tolerated", thinking: `{"type":"adaptive","display":" Updates "}`,
			beta: otherBetas + "," + displayUpdatesBeta, wantType: "adaptive", wantBeta: otherBetas},
		{name: "a later dated flag of the same family is dropped too", thinking: `{"type":"adaptive","display":"updates"}`,
			beta: otherBetas + ",thinking-display-updates-2027-01-01", wantType: "adaptive", wantBeta: otherBetas},
		{name: "header disappears when the flag was the only one", thinking: `{"type":"adaptive","display":"updates"}`,
			beta: displayUpdatesBeta, wantType: "adaptive", wantBetaGone: true},
		{name: "works without any beta header", thinking: `{"type":"adaptive","display":"updates"}`,
			wantType: "adaptive", wantBetaGone: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := claudeBodyWithThinking("claude-sonnet-4.5", tc.thinking)
			r, req := parseClaudeRequestForTest(t, body, tc.beta)

			out := dropThinkingDisplayUpdates(r, []byte(body), &req)

			if req.Thinking == nil || req.Thinking.Display != "" || req.Thinking.Type != tc.wantType || req.Thinking.BudgetTokens != tc.wantBudget {
				t.Fatalf("parsed thinking = %+v, want type=%q budget=%d and no display", req.Thinking, tc.wantType, tc.wantBudget)
			}
			var raw struct {
				Thinking map[string]interface{} `json:"thinking"`
			}
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatalf("normalized body is not valid JSON: %v\n%s", err, out)
			}
			if _, has := raw.Thinking["display"]; has {
				t.Fatalf("raw body still carries thinking.display: %s", out)
			}
			if raw.Thinking["type"] != tc.wantType {
				t.Fatalf("raw thinking.type = %v, want %s", raw.Thinking["type"], tc.wantType)
			}
			// Everything outside thinking.display must survive byte-for-byte.
			for _, want := range []string{
				`"x_unknown":{"a":[1,2,3],"big":12345678901234567890}`,
				`"content":"<system-reminder>a & b</system-reminder>"`,
				`"max_tokens":4096`,
			} {
				if !bytes.Contains(out, []byte(want)) {
					t.Fatalf("normalized body lost %s:\n%s", want, out)
				}
			}
			if tc.wantBetaGone {
				if got := r.Header.Get("Anthropic-Beta"); got != "" {
					t.Fatalf("anthropic-beta = %q, want it removed", got)
				}
			} else if got := r.Header.Get("Anthropic-Beta"); got != tc.wantBeta {
				t.Fatalf("anthropic-beta = %q, want %q", got, tc.wantBeta)
			}
			if msg := validateClaudeRequestShape(&req); msg != "" {
				t.Fatalf("normalized request must validate, got %q", msg)
			}
		})
	}
}

// Requests the normalizer has no business touching: the same body bytes, the same
// header, and the same validation verdict as before.
func TestDropThinkingDisplayUpdatesLeavesOtherRequestsAlone(t *testing.T) {
	headerWithFlag := otherBetas + "," + displayUpdatesBeta
	tests := []struct {
		name     string
		thinking string
		beta     string
		wantErr  string
	}{
		{name: "summarized", thinking: `{"type":"adaptive","display":"summarized"}`, beta: otherBetas},
		{name: "omitted", thinking: `{"type":"adaptive","display":"omitted"}`, beta: otherBetas},
		{name: "adaptive without display", thinking: `{"type":"adaptive"}`, beta: otherBetas},
		{name: "enabled without display", thinking: `{"type":"enabled","budget_tokens":2048}`, beta: otherBetas},
		{name: "no thinking at all", thinking: ``, beta: otherBetas},
		{name: "flag without the display value is not this normalizer's business", thinking: `{"type":"adaptive","display":"omitted"}`, beta: headerWithFlag},
		{name: "unknown value is still rejected", thinking: `{"type":"adaptive","display":"full"}`, beta: otherBetas,
			wantErr: "thinking.display must be one of: summarized, omitted"},
		{name: "disabled with updates is still rejected", thinking: `{"type":"disabled","display":"updates"}`, beta: otherBetas,
			wantErr: "thinking.display must be one of: summarized, omitted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := claudeBodyWithThinking("claude-sonnet-4.5", tc.thinking)
			r, req := parseClaudeRequestForTest(t, body, tc.beta)
			before := req.Thinking

			out := dropThinkingDisplayUpdates(r, []byte(body), &req)

			if string(out) != body {
				t.Fatalf("body was rewritten:\n got %s\nwant %s", out, body)
			}
			if got := r.Header.Get("Anthropic-Beta"); got != tc.beta {
				t.Fatalf("anthropic-beta = %q, want %q", got, tc.beta)
			}
			if req.Thinking != before || (before != nil && *req.Thinking != *before) {
				t.Fatalf("parsed thinking changed: %+v", req.Thinking)
			}
			msg := validateClaudeRequestShape(&req)
			if msg != tc.wantErr {
				t.Fatalf("validation = %q, want %q", msg, tc.wantErr)
			}
		})
	}
}

// The forward path relays the raw body and the anthropic-beta header; both must
// reach the provider already normalized.
func TestForwardClaudeRelaysUpdatesRequestWithoutDisplay(t *testing.T) {
	var gotBody []byte
	var gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	setupForwardRoute(t, upstream.URL, "claude-forward", "")

	body := claudeBodyWithThinking("claude-forward", `{"type":"adaptive","display":"updates"}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Anthropic-Beta", "claude-code-20250219,"+displayUpdatesBeta+",effort-2025-11-24")
	rec := httptest.NewRecorder()
	(&Handler{}).handleClaudeMessagesInternal(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	var relayed struct {
		Thinking map[string]interface{} `json:"thinking"`
	}
	if err := json.Unmarshal(gotBody, &relayed); err != nil {
		t.Fatalf("provider got invalid JSON: %v\n%s", err, gotBody)
	}
	if relayed.Thinking["type"] != "adaptive" {
		t.Fatalf("provider thinking = %v, want type adaptive kept", relayed.Thinking)
	}
	if _, has := relayed.Thinking["display"]; has {
		t.Fatalf("provider still received thinking.display: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`"x_unknown":{"a":[1,2,3],"big":12345678901234567890}`)) ||
		!bytes.Contains(gotBody, []byte(`<system-reminder>a & b</system-reminder>`)) {
		t.Fatalf("provider body lost unrelated members:\n%s", gotBody)
	}
	if gotBeta != otherBetas {
		t.Fatalf("provider anthropic-beta = %q, want %q", gotBeta, otherBetas)
	}
}

// summarized / omitted / unset requests reach the provider exactly as sent.
func TestForwardClaudeRelaysOtherDisplayValuesUnchanged(t *testing.T) {
	for _, thinking := range []string{
		`{"type":"adaptive","display":"summarized"}`,
		`{"type":"adaptive","display":"omitted"}`,
		`{"type":"adaptive"}`,
		``,
	} {
		t.Run(thinking, func(t *testing.T) {
			var gotBody []byte
			var gotBeta string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				gotBeta = r.Header.Get("Anthropic-Beta")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer upstream.Close()
			setupForwardRoute(t, upstream.URL, "claude-forward", "")

			body := claudeBodyWithThinking("claude-forward", thinking)
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			r.Header.Set("Anthropic-Beta", otherBetas)
			rec := httptest.NewRecorder()
			(&Handler{}).handleClaudeMessagesInternal(rec, r)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
			}
			if string(gotBody) != body {
				t.Fatalf("provider body differs from the client's:\n got %s\nwant %s", gotBody, body)
			}
			if gotBeta != otherBetas {
				t.Fatalf("provider anthropic-beta = %q, want %q", gotBeta, otherBetas)
			}
		})
	}
}

// newKiroTestHandler returns a handler wired to one account and a fake Kiro
// endpoint that completes every turn normally.
func newKiroTestHandler(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "test-account", Enabled: true, AccessToken: "token-test",
		ProfileArn: "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(swapKiroEndpointsForTest(t, server))

	p := accountpool.GetPool()
	p.Reload()
	return &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
}

func TestKiroNativeAcceptsThinkingDisplayUpdates(t *testing.T) {
	h := newKiroTestHandler(t)

	tests := []struct {
		name       string
		thinking   string
		wantStatus int
		wantInBody string
	}{
		{name: "updates is accepted", thinking: `{"type":"adaptive","display":"updates"}`, wantStatus: http.StatusOK, wantInBody: `"text":"ok"`},
		{name: "omitted is still accepted", thinking: `{"type":"adaptive","display":"omitted"}`, wantStatus: http.StatusOK, wantInBody: `"text":"ok"`},
		{name: "no display is still accepted", thinking: `{"type":"adaptive"}`, wantStatus: http.StatusOK, wantInBody: `"text":"ok"`},
		{name: "an unknown value is still rejected", thinking: `{"type":"adaptive","display":"full"}`,
			wantStatus: http.StatusBadRequest, wantInBody: "thinking.display must be one of: summarized, omitted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
				`{"model":"claude-sonnet-4.5","max_tokens":4096,"thinking":`+tc.thinking+`,"messages":[{"role":"user","content":"hello"}]}`))
			r.Header.Set("Anthropic-Beta", "claude-code-20250219,"+displayUpdatesBeta)
			rec := httptest.NewRecorder()
			h.handleClaudeMessages(rec, r)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantInBody) {
				t.Fatalf("body = %s, want it to contain %q", rec.Body.String(), tc.wantInBody)
			}
		})
	}
}

func TestCountTokensAcceptsThinkingDisplayUpdates(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for display, wantStatus := range map[string]int{"updates": http.StatusOK, "omitted": http.StatusOK, "full": http.StatusBadRequest} {
		t.Run(display, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
				`{"model":"claude-sonnet-4.5","max_tokens":4096,"thinking":{"type":"adaptive","display":"`+display+`"},"messages":[{"role":"user","content":"hello"}]}`))
			rec := httptest.NewRecorder()
			(&Handler{}).handleCountTokens(rec, r)
			if rec.Code != wantStatus {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, wantStatus, rec.Body.String())
			}
		})
	}
}
