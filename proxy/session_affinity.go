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
// Clients that are neither Claude Code nor a native OpenCode client may still
// carry a real conversation session under a generic spelling — X-Session-
// Affinity, X-Session-Id or Session-Id. Those are accepted behind both native
// headers, on the same terms: an explicit, client-supplied conversation id.
//
// A body carrier exists for compatibility: aggregators that rebuild requests
// (9router's DefaultExecutor builds outbound headers from scratch) drop the
// session headers yet still relay the same Claude Code identity inside the
// body's metadata.user_id — Claude Code itself sends it in both places. That
// body value is a LAST-RESORT source behind every header, validated against
// strict known shapes only, and it stays an UNTRUSTED routing hint: like the
// headers, it may steer session affinity, but it never feeds authentication,
// billing, or tenant decisions.
//
// Kiro-Session is the gateway's own conversation token, for callers that have no
// conversation identity of their own but can read a response header and send it
// back. It is a DOWNSTREAM contract (client <-> Kiro-Go) and never an upstream
// one: an inbound Kiro-Session becomes the canonical identity and reaches the
// provider through that provider's configured SessionHeader, but the
// Kiro-Session header itself is never written on an outbound request. It is also
// the WEAKEST identity, below the strict body fallback, so a client that starts
// sending a real conversation id is never overridden by a token Kiro-Go handed
// it earlier.
//
// Only carriers whose conversation semantics are established are read. Notably
// absent, and deliberately so: X-Client-Request-Id (request-level for Claude
// Code even though some clients overload it with a session), prompt_cache_key
// (a cache bucketing key with no one-key-per-conversation guarantee) and
// previous_response_id (a per-response chain pointer that changes every turn).
// A field is not a session because its name sounds like one.
//
// Session RESOLUTION never manufactures an id: a request without one resolves
// to no identity, because giving every request of a conversation a different
// logical session would destroy the affinity this exists to preserve. Whether a
// sessionless request may still proceed, and under which generated identity, is
// a separate decision made in exactly one place — see
// effectiveSessionForProvider, config.ManagedSessionsEnabled and
// config.SessionMissingPolicy.
//
// There are two generated identities and they come from ONE per-request holder,
// so a single logical request can never mint two different ids: whichever reason
// asks first materializes the value and the other reason cannot be reached.
// They differ only in what they claim. A managed issuance (kiro-issued) is
// offered to the client and may become conversation identity on the next turn; a
// provider fallback (synthetic-request) is availability only and never leaves the
// request. Both are request-scoped when generated, because at that moment nobody
// knows whether the client will ever send the token back.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/textproto"
	"strings"
	"sync"

	"github.com/google/uuid"

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
	// HeaderSessionAffinity is the generic affinity spelling emitted by clients
	// that are neither Claude Code nor OpenCode (OpenClaw sends it on both its
	// Anthropic and OpenAI paths when session affinity is enabled).
	HeaderSessionAffinity = "X-Session-Affinity"
	// HeaderSessionID and HeaderSessionIDBare are the two remaining generic
	// spellings in circulation; aggregators read both interchangeably.
	HeaderSessionID     = "X-Session-Id"
	HeaderSessionIDBare = "Session-Id"
	// HeaderKiroSession is the gateway's own managed conversation token, sent on
	// the response when one is issued and read back on later turns. No "X-"
	// prefix: RFC 6648 deprecates it for new protocol parameters. It is
	// deliberately NOT part of sessionHeaderCarriers — that list also drives
	// egress reconciliation, and this header must never reach an upstream.
	//
	// RETENTION IS THE CLIENT'S JOB, and the contract is deliberately one-way:
	// once a client has been given a token for a conversation it keeps sending
	// that same value on every later request of that conversation until the
	// conversation ends or the client starts a new one. A response WITHOUT the
	// header does not revoke anything — there is no revocation in this protocol.
	// Absence simply means this turn had nothing to announce, which happens
	// whenever a turn resolves no session at all: a turn served entirely by the
	// Kiro account pool never builds an upstream request, so it never reaches
	// effectiveSessionForProvider and never publishes. A client that mirrored the
	// last response instead of remembering the token would drop it there and
	// silently lose affinity, which is why the contract is stated as retention
	// rather than as an echo of whatever the gateway last said.
	HeaderKiroSession = "Kiro-Session"
)

// session sources, for observability only (never logged with the id itself).
// Deliberately low-cardinality: a fixed, closed set of labels, never a value
// derived from the session id.
const (
	sessionSourceClaudeCode       = "claude-code"
	sessionSourceClaudeMetadata   = "claude-metadata"
	sessionSourceOpencode         = "opencode"
	sessionSourceGenericAffinity  = "generic-affinity"
	sessionSourceGenericSession   = "generic-session"
	sessionSourceSyntheticRequest = "synthetic-request"
	// sessionSourceKiroIssued is a managed token Kiro-Go has just minted and
	// offered to the client. Adoption is NOT proven: it stays request-scoped
	// until a later turn actually presents it.
	sessionSourceKiroIssued = "kiro-issued"
	// sessionSourceKiroManaged is a managed token the client sent back, which is
	// the only evidence that cross-turn identity exists.
	sessionSourceKiroManaged = "kiro-managed"
	sessionSourceNone        = "none"
)

// sessionScope separates an identity that groups a whole conversation from one
// that is only valid for a single logical request. The distinction is the whole
// reason a synthetic fallback is safe to have: it tells every consumer that this
// identity must not be presented as conversation continuity.
type sessionScope uint8

const (
	// scopeUnknown is the zero value and means "no identity". It must never be
	// read as conversation scope — a zero-valued clientSession claiming to group
	// a conversation is exactly the bug this ordering prevents.
	scopeUnknown sessionScope = iota
	// scopeConversation is a client-supplied identity that groups turns.
	scopeConversation
	// scopeRequest is valid for ONE logical request only (all its retries,
	// failovers and tool rounds), and deliberately differs on the next turn.
	scopeRequest
)

func (s sessionScope) String() string {
	switch s {
	case scopeConversation:
		return "conversation"
	case scopeRequest:
		return "request"
	default:
		return ""
	}
}

// clientSession is the canonical session identity of one inbound request.
// ID is "" when the client presented no recognizable session identity at all.
type clientSession struct {
	ID     string
	Source string
	Scope  sessionScope
}

// present reports whether the session carries an identity at all.
func (s clientSession) present() bool { return s.ID != "" }

// sessionHeaderCarriers is the ordered precedence of header carriers. Native
// spellings first (they are unambiguous), then the generic ones. Order is the
// contract: it is what makes the resolution deterministic when a client sends
// several, and it is asserted by tests rather than left to map iteration.
var sessionHeaderCarriers = []struct {
	Header string
	Source string
}{
	{HeaderClaudeCodeSession, sessionSourceClaudeCode},
	{HeaderOpencodeSession, sessionSourceOpencode},
	{HeaderSessionAffinity, sessionSourceGenericAffinity},
	{HeaderSessionID, sessionSourceGenericSession},
	{HeaderSessionIDBare, sessionSourceGenericSession},
}

// sessionSourceHeader is the header a source is echoed back under when the
// caller has no inbound request to compare against (the local web-search loop
// replays a pre-resolved identity). A body-derived identity has no header of its
// own, so it takes the canonical Claude spelling — the same rule the normal
// forward path applies.
func sessionSourceHeader(source string) string {
	switch source {
	case sessionSourceClaudeCode, sessionSourceClaudeMetadata:
		return HeaderClaudeCodeSession
	case sessionSourceOpencode:
		return HeaderOpencodeSession
	case sessionSourceGenericAffinity:
		return HeaderSessionAffinity
	case sessionSourceGenericSession:
		return HeaderSessionID
	case sessionSourceKiroManaged:
		// Deliberately no header. Kiro-Session is a downstream contract; echoing
		// it upstream would tell a provider the gateway's own token is a client
		// conversation id. The canonical value still reaches the provider through
		// its configured SessionHeader, which is the only channel it may use.
		return ""
	default:
		return ""
	}
}

// clientSessionAffinity resolves the canonical session identity the CLIENT
// presented. Precedence: the native Claude Code header, the native OpenCode
// header, the generic affinity/session headers, the strict body-metadata
// fallback extracted where the handler parsed the body, and last a Kiro-Session
// token the client is echoing back to us.
//
// Every header carrier goes through the same bounded validation as the body
// carrier, so an oversized or control-character-bearing value is not an
// identity: that carrier is skipped and the next source is tried, exactly as if
// it had been absent. A duplicate header keeps Go's first-value semantics.
//
// Kiro-Session sits last on purpose. It is the only carrier the gateway itself
// authored, so any real client identity must be able to supersede it: a client
// migrating from managed sessions to its own conversation id would otherwise be
// pinned to a stale token Kiro-Go issued. Being gateway-owned, it is also held to
// a stricter shape than the opaque generic carriers — exactly the canonical UUID
// this code issues, nothing else — and a malformed value is simply absent, which
// lets the managed layer issue a fresh valid one.
//
// Nothing is generated here. A sessionless request resolves to no identity; both
// generated identities live in effectiveSessionForProvider.
func clientSessionAffinity(r *http.Request) clientSession {
	for _, c := range sessionHeaderCarriers {
		if v := sanitizedSessionToken(r.Header.Get(c.Header)); v != "" {
			return clientSession{ID: v, Source: c.Source, Scope: scopeConversation}
		}
	}
	if v := bodySessionFallback(r.Context()); v != "" {
		return clientSession{ID: v, Source: sessionSourceClaudeMetadata, Scope: scopeConversation}
	}
	if v := strictUUID(r.Header.Get(HeaderKiroSession)); v != "" {
		return clientSession{ID: v, Source: sessionSourceKiroManaged, Scope: scopeConversation}
	}
	return clientSession{Source: sessionSourceNone, Scope: scopeUnknown}
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

// maxSessionTokenLen caps an extracted session token, matching the producer's
// own limit on the JSON envelope's session_id field.
const maxSessionTokenLen = 256

// metadataSessionID extracts the canonical Claude Code session id from a body
// metadata.user_id, in one of exactly two known shapes:
//
//   - Claude Code's native envelope: "user_<hex>_account_<uuid>_session_<uuid>"
//   - the JSON envelope gateways write when rebuilding the identity:
//     {"device_id":"...","account_uuid":"...","session_id":"..."}
//
// Only the session id is ever taken — device_id and account_uuid are
// account-scoped and never stand in for a session. The two shapes validate
// differently: the native suffix is position-derived, so it must be a
// well-formed UUID (which Claude Code session ids are), while the JSON field
// is written by the aggregator's own session resolver and may be any bounded,
// whitespace-free token (9router's fallback resolver mints UUIDs with a
// timestamp suffix). Anything else yields "": a sessionless request stays
// sessionless rather than gaining an invented identity. Validation bounds
// what passes as an identity, not whether the client can plant one — headers
// are equally client-controlled, so the id remains an untrusted routing hint
// throughout.
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
// nothing else. Non-objects, missing fields, and unusable values all yield "".
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
	return sanitizedSessionToken(envelope.SessionID)
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

// sanitizedSessionToken accepts a bounded, whitespace-free session token and
// returns it as sent — the acceptance rule the JSON envelope's producer
// (9router's normalizeSessionId: trim, ≤256 chars) applies to the same field.
func sanitizedSessionToken(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxSessionTokenLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c <= ' ' || c == 0x7f {
			return ""
		}
	}
	return v
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

// requestGeneratedSession is the ONE generated identity a single logical
// inference request may use, whichever reason asks for it: a managed issuance
// offered to the client, or a provider's missing-session fallback. It is
// attached at the request boundary (serveInference) and materialized lazily, so
// a request that needs neither never mints an id at all.
//
// Deliberately NOT two holders. Two would let one request answer the client with
// one id and the upstream with another — the client would faithfully echo a token
// the provider never saw, and the next turn's telemetry would report confirmed
// cross-turn identity for a conversation that never had any. One holder makes
// that state unrepresentable rather than merely discouraged.
//
// The value is generated exactly once per logical request and reused by every
// attempt beneath it — key rotation, failover to another target, and every
// local web-search round — because those are all the same client turn. The locks
// are request-local by construction: the holder lives in one request's context
// and is never shared, so there is no cross-request contention and no global
// state to scale or evict.
type requestGeneratedSession struct {
	mintOnce sync.Once
	id       string

	// managed records whether managed issuance was enabled for this request,
	// read once at the boundary so a mid-request config change cannot make one
	// logical request behave two ways.
	managed bool
	// resp is the response header map captured at the boundary, before the
	// handler runs and therefore before any status line or body byte. Nil for
	// callers with no response to write to, which disables publication only.
	resp        http.Header
	publishOnce sync.Once
}

// value returns this request's generated session id, minting it on first use.
func (s *requestGeneratedSession) value() string {
	if s == nil {
		return ""
	}
	s.mintOnce.Do(func() { s.id = uuid.NewString() })
	return s.id
}

// publish announces a managed session to the client exactly once per logical
// request. Idempotent and race-free by the Once: for any one request the
// published value is determined before the first call — either the token the
// client echoed or this holder's own minted id, never both, because an echoed
// token resolves as a client identity and stops resolution before issuance.
func (s *requestGeneratedSession) publish(id string) {
	if s == nil || s.resp == nil || id == "" {
		return
	}
	s.publishOnce.Do(func() { s.resp.Set(HeaderKiroSession, id) })
}

type requestGeneratedSessionKey struct{}

// withRequestGeneratedSession attaches an unmaterialized generated-session
// holder to a request. Called once per logical inference request, after
// authentication: attaching costs nothing until some reason asks for the value,
// and an unauthenticated request never reaches here, so it is never offered a
// managed session.
//
// resp is the response header map to publish a managed session into; pass nil to
// disable publication. managed is config.GetManagedSessionsEnabled(), sampled
// here so the whole request sees one answer.
func withRequestGeneratedSession(r *http.Request, resp http.Header, managed bool) *http.Request {
	holder := &requestGeneratedSession{managed: managed, resp: resp}
	return r.WithContext(context.WithValue(r.Context(), requestGeneratedSessionKey{}, holder))
}

func requestGeneratedSessionHolder(ctx context.Context) *requestGeneratedSession {
	holder, _ := ctx.Value(requestGeneratedSessionKey{}).(*requestGeneratedSession)
	return holder
}

// requestGeneratedSessionID returns this request's generated session id, minting
// it on first use. Empty when no holder was attached, which keeps non-inference
// paths (and any caller outside serveInference) from silently gaining a
// per-attempt identity: no holder means no generated session at all.
func requestGeneratedSessionID(ctx context.Context) string {
	return requestGeneratedSessionHolder(ctx).value()
}

// issuedManagedSessionID mints this request's managed session and offers it to
// the client, returning "" when managed issuance is off or unavailable. The
// response header is set here rather than at the boundary because whether a token
// may be issued at all is only known after the client's own identity has been
// resolved — the body-metadata carrier is attached inside the handler, so a
// request that turns out to own a real session must not have been told about a
// managed one.
func issuedManagedSessionID(ctx context.Context) string {
	holder := requestGeneratedSessionHolder(ctx)
	if holder == nil || !holder.managed {
		return ""
	}
	id := holder.value()
	holder.publish(id)
	return id
}

// republishManagedSession echoes a client's own managed token back on the
// response, so a client that adopted the protocol keeps being told the token is
// still current and does not have to remember it from the first turn alone.
func republishManagedSession(ctx context.Context, id string) {
	requestGeneratedSessionHolder(ctx).publish(id)
}

// effectiveSessionForProvider is the SINGLE place the missing-session question
// is answered, for both the normal forward path and the local web-search loop.
// Letting each call site decide is how the two paths drift apart.
//
//	real client identity        -> that identity, conversation-scoped
//	echoed Kiro-Session         -> that token, conversation-scoped, re-offered
//	none + managed sessions on  -> this request's one UUID, request-scoped, offered
//	none + passthrough          -> no identity (existing behavior)
//	none + synthetic_request    -> this request's one UUID, request-scoped
//
// Managed issuance is ordered ahead of the provider fallback and returns a
// present identity, so the fallback is unreachable once a managed token exists.
// That is what guarantees one generated id per logical request: the two reasons
// cannot both run, and they share one holder even if they could.
//
// The synthetic branch requires a usable SessionHeader: that header is the only
// channel the generated value can travel in, so without it the policy could only
// produce an identity nothing carries. That combination is refused at admin
// write time and warned about here rather than silently doing nothing. Managed
// issuance has no such requirement — its value has a second channel, the response
// header, so it is still worth minting for a provider that maps no session.
func effectiveSessionForProvider(r *http.Request, up config.UpstreamProvider) clientSession {
	sess := clientSessionAffinity(r)
	if sess.present() {
		if sess.Source == sessionSourceKiroManaged {
			republishManagedSession(r.Context(), sess.ID)
		}
		return sess
	}
	if id := issuedManagedSessionID(r.Context()); id != "" {
		return clientSession{ID: id, Source: sessionSourceKiroIssued, Scope: scopeRequest}
	}
	if up.SessionMissingPolicyResolved() != config.SessionMissingSyntheticRequest {
		return sess
	}
	name := strings.TrimSpace(up.SessionHeader)
	if name == "" || !validHeaderName(name) {
		logger.Warnf("[Forward] provider %s requests a synthetic session but its sessionHeader %q cannot carry one; request stays sessionless", up.Name, up.SessionHeader)
		return sess
	}
	id := requestGeneratedSessionID(r.Context())
	if id == "" {
		return sess
	}
	return clientSession{ID: id, Source: sessionSourceSyntheticRequest, Scope: scopeRequest}
}

// applyUpstreamSessionAffinity writes the effective session identity onto the
// outbound request. This is the single egress point for session headers — the
// generic forwardableRequestHeaders allow-list deliberately does not carry them,
// because the provider mapping is a per-provider policy, not a header every
// upstream should receive.
//
// Egress policy, in order:
//
//  1. Preserve what the client actually sent: every session header present on
//     the inbound request is relayed with the ONE canonical value. A client that
//     sent several keeps them all, reconciled (the highest-precedence carrier
//     wins) so the upstream never sees two different logical sessions on one
//     request.
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
// A REQUEST-SCOPED generated identity (a managed issuance or a provider
// fallback) skips step 1 and 2 entirely: it travels only through the provider's
// own configured header. Writing it into a native header would tell that upstream
// a real client conversation id had arrived, which is the one thing a generated
// value must never claim. An operator who genuinely wants the native spelling
// configures SessionHeader as that header.
//
// A managed token the client echoed is conversation-scoped and so does reach step
// 1, but Kiro-Session is absent from sessionHeaderCarriers, so the loop finds
// nothing to reconcile and the token itself never leaves the gateway. That
// omission is the mechanism, not an oversight: the carrier list drives both
// ingress and egress, and this is the first carrier where those two sets differ.
//
// A request with no effective identity gets nothing: no preserve, no inject.
func applyUpstreamSessionAffinity(dst *http.Request, src *http.Request, up config.UpstreamProvider) clientSession {
	sess := effectiveSessionForProvider(src, up)
	if !sess.present() {
		return sess
	}
	if sess.Scope == scopeConversation {
		// Reconcile every carrier the client presented a USABLE value in. The
		// same validation decides what counts as a carrier here as during
		// resolution: a carrier whose value could not be an identity is treated
		// as absent, so it is neither relayed nor rewritten to the winning value
		// — writing it would state a claim (a Claude Code session id, say) that
		// the client never validly made.
		for _, c := range sessionHeaderCarriers {
			if sanitizedSessionToken(src.Header.Get(c.Header)) != "" {
				dst.Header.Set(c.Header, sess.ID)
			}
		}
		if sess.Source == sessionSourceClaudeMetadata {
			// The identity arrived in the body, so there is no session header to
			// preserve; write the canonical Claude spelling so upstreams reading
			// the native header see the session the mapping header carries.
			dst.Header.Set(HeaderClaudeCodeSession, sess.ID)
		}
	}
	applyProviderSessionMapping(dst, sess, up)
	return sess
}

// applyClientSessionAffinity is the variant for callers that already resolved
// the effective identity (the local web-search loop resolves once and replays it
// on every continuation round). The carrier the identity came from is echoed with
// the canonical value, plus the provider mapping when configured. A
// request-scoped synthetic identity travels only through the provider mapping,
// exactly as on the normal forward path. A non-present session writes nothing.
func applyClientSessionAffinity(dst *http.Request, sess clientSession, up config.UpstreamProvider) {
	if !sess.present() {
		return
	}
	if sess.Scope == scopeConversation {
		if name := sessionSourceHeader(sess.Source); name != "" {
			dst.Header.Set(name, sess.ID)
		}
	}
	applyProviderSessionMapping(dst, sess, up)
}

// applyProviderSessionMapping writes the provider-configured session header,
// if any, with the canonical value — the EXACT effective id for this request,
// never one regenerated per attempt. An invalid configured name is warned about
// and skipped rather than corrupting the upstream request.
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

// sessionMappedUpstream reports whether the provider's configured session header
// was actually written for this identity — the question an operator debugging a
// MissingSessionID needs answered, and one that "SessionHeader is configured"
// alone does not answer (an unusable name is skipped, and a sessionless request
// writes nothing).
func sessionMappedUpstream(sess clientSession, up config.UpstreamProvider) bool {
	name := strings.TrimSpace(up.SessionHeader)
	return sess.present() && name != "" && validHeaderName(name)
}

// logSessionAffinity records the session-affinity metadata for one upstream
// attempt. Deliberately id-free: the session id is client conversation state, so
// only presence, source, scope and the mapped upstream header reach the log.
//
// A sessionless request stays at Debug on purpose. For a provider whose policy
// is passthrough it is the normal, correct state, and promoting it would turn
// ordinary traffic into a warning stream. The exceptional cases — a synthetic
// policy that cannot map, an invalid configured header — warn from where they are
// detected instead.
func logSessionAffinity(sess clientSession, upstreamHeader string) {
	if !sess.present() {
		logger.Debugf("[Forward] session_affinity_present=false session_source=none upstream_session_header=none")
		return
	}
	logger.Infof("[Forward] session_affinity_present=true session_source=%s session_scope=%s upstream_session_header=%s",
		sess.Source, sess.Scope, upstreamHeader)
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
