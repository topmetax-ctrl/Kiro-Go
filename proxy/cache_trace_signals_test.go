package proxy

// The classification signals and session digests on a cache trace: what a body's
// metadata.user_id makes of them, and that recording them leaves the request
// that goes upstream exactly as the client sent it.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"kiro-go/cachetrace"
	"kiro-go/metrics"
)

const (
	testParentSession = "0a1b2c3d-0000-4000-8000-00000000abcd"
	testChildSession  = "9f8e7d6c-0000-4000-8000-00000000dcba"
)

func TestClaudeUserIdentity(t *testing.T) {
	native := "user_" + strings.Repeat("ab", 32) + "_account_" + testParentSession + "_session_" + testChildSession
	envelope := func(extra string) string {
		return `{"device_id":"d","account_uuid":"a","session_id":"` + testChildSession + `"` + extra + `}`
	}
	asString := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	for name, tc := range map[string]struct {
		raw                    string
		shape, parent          string
		session, parentSession string
	}{
		"native":                         {asString(native), "native", "not-observable", testChildSession, ""},
		"envelope without a parent":      {asString(envelope("")), "json", "absent", testChildSession, ""},
		"envelope with a parent":         {asString(envelope(`,"parent_session_id":"` + testParentSession + `"`)), "json", "present", testChildSession, testParentSession},
		"parent key with an empty value": {asString(envelope(`,"parent_session_id":""`)), "json", "present", testChildSession, ""},
		"parent key with null":           {asString(envelope(`,"parent_session_id":null`)), "json", "present", testChildSession, ""},
		"parent id with a space":         {asString(envelope(`,"parent_session_id":"a b"`)), "json", "present", testChildSession, ""},
		"object form":                    {envelope(`,"parent_session_id":"` + testParentSession + `"`), "json", "present", testChildSession, testParentSession},
		"envelope without a session":     {asString(`{"device_id":"d","parent_session_id":"` + testParentSession + `"}`), "json", "present", "", testParentSession},
		"parent key in a broken string":  {asString(`user_x "parent_session_id" y`), "invalid", "present", "", ""},
		"unrecognised text":              {asString("some-other-id"), "invalid", "not-observable", "", ""},
		"almost native":                  {asString("user_x_account_y_session_z"), "invalid", "not-observable", "", ""},
		"empty string":                   {`""`, "none", "not-observable", "", ""},
		"null":                           {`null`, "none", "not-observable", "", ""},
		"missing":                        {``, "none", "not-observable", "", ""},
		"a number":                       {`42`, "invalid", "not-observable", "", ""},
		"an array":                       {`[1]`, "invalid", "not-observable", "", ""},
		"too long":                       {asString(envelope(`,"pad":"` + strings.Repeat("x", maxMetadataUserIDLen) + `"`)), "invalid", "not-observable", "", ""},
		"too long and naming a parent":   {asString(envelope(`,"parent_session_id":"p","pad":"` + strings.Repeat("x", maxMetadataUserIDLen) + `"`)), "invalid", "present", "", ""},
	} {
		got := claudeUserIdentity(json.RawMessage(tc.raw))
		want := cachetrace.Identity{Shape: tc.shape, Parent: tc.parent, Session: tc.session, ParentSession: tc.parentSession}
		if got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
}

type seenRequest struct {
	mu     sync.Mutex
	body   []byte
	header http.Header
}

// forwardSigned forwards body through the trace route as the API key apiKey,
// with the given client headers, and returns what the upstream received.
func forwardSigned(t *testing.T, h *Handler, body, apiKey string, hdr map[string]string) (*seenRequest, metrics.Event) {
	t.Helper()
	metrics.Reset()
	seen := &seenRequest{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		seen.body, _ = io.ReadAll(r.Body)
		seen.header = r.Header.Clone()
		seen.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-sonnet-5-5","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	setupTraceRoute(t, upstream.URL, "")

	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, apiKey))
	if !h.tryForwardUpstream(r, httptest.NewRecorder(), []byte(body), "claude-trace", false, "/messages", true, "") {
		t.Fatal("expected route to match and forward")
	}
	evs, total := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if total != 1 || evs[0].Cache == nil {
		t.Fatalf("want one traced event, got %d: %+v", total, evs)
	}
	return seen, evs[0]
}

// bodyOf is claudeBody with a metadata member, as Claude Code sends it.
func bodyOf(userID string) string {
	b := strings.Replace(claudeBody, "%STREAM%", "false", 1)
	if userID == "" {
		return b
	}
	return strings.Replace(b, `"messages"`, `"metadata":{"user_id":`+userID+`},"messages"`, 1)
}

func envelopeID(session, parent string) string {
	env := map[string]string{"device_id": "d", "account_uuid": "a", "session_id": session}
	if parent != "" {
		env["parent_session_id"] = parent
	}
	b, _ := json.Marshal(env)
	s, _ := json.Marshal(string(b))
	return string(s)
}

func TestForwardRecordsSignalsWithoutChangingTheRequest(t *testing.T) {
	body := bodyOf(envelopeID(testChildSession, testParentSession))
	hdr := map[string]string{
		"X-Claude-Code-Agent-Id": "agent-1",
		"Anthropic-Beta":         "claude-code-20250219,extended-cache-ttl-2025-04-11",
	}
	seen, ev := forwardSigned(t, &Handler{}, body, "key-1", hdr)

	if string(seen.body) != body {
		t.Fatalf("upstream received a different body:\n got %s\nwant %s", seen.body, body)
	}
	if got := seen.header.Get("X-Claude-Code-Agent-Id"); got != "" {
		t.Fatalf("the agent header reached the upstream: %q", got)
	}
	if got := seen.header.Get("Anthropic-Beta"); got != hdr["Anthropic-Beta"] {
		t.Fatalf("Anthropic-Beta = %q: the trace must describe the value that is forwarded", got)
	}

	sig := ev.Cache.Signals
	want := metrics.CacheSignals{
		UserID: "json", ParentSession: "present", SubagentMarker: "absent", AgentHeader: "present",
		Probe: "absent", TitleHelper: "absent", ExtendedTTLBeta: "present",
	}
	if sig == nil || *sig != want {
		t.Fatalf("signals = %+v, want %+v", sig, want)
	}
	if len(ev.Cache.Sess) != 24 || len(ev.Cache.Parent) != 24 || ev.Cache.Sess == ev.Cache.Parent {
		t.Fatalf("session digests = %q / %q", ev.Cache.Sess, ev.Cache.Parent)
	}
	raw, _ := json.Marshal(ev)
	for _, leak := range []string{testParentSession, testChildSession, "agent-1", "device_id"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("event holds %q: %s", leak, raw)
		}
	}
}

// A parent and its child, sent through one key, meet in the events; sent through
// two keys, they do not.
func TestForwardLinksChildToParentWithinOneKey(t *testing.T) {
	h := &Handler{}
	_, parent := forwardSigned(t, h, bodyOf(envelopeID(testParentSession, "")), "key-1", nil)
	_, child := forwardSigned(t, h, bodyOf(envelopeID(testChildSession, testParentSession)), "key-1", nil)
	_, stranger := forwardSigned(t, h, bodyOf(envelopeID(testChildSession, testParentSession)), "key-2", nil)

	if parent.Cache.Sess == "" || child.Cache.Parent != parent.Cache.Sess {
		t.Fatalf("child.Parent %q != parent.Sess %q", child.Cache.Parent, parent.Cache.Sess)
	}
	if stranger.Cache.Parent == parent.Cache.Sess || stranger.Cache.Sess == child.Cache.Sess {
		t.Fatal("digests link two API keys")
	}
	if parent.Cache.Signals.ParentSession != "absent" || child.Cache.Signals.ParentSession != "present" {
		t.Fatalf("parentSession: parent %q child %q", parent.Cache.Signals.ParentSession, child.Cache.Signals.ParentSession)
	}
}

func TestForwardRecordsWhatItCannotSee(t *testing.T) {
	native := `"user_` + strings.Repeat("ab", 32) + `_account_` + testParentSession + `_session_` + testChildSession + `"`
	for name, tc := range map[string]struct {
		userID, shape, parent string
		hasSession            bool
	}{
		"native user id": {native, "native", "not-observable", true},
		"no metadata":    {"", "none", "not-observable", false},
	} {
		body := bodyOf(tc.userID)
		seen, ev := forwardSigned(t, &Handler{}, body, "", nil)
		if string(seen.body) != body {
			t.Fatalf("%s: body changed", name)
		}
		sig := ev.Cache.Signals
		if sig.UserID != tc.shape || sig.ParentSession != tc.parent {
			t.Errorf("%s: userId %q parentSession %q", name, sig.UserID, sig.ParentSession)
		}
		if (ev.Cache.Sess != "") != tc.hasSession || ev.Cache.Parent != "" {
			t.Errorf("%s: sess %q parent %q", name, ev.Cache.Sess, ev.Cache.Parent)
		}
		if sig.AgentHeader != "absent" || sig.ExtendedTTLBeta != "absent" {
			t.Errorf("%s: header signals %q %q, want absent (headers were read)", name, sig.AgentHeader, sig.ExtendedTTLBeta)
		}
	}
}

// Without the client's request the header signals are unknown, not absent.
func TestStartCacheTraceWithoutARequestLeavesHeaderSignalsUnknown(t *testing.T) {
	setupTraceRoute(t, "http://127.0.0.1:1", "")
	tr := (&Handler{}).startCacheTrace(nil, []byte(bodyOf(envelopeID(testChildSession, ""))), true, "key-1")()
	if tr == nil || tr.Signals.AgentHeader != "not-observable" || tr.Signals.ExtendedTTLBeta != "not-observable" {
		t.Fatalf("signals = %+v", tr)
	}
	if tr.Signals.ParentSession != "absent" || tr.Sess == "" {
		t.Fatalf("body-derived signals lost: %+v sess=%q", tr.Signals, tr.Sess)
	}
}
