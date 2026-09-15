package proxy

// Canonical client session identity for forwarded requests.
//
// Claude Code stamps every API request of a conversation with
// X-Claude-Code-Session-Id. That is the session-level identifier: it groups the
// whole conversation, survives retries and provider failover, and is the only
// basis for upstream session affinity here. X-Claude-Code-Agent-Id is
// agent/subagent-level (parallel agents of one session legitimately differ) and
// X-Client-Request-Id is request-level — neither may stand in for a session.
//
// A native OpenCode client instead sends X-Opencode-Session; it is accepted as a
// fallback source. The identity is normalized ONCE at the gateway boundary and
// the provider-specific spelling happens at egress only, so routing logic never
// grows per-backend header knowledge.
//
// A third carrier exists for compatibility: aggregators that rebuild requests
// (9router's DefaultExecutor builds outbound headers from scratch) drop the
// session headers yet still relay the same Claude Code identity inside the
// body's metadata.user_id — Claude Code itself sends it in both places. That
// body value is a LAST-RESORT source behind both headers, validated against
// strict known shapes only, and it stays an UNTRUSTED routing hint: like the
// headers, it may steer session affinity, but it never feeds authentication,
// billing, or tenant decisions.
//
// The forwarder never manufactures a session id. A request without one stays
// without one: generating a per-request UUID would silently give every request
// of the same conversation a different logical session, which is exactly the
// affinity this exists to preserve.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/textproto"
	"strings"

	"kiro-go/config"
	"kiro-go/logger"
)

const (
	// HeaderClaudeCodeSession is Claude Code's native session header. It is the
	// preferred source of the canonical session identity.
	HeaderClaudeCodeSession = "X-Claude-Code-Session-Id"
	// HeaderOpencodeSession is the native OpenCode client's session header, and
	// also the header an OpenCode Go upstream's backend paths require.
	HeaderOpencodeSession = "X-Opencode-Session"
)

// session sources, for observability only (never logged with the id itself).
const (
	sessionSourceClaudeCode     = "claude-code"
	sessionSourceClaudeMetadata = "claude-metadata"
	sessionSourceOpencode       = "opencode"
	sessionSourceNone           = "none"
)

// clientSession is the canonical session identity of one inbound request.
// ID is "" when the client presented no recognizable session identity at all.
type clientSession struct {
	ID     string
	Source string
}

// present reports whether the client actually carried a session identity.
func (s clientSession) present() bool { return s.ID != "" }

// clientSessionAffinity resolves the canonical session identity from the
// inbound request. Precedence: Claude Code's native header, then the native
// OpenCode header, then the strict body-metadata fallback extracted where the
// handler parsed the body. Nothing is generated: no identity, no affinity.
func clientSessionAffinity(r *http.Request) clientSession {
	if v := strings.TrimSpace(r.Header.Get(HeaderClaudeCodeSession)); v != "" {
		return clientSession{ID: v, Source: sessionSourceClaudeCode}
	}
	if v := strings.TrimSpace(r.Header.Get(HeaderOpencodeSession)); v != "" {
		return clientSession{ID: v, Source: sessionSourceOpencode}
	}
	if v := bodySessionFallback(r.Context()); v != "" {
		return clientSession{ID: v, Source: sessionSourceClaudeMetadata}
	}
	return clientSession{Source: sessionSourceNone}
}

// bodySessionContextKey carries the body-derived session fallback of one
// request. It is populated exactly once — where the handler has already parsed
// the body — so the resolver never needs to touch a body it does not own.
type bodySessionContextKey struct{}

// withBodySessionFallback attaches a body-derived session fallback to the
// request context.
func withBodySessionFallback(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), bodySessionContextKey{}, id))
}

// bodySessionFallback returns the body-derived session fallback; "" when the
// request carries none.
func bodySessionFallback(ctx context.Context) string {
	id, _ := ctx.Value(bodySessionContextKey{}).(string)
	return id
}

// withBodySessionFallbackFromRequest extracts the session identity that
// reached the gateway only inside the body (aggregators that rebuild requests
// drop the session headers) and attaches it to the request context. Headers
// keep precedence: the fallback is read only when neither session header is
// present. A no-op when the body carries no recognizable identity.
func withBodySessionFallbackFromRequest(r *http.Request, req *ClaudeRequest) *http.Request {
	if req == nil || req.Metadata == nil {
		return r
	}
	if id := metadataSessionID(req.Metadata.UserID); id != "" {
		return withBodySessionFallback(r, id)
	}
	return r
}

// maxMetadataUserIDLen bounds how much of metadata.user_id is considered at
// all. Both known shapes are far smaller; anything longer is not an identity.
const maxMetadataUserIDLen = 4096

// metadataSessionID extracts the canonical Claude Code session id from a body
// metadata.user_id, in one of exactly two known shapes:
//
//   - Claude Code's native envelope: "user_<hex>_account_<uuid>_session_<uuid>"
//   - the JSON envelope gateways write when rebuilding the identity:
//     {"device_id":"...","account_uuid":"...","session_id":"<uuid>"}
//
// Only the session id is ever taken — device_id and account_uuid are
// account-scoped and never stand in for a session — and the value must be a
// well-formed UUID, which Claude Code session ids are. Anything else yields
// "": a sessionless request stays sessionless rather than gaining an invented
// identity. Validation bounds what passes as an identity, not whether the
// client can plant one — headers are equally client-controlled, so the id
// remains an untrusted routing hint throughout.
func metadataSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxMetadataUserIDLen {
		return ""
	}
	if sid := sessionIDFromJSONUserID(raw); sid != "" {
		return sid
	}
	return sessionIDFromNativeUserID(raw)
}

// sessionIDFromJSONUserID reads the session_id field of the JSON envelope and
// nothing else. Non-objects, missing fields, and non-UUID values all yield "".
func sessionIDFromJSONUserID(raw string) string {
	if !strings.HasPrefix(raw, "{") {
		return ""
	}
	var envelope struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return ""
	}
	return strictUUID(envelope.SessionID)
}

// sessionIDFromNativeUserID reads the trailing _session_<uuid> of Claude
// Code's native user_id envelope; any other trailing shape yields "".
func sessionIDFromNativeUserID(raw string) string {
	const marker = "_session_"
	i := strings.LastIndex(raw, marker)
	if i < 0 {
		return ""
	}
	return strictUUID(raw[i+len(marker):])
}

// strictUUID accepts exactly a canonical 36-character UUID (hex, with dashes)
// and returns it as sent. Claude Code session ids are UUIDs — the CLI's
// --session-id requires one — so this keeps account fields, request ids, and
// free-form junk from ever passing as a session identity.
func strictUUID(v string) string {
	v = strings.TrimSpace(v)
	if len(v) != 36 {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return ""
			}
			continue
		}
		if !isHexDigit(c) {
			return ""
		}
	}
	return v
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// validHeaderName reports whether name is a well-formed MIME header key after
// canonicalization, so a mistyped provider sessionHeader cannot produce a
// malformed upstream request.
func validHeaderName(name string) bool {
	c := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
	return c != "" && c == textproto.CanonicalMIMEHeaderKey(name)
}

// applyUpstreamSessionAffinity writes the client's canonical session identity
// onto the outbound request. This is the single egress point for session
// headers — the generic forwardableRequestHeaders allow-list deliberately does
// not carry them, because the OpenCode mapping is a per-provider policy, not a
// header every upstream should receive.
//
// Egress policy, in order:
//
//  1. Preserve what the client actually sent: the header the identity came from
//     is relayed with the canonical value. A client that sent both headers keeps
//     both, reconciled to one value (the Claude Code one wins) so the upstream
//     never sees two different logical sessions on one request.
//  2. Body-derived identity (aggregators that dropped the headers) has no
//     session header to preserve; the canonical Claude spelling is written
//     instead, so native-header upstreams see the same session.
//  3. Provider mapping: when the provider configures SessionHeader, that header
//     is set to the canonical value as well — this is the adapter that satisfies
//     an OpenCode Go backend path demanding X-Opencode-Session from a Claude
//     Code conversation, always with the EXACT same value. An invalid
//     configured name is warned about once and ignored rather than corrupting
//     the request.
//
// A request without session identity gets nothing: no preserve, no inject, no
// generated stand-in.
func applyUpstreamSessionAffinity(dst *http.Request, src *http.Request, up config.UpstreamProvider) clientSession {
	sess := clientSessionAffinity(src)
	if !sess.present() {
		return sess
	}
	if src.Header.Get(HeaderClaudeCodeSession) != "" {
		dst.Header.Set(HeaderClaudeCodeSession, sess.ID)
	}
	if src.Header.Get(HeaderOpencodeSession) != "" {
		dst.Header.Set(HeaderOpencodeSession, sess.ID)
	}
	if sess.Source == sessionSourceClaudeMetadata {
		// The identity arrived in the body, so there is no session header to
		// preserve; write the canonical Claude spelling so upstreams reading
		// the native header see the session the mapping header carries.
		dst.Header.Set(HeaderClaudeCodeSession, sess.ID)
	}
	applyProviderSessionMapping(dst, sess, up)
	return sess
}

// applyClientSessionAffinity is the variant for callers that already resolved
// the identity (the local web-search loop resolves once and replays it on every
// continuation round). The header the identity came from is preserved with the
// canonical value, plus the provider mapping when configured. A non-present
// session writes nothing.
func applyClientSessionAffinity(dst *http.Request, sess clientSession, up config.UpstreamProvider) {
	if !sess.present() {
		return
	}
	switch sess.Source {
	case sessionSourceClaudeCode, sessionSourceClaudeMetadata:
		dst.Header.Set(HeaderClaudeCodeSession, sess.ID)
	case sessionSourceOpencode:
		dst.Header.Set(HeaderOpencodeSession, sess.ID)
	}
	applyProviderSessionMapping(dst, sess, up)
}

// applyProviderSessionMapping writes the provider-configured session header,
// if any, with the canonical value — the EXACT id the client presented, never
// a regenerated one. An invalid configured name is warned about and skipped
// rather than corrupting the upstream request.
func applyProviderSessionMapping(dst *http.Request, sess clientSession, up config.UpstreamProvider) {
	name := strings.TrimSpace(up.SessionHeader)
	if name == "" || !sess.present() {
		return
	}
	if !validHeaderName(name) {
		logger.Warnf("[Forward] provider %s has an invalid sessionHeader %q; session mapping skipped", up.Name, up.SessionHeader)
		return
	}
	dst.Header.Set(name, sess.ID)
}

// logSessionAffinity records the session-affinity metadata for one upstream
// attempt. Deliberately id-free: the session id is client conversation state,
// so only presence, source and the mapped upstream header reach the log.
func logSessionAffinity(sess clientSession, upstreamHeader string) {
	if !sess.present() {
		logger.Debugf("[Forward] session_affinity_present=false session_source=none upstream_session_header=none")
		return
	}
	logger.Infof("[Forward] session_affinity_present=true session_source=%s upstream_session_header=%s",
		sess.Source, upstreamHeader)
}

// ProbeSessionPrefix marks a synthetic session id carried by an admin Test
// probe. It is diagnostic traffic, not a conversation: the prefix keeps probe
// sessions recognizable upstream, and each probe gets its own id so one Test
// click can never be confused with another probe or with real traffic.
const ProbeSessionPrefix = "kiro-go-probe-"

// probeSessionID derives a fresh synthetic probe session id from the admin
// request's own request id (generated when the middleware gave none).
func probeSessionID(requestID string) string {
	if requestID == "" {
		requestID = config.GenerateMachineId()
	}
	return ProbeSessionPrefix + requestID
}

// applyProbeSessionAffinityInto writes the session headers a Test probe presents
// to an upstream, into the probe's extra-header map (probeUpstreamOnce builds
// the actual request from it). The probe carries a clearly-marked synthetic
// session id and runs through the SAME egress policy as real traffic —
// including the provider's SessionHeader mapping — because the Test button must
// answer the question an operator actually has: "would a real forwarded request
// work?". A sessionless probe would exercise a path no real client takes and
// read backends that demand session identity (OpenCode Go answers
// MissingSessionID) as broken targets while live forwarding works fine.
//
// The synthetic id is scoped to the probe: nothing here touches the no-
// generation rule for client traffic, which stays absolute (see
// clientSessionAffinity).
func applyProbeSessionAffinityInto(dst map[string]string, probeID string, up config.UpstreamProvider) {
	if probeID == "" {
		return
	}
	dst[HeaderClaudeCodeSession] = probeID
	if name := strings.TrimSpace(up.SessionHeader); name != "" && validHeaderName(name) {
		dst[name] = probeID
	}
}
