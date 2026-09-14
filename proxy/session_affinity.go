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
// The forwarder never manufactures a session id. A request without one stays
// without one: generating a per-request UUID would silently give every request
// of the same conversation a different logical session, which is exactly the
// affinity this exists to preserve.

import (
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
	sessionSourceClaudeCode = "claude-code"
	sessionSourceOpencode   = "opencode"
	sessionSourceNone       = "none"
)

// clientSession is the canonical session identity of one inbound request.
// ID is "" when the client presented no session header at all.
type clientSession struct {
	ID     string
	Source string
}

// present reports whether the client actually carried a session identity.
func (s clientSession) present() bool { return s.ID != "" }

// clientSessionAffinity resolves the canonical session identity from the
// inbound request. Precedence: Claude Code's native header, then the native
// OpenCode header. Nothing is generated: no header, no affinity.
func clientSessionAffinity(r *http.Request) clientSession {
	if v := strings.TrimSpace(r.Header.Get(HeaderClaudeCodeSession)); v != "" {
		return clientSession{ID: v, Source: sessionSourceClaudeCode}
	}
	if v := strings.TrimSpace(r.Header.Get(HeaderOpencodeSession)); v != "" {
		return clientSession{ID: v, Source: sessionSourceOpencode}
	}
	return clientSession{Source: sessionSourceNone}
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
//  2. Provider mapping: when the provider configures SessionHeader, that header
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
	case sessionSourceClaudeCode:
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
