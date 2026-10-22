package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"kiro-go/cachetrace"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/metrics"
)

// Glue between the forwarding relay and package cachetrace. The relay asks for a
// trace when it builds the upstream request and reads it when it records the
// event; the digests are computed in between, beside the upstream call.

// cacheTracer returns the shared Tracer, built on first use. Its key derives from
// the api-key pepper, which already persists across restarts, so digests stay
// comparable across a redeploy. Without that secret tracing stays off.
func (h *Handler) cacheTracer() *cachetrace.Tracer {
	h.cacheTraceOnce.Do(func() {
		pepper, err := config.GetOrCreateAPIKeyPepper()
		if err != nil {
			logger.Warnf("[CacheTrace] off: no signing secret: %v", err)
			return
		}
		h.cacheTraceTracer = cachetrace.New(pepper)
	})
	return h.cacheTraceTracer
}

// startCacheTrace begins digesting a Claude Messages body and returns the reader
// for the result: nil when tracing is off, the route is not Claude-shaped, or the
// body is not a Messages request. The reader may be called any number of times.
// It never fails the relay: a panic while hashing yields no trace.
//
// r is the client's request, read here and never kept: its headers feed the
// signals that are not in the body. A nil r leaves those not observable.
// apiKeyID scopes the session digests to one principal.
func (h *Handler) startCacheTrace(r *http.Request, body []byte, isClaudeRoute bool, apiKeyID string) func() *metrics.CacheTrace {
	none := func() *metrics.CacheTrace { return nil }
	if !isClaudeRoute || !config.CacheTraceEnabled() {
		return none
	}
	t := h.cacheTracer()
	if t == nil {
		return none
	}
	policy := tokenOptPolicyLabel(config.GetTokenOptimization())
	in := cachetrace.Request{Principal: apiKeyID, Ingress: ingressOf(r), Identify: claudeUserIdentity}
	done := make(chan *metrics.CacheTrace, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[CacheTrace] hashing panicked; event recorded without a trace: %v", r)
				done <- nil
			}
		}()
		done <- t.TraceFor(body, policy, in)
	}()
	var (
		once sync.Once
		res  *metrics.CacheTrace
	)
	return func() *metrics.CacheTrace {
		once.Do(func() { res = <-done })
		return res
	}
}

// tokenOptPolicyLabel names the Token Optimization settings in force, in a form
// that changes whenever they change what the prompt contains: "off", "observe",
// or "on" followed by each active part, e.g. "on+headroom-cache+caveman".
func tokenOptPolicyLabel(cfg config.TokenOptimizationConfig) string {
	switch cfg.Mode {
	case config.TokenOptModeOn:
	case config.TokenOptModeObserve:
		return config.TokenOptModeObserve
	default:
		return config.TokenOptModeOff
	}
	parts := []string{config.TokenOptModeOn}
	if cfg.InputCompression == config.TokenOptInputHeadroom {
		parts = append(parts, "headroom-"+cfg.Headroom.Mode)
	}
	if cfg.Caveman != "" && cfg.Caveman != config.TokenOptStyleOff {
		parts = append(parts, cfg.Caveman)
	}
	if cfg.Ponytail != "" && cfg.Ponytail != config.TokenOptPonytailOff {
		parts = append(parts, "ponytail-"+cfg.Ponytail)
	}
	return strings.Join(parts, "+")
}

// ingressOf reads the client headers a cache trace records. The beta value is
// read the way forwardRequestHeaders reads it, so it is what the upstream gets;
// the agent headers are not on that allow-list and never leave this gateway.
func ingressOf(r *http.Request) *cachetrace.Ingress {
	if r == nil {
		return nil
	}
	return &cachetrace.Ingress{
		AgentID: r.Header.Get("X-Claude-Code-Agent-Id") != "" || r.Header.Get("X-Claude-Code-Parent-Agent-Id") != "",
		Beta:    r.Header.Get("Anthropic-Beta"),
	}
}

// parentSessionKey is how a body names a parent session. The upstream hop treats
// the key's mere presence, in any shape of user_id, as the mark of a subagent.
const parentSessionKey = `"parent_session_id"`

// claudeUserIdentity reads a body's metadata.user_id as given, a JSON string or
// an object. The session id comes from the same parser session affinity uses;
// the parent id from the JSON envelope's parent_session_id, held to the same
// rule as a session id.
//
// Only the JSON envelope can carry a parent, so only it can show that there is
// none: Claude Code's native form, no user_id and an unusable one say
// not-observable.
func claudeUserIdentity(raw json.RawMessage) cachetrace.Identity {
	id := cachetrace.Identity{Shape: "none", Parent: metrics.SignalNotObservable}
	text, ok := userIDText(raw)
	if !ok {
		id.Shape = "invalid"
		return id
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return id
	}
	var env map[string]json.RawMessage
	switch {
	case len(text) > maxMetadataUserIDLen:
		id.Shape = "invalid"
	case strings.HasPrefix(text, "{") && json.Unmarshal([]byte(text), &env) == nil:
		id.Shape = "json"
		id.Session = sessionIDFromJSONUserID(text)
		var parent string
		if json.Unmarshal(env["parent_session_id"], &parent) == nil {
			id.ParentSession = sanitizedSessionToken(parent)
		}
	default:
		if id.Session = sessionIDFromNativeUserID(text); id.Session != "" {
			id.Shape = "native"
		} else {
			id.Shape = "invalid"
		}
	}
	switch {
	case strings.Contains(text, parentSessionKey):
		id.Parent = metrics.SignalPresent
	case id.Shape == "json":
		id.Parent = metrics.SignalAbsent
	}
	return id
}

// userIDText returns the text of a user_id given as a JSON string, or the JSON
// text of one given as an object. ok is false for any other value.
func userIDText(raw json.RawMessage) (string, bool) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	switch raw[0] {
	case '"':
		var s string
		return s, json.Unmarshal(raw, &s) == nil
	case '{':
		return string(raw), true
	}
	return "", false
}
