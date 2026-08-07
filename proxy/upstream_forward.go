package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/metrics"
	"net/http"
	"strings"
	"time"
)

// tryForwardUpstream checks whether the given client model matches an enabled
// forward route. If so, it passes the raw request body through to one of the
// route's upstream targets (an OpenAI/Anthropic-compatible endpoint such as
// 9router or xpiki), streams or copies the response back to the client, and
// returns true.
//
// A route may list several ranked targets. They are tried in the order
// config.ResolveRoute returns (Priority tier, then weighted pick within the tier),
// but only while the failure is one another provider might not share AND nothing
// has been written to the client yet — see forwardOutcome. Once the first byte
// reaches the client the request is committed to that target, because a response
// cannot be un-sent; a mid-stream failure is therefore reported as a failure, not
// retried elsewhere.
//
// When there is no matching enabled route — including a route whose every target
// is disabled or whose providers are all disabled — it returns false and the
// caller continues with the normal Kiro dispatch path. Forwarded requests bypass
// the account pool and per-API-key quota, but are still counted in the global
// request/success/failure stats. Token counts are not tracked for passthroughs.
//
// subPath is the upstream path appended to the provider BaseURL (e.g.
// "/messages", "/chat/completions", "/responses"). isClaudeRoute selects the
// error-response shape (Anthropic vs OpenAI) on failure.
//
// captureUserText, when non-empty AND memory capture is enabled AND this is a
// non-streaming Claude route, causes the relayed assistant answer to be captured
// into memory (fire-and-forget). It is "" on paths where capture is out of scope
// (OpenAI/responses) or unavailable (streaming). Capture never affects the relay.
func (h *Handler) tryForwardUpstream(r *http.Request, w http.ResponseWriter, body []byte, model string, stream bool, subPath string, isClaudeRoute bool, captureUserText string) bool {
	route, targets := config.ResolveRoute(model)
	if route == nil || len(targets) == 0 {
		return false
	}

	// Demote targets whose circuit is open, so a provider that is currently down
	// does not cost every request a wasted round-trip before failover. Targets are
	// reordered, never dropped: an all-cold route still tries everything, and a
	// route can never silently become unrouted (which would fall through to the
	// Kiro pool and answer from a different backend entirely).
	if demoted := applyForwardCooldown(targets, time.Now()); demoted > 0 {
		logForwardCooldown(model, demoted, targets[0].Provider.Name)
	}

	// Walk the targets in resolved order. A target is only retried past when it
	// failed BEFORE anything was written to the client — see forwardToTarget.
	var last forwardOutcome
	for i, rt := range targets {
		moreTargets := i+1 < len(targets)
		outcome := h.forwardToTarget(r, w, body, model, stream, subPath, isClaudeRoute, captureUserText, route, rt, i, moreTargets)
		last = outcome
		if outcome.committed {
			// The client has its answer (success, or an error we chose to surface).
			return true
		}
		if !outcome.retryable {
			break
		}
		if moreTargets {
			logger.Warnf("[Forward] %s via %s failed (%s); trying %s",
				model, rt.Provider.Name, outcome.internalMsg, targets[i+1].Provider.Name)
		}
	}

	// Every eligible target failed without producing a client response. Report the
	// last failure — the client asked for one model and gets one error, rather
	// than an error per attempted provider.
	if last.canceled {
		// Client gave up mid-attempt; nothing to send and nothing to blame.
		return true
	}
	h.recordFailure()
	if isClaudeRoute {
		h.sendClaudeError(w, last.status, "api_error", last.clientMsg)
	} else {
		h.sendOpenAIError(w, last.status, "server_error", last.clientMsg)
	}
	return true
}

// forwardOutcome describes one attempt against a single upstream target.
//
// The two flags are the whole contract of the retry loop and are deliberately
// independent:
//
//   - committed: a client-visible byte (or an error response) has been written.
//     Once true the request is finished, whatever else happened — no further
//     target may be tried, because we cannot un-send a response. This mirrors
//     streamGuard.Committed() on the Kiro pool's account-rotation path
//     (proxy/chat_executor.go); the two loops gate retry on the same boundary.
//   - retryable: the attempt failed for a reason another provider might not
//     share (connection error, 429, 5xx) AND nothing was written. Only then is
//     it safe to move on.
//
// clientMsg/status/internalMsg carry what to tell the client if this turns out
// to be the last attempt.
type forwardOutcome struct {
	committed   bool
	retryable   bool
	canceled    bool
	status      int
	clientMsg   string
	internalMsg string
}

// retryableUpstreamStatus reports whether an upstream HTTP status is worth trying
// on a different provider.
//
// 429 and 5xx are provider-specific conditions (rate limit, outage) that a backup
// may well serve. 4xx other than 429 are not: 400 means the payload is wrong, 401
// and 403 mean the credential or entitlement is wrong. Retrying those just
// multiplies the same failure across every provider and delays the real error
// reaching the operator, so they are surfaced immediately.
func retryableUpstreamStatus(status int) bool {
	return status == 429 || status >= 500
}

// forwardToTarget performs one attempt: build the request, relay the response,
// record metrics. Everything that can fail before the first client write returns
// committed=false so the caller may try the next target.
//
// attempt is the zero-based index in the resolved target list; it is recorded on
// the metric so the stats panel can distinguish "served first try" from
// "served after failover".
//
// moreTargets tells this attempt whether a backup exists. It only affects
// retryable non-2xx handling: with a backup left the error body is withheld (so
// the next target inherits an unwritten ResponseWriter), and without one it is
// relayed verbatim, preserving the pre-multi-target contract that clients receive
// the upstream's own error shape.
func (h *Handler) forwardToTarget(r *http.Request, w http.ResponseWriter, body []byte, model string, stream bool, subPath string, isClaudeRoute bool, captureUserText string, route *config.ModelRoute, rt config.ResolvedTarget, attempt int, moreTargets bool) forwardOutcome {
	up := rt.Provider
	apiKeyID := apiKeyIDFromContext(r.Context())
	start := time.Now()

	// Live concurrency gauge: released when this function returns, whatever the
	// outcome, so the in-flight count cannot leak on an early error return.
	releaseInFlight := metrics.BeginInFlight(up.ID, up.Name)
	defer releaseInFlight()

	// Token usage and TTFB are filled in as the relay progresses; recordMetric
	// reads whatever was learned by the time it runs.
	var usage usageCounts
	var ttfb time.Duration
	// upstreamErrMsg holds the upstream's own error text for non-2xx responses,
	// so the recorded event explains the failure instead of showing a bare code.
	var upstreamErrMsg string
	// streamTruncated marks a 200 stream that ended with no terminal frame (see
	// usageScanner.Truncated). It downgrades the recorded outcome to a failure and
	// appends an SSE error frame, so neither the dashboard nor the client mistakes
	// a half-delivered turn for a complete one. truncatedHadContent only shapes the
	// message: it says whether any answer text made it out before the stream died.
	var streamTruncated bool
	var truncatedHadContent bool

	// recordMetric is called once, at the end of the attempt (after the body/stream
	// finishes), so LatencyMs reflects the full relay — not just time-to-headers.
	// Every attempt records its own event, so a failover shows both the failure and
	// the eventual success against their respective providers.
	recordMetric := func(status int, ok bool, errMsg string) {
		ev := metrics.Event{
			ClientModel:  model,
			TargetModel:  strings.TrimSpace(rt.Target.TargetModel),
			RouteID:      route.ID,
			ProviderID:   up.ID,
			ProviderName: up.Name,
			Endpoint:     forwardEndpointKind(isClaudeRoute),
			Status:       status,
			LatencyMs:    time.Since(start).Milliseconds(),
			TTFBMs:       ttfb.Milliseconds(),
			InputTokens:  usage.Input,
			OutputTokens: usage.Output,
			CostUSD:      up.CostUSD(usage.Input, usage.Output),
			Stream:       stream,
			Canceled:     status == 499,
			Ok:           ok,
			ErrorMsg:     errMsg,
			Attempt:      attempt,
		}
		metrics.Record(ev)
	}

	// failed records a failure that produced no client output. The caller decides
	// whether to retry or to surface clientMsg. The raw upstream error is never
	// used as clientMsg: it can leak upstream host/IP/proxy topology.
	failed := func(status int, clientMsg, internalMsg string, retryable bool) forwardOutcome {
		recordMetric(status, false, internalMsg)
		return forwardOutcome{
			retryable: retryable, status: status,
			clientMsg: clientMsg, internalMsg: internalMsg,
		}
	}

	// Optionally rewrite the "model" field before forwarding. A malformed body is
	// not retryable — every provider would reject it identically.
	payload := body
	if tm := strings.TrimSpace(rt.Target.TargetModel); tm != "" {
		var m map[string]interface{}
		if err := json.Unmarshal(body, &m); err != nil {
			return failed(400, "invalid request body", "invalid_request_error: "+err.Error(), false)
		}
		m["model"] = tm
		rewritten, err := json.Marshal(m)
		if err != nil {
			return failed(500, "failed to prepare upstream request", "failed to rewrite model: "+err.Error(), false)
		}
		payload = rewritten
	}

	url := strings.TrimRight(up.BaseURL, "/") + subPath
	// Propagate the client's context so a client disconnect cancels the upstream
	// call and the relayed stream (no orphaned upstream request, no wasted quota).
	req, err := http.NewRequestWithContext(r.Context(), "POST", url, bytes.NewReader(payload))
	if err != nil {
		// A bad BaseURL is this target's problem alone, so another target may work.
		return failed(500, "failed to prepare upstream request", "build request: "+err.Error(), true)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if up.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.ApiKey)
		req.Header.Set("X-Api-Key", up.ApiKey)
	}
	if isClaudeRoute {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	// Forward a curated allow-list of client headers. Never forward the client's
	// own Authorization/X-Api-Key (those authenticate to THIS proxy; the upstream
	// credential is set above from the route config).
	forwardRequestHeaders(req, r)

	proxyURL := up.ProxyURL
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	client := GetForwardClientForProxy(proxyURL)

	logger.Infof("[Forward] %s -> %s (%s)", model, up.Name, url)

	resp, err := client.Do(req)
	if err != nil {
		// Client cancellation is expected, not an upstream failure — and must not
		// trigger failover: the caller is gone, so there is nobody to serve.
		if r.Context().Err() != nil {
			logger.Debugf("[Forward] client canceled request to %s", up.Name)
			recordMetric(499, false, "client canceled")
			return forwardOutcome{canceled: true, status: 499}
		}
		logger.Warnf("[Forward] upstream request to %s failed: %v", up.Name, err)
		// Transport-level failure: the classic failover case.
		return failed(502, "upstream request failed", "upstream do: "+err.Error(), true)
	}
	defer resp.Body.Close()

	// Headers are in: this is the upstream's time-to-first-byte. For streams the
	// remaining latency is generation time, so the two figures separate a slow
	// provider from a long answer.
	ttfb = time.Since(start)

	ok := resp.StatusCode == 200

	// Retryable non-2xx WITH a backup available: drain a bounded prefix of the error
	// body for the log and return WITHOUT writing anything to the client, so the
	// next target gets a clean response writer. When this is the last target we
	// fall through instead and relay the body verbatim — a client that is about to
	// receive an error deserves the upstream's own error shape, which is what the
	// single-target code always did.
	if !ok && moreTargets && retryableUpstreamStatus(resp.StatusCode) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorBodyLimit))
		upstreamErrMsg = upstreamErrorSummary(errBody)
		logger.Warnf("[Forward] %s returned %d: %s", up.Name, resp.StatusCode, upstreamErrMsg)
		return failed(resp.StatusCode, "upstream request failed", upstreamErrMsg, true)
	}

	// From here on we are committed to this target: everything below writes to the
	// client, so the outcome is committed=true regardless of how the relay ends.

	// A non-200 upstream response is an error body (usually JSON), not an SSE
	// stream — copy it through verbatim regardless of the client's stream flag.
	var relayErr error
	switch {
	case stream && resp.StatusCode == 200:
		// Stream forward: relay bytes verbatim, teeing them through a usage scanner
		// that watches for the trailing usage frame. The client is written first and
		// the scanner observes afterwards, so accounting cannot delay or alter the
		// relay. Capture is intentionally skipped here (we do not buffer a whole
		// stream); inject still applied at the call site.
		scanner := &usageScanner{}
		relayErr = h.streamUpstreamResponse(w, resp, scanner)
		usage = scanner.Counts()
		// A stream that ended without any terminal frame is a truncation, even
		// though the read returned a clean io.EOF. Without this check the relay
		// below records a 200 success and the client is left with a stream that
		// simply stops — the failure is invisible to both sides. Only meaningful
		// when the read itself succeeded and the client is still connected: a
		// relayErr or a canceled context already explains the short stream.
		if relayErr == nil && r.Context().Err() == nil && scanner.Truncated() {
			streamTruncated = true
			truncatedHadContent = scanner.hadContent()
		}
	case ok && captureUserText != "" && config.MemoryCaptureEnabled() && resp.Header.Get("Content-Encoding") == "":
		// Non-stream success with capture on: buffer the body so we can BOTH relay it
		// to the client and extract the assistant text for memory capture. Compressed
		// bodies are excluded (fall to the plain copy) to avoid decompressing here.
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			logger.Warnf("[Forward] response read error: %v", readErr)
			relayErr = readErr
			break
		}
		usage = usageFromJSONBody(bodyBytes)
		relayErr = writeUpstreamResponseBytes(w, resp, bodyBytes)
		if relayErr == nil {
			if answer := extractForwardedAssistantText(bodyBytes); answer != "" {
				h.captureTurnAsync(memoryScopeForRequest(r.Context()), captureUserText, answer)
			}
		}
	case ok && resp.Header.Get("Content-Encoding") == "":
		// Plain non-stream success: buffer the (small) JSON body so the usage object
		// can be read, then relay it byte-for-byte. Compressed bodies fall through to
		// the streaming copy below rather than being decompressed here.
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			logger.Warnf("[Forward] response read error: %v", readErr)
			relayErr = readErr
			break
		}
		usage = usageFromJSONBody(bodyBytes)
		relayErr = writeUpstreamResponseBytes(w, resp, bodyBytes)
	case !ok && resp.Header.Get("Content-Encoding") == "":
		// A non-2xx we are surfacing to the client: either non-retryable
		// (400/401/403/404...) or retryable on the last target. Buffer the body so the
		// upstream's own explanation ("invalid api key", "rate limited") can be
		// attached to the recorded event. Without this the admin panel shows a bare
		// status code with no reason, which is the first thing an operator needs. The
		// body is still relayed byte-for-byte; only a short prefix is kept for the log.
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			logger.Warnf("[Forward] error-body read failed: %v", readErr)
			relayErr = readErr
			break
		}
		upstreamErrMsg = upstreamErrorSummary(bodyBytes)
		relayErr = writeUpstreamResponseBytes(w, resp, bodyBytes)
	default:
		relayErr = copyUpstreamResponse(w, resp)
	}

	// A truncated stream is not a success, however clean the transport looked.
	// Tell the client explicitly — an SSE error frame is the only way to turn a
	// stream that silently stops into a visible failure — and record it as such so
	// the provider's success rate reflects reality. Appended after the relayed
	// bytes, which the client already has; this cannot un-send them, only explain
	// why nothing more is coming.
	if streamTruncated {
		reason := "upstream stream ended without a terminal event (truncated response)"
		if !truncatedHadContent {
			reason = "upstream stream ended before any answer content (truncated response)"
		}
		logger.Warnf("[Forward] %s: %s", up.Name, reason)
		h.sendForwardStreamError(w, isClaudeRoute, reason)
		h.recordFailure()
		recordMetric(resp.StatusCode, false, reason)
		return forwardOutcome{committed: true, status: resp.StatusCode}
	}

	// Account usage AFTER a successful relay. Forwarded traffic still consumes the
	// key's quota: previously recordSuccess(0,0,0) left the per-key counter flat,
	// so a within-limit key could forward without ever approaching its limit. We
	// count the forwarded response's tokens when the upstream reports them, else
	// fall back to a request-count charge (1 unit) so usage still advances. No
	// fabricated pricing; multiplier defaults to 1.
	if ok && relayErr == nil {
		inTok, outTok := forwardedUsageTokens(resp, usage)
		h.recordSuccessForApiKey(apiKeyID, inTok, outTok, 0)
	} else if !ok {
		h.recordFailure()
	}

	// Record the metric at the very end, so LatencyMs covers the full relay
	// (time-to-first-byte + stream duration), not just time-to-headers.
	//
	// A client that disconnects mid-relay is not an upstream failure: the upstream
	// answered (often 200) and we simply stopped being able to write. Recording the
	// upstream's status here would file a "200 error" in the status histogram and
	// the recent-errors list, and would count against the provider's success rate
	// and fail streak. Attribute it to the client with 499 instead. Only the
	// client's context proves cancellation — relayErr alone can also mean a genuine
	// upstream read failure mid-stream, which must stay a real failure.
	if relayErr != nil && r.Context().Err() != nil {
		logger.Debugf("[Forward] client disconnected mid-relay from %s", up.Name)
		recordMetric(499, false, "client canceled")
		return forwardOutcome{committed: true, canceled: true, status: 499}
	}
	internalErr := ""
	if relayErr != nil {
		internalErr = relayErr.Error()
	} else if !ok {
		// The relay itself succeeded; the failure is the upstream's, so report what
		// the upstream said rather than leaving the reason blank.
		internalErr = upstreamErrMsg
	}
	recordMetric(resp.StatusCode, ok && relayErr == nil, internalErr)
	return forwardOutcome{committed: true, status: resp.StatusCode}
}

// sendForwardStreamError appends an error frame to an already-committed relayed
// stream. Headers and body bytes are long gone, so the HTTP status cannot be
// changed — the only remaining channel to the client is another SSE event.
//
// Each dialect gets the error shape its clients parse: Anthropic clients read a
// named `error` event, OpenAI clients read an error object in a data frame
// followed by [DONE] to close the stream cleanly. Best-effort by nature: if the
// connection is already gone the writes fail silently, which is correct — there
// is nobody left to inform.
func (h *Handler) sendForwardStreamError(w http.ResponseWriter, isClaudeRoute bool, message string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	if isClaudeRoute {
		payload, err := json.Marshal(map[string]interface{}{
			"type": "error",
			"error": map[string]string{
				"type":    "api_error",
				"message": message,
			},
		})
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
		flusher.Flush()
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"error": map[string]string{
			"type":    "server_error",
			"message": message,
		},
	})
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	flusher.Flush()
}

// upstreamErrorBodyLimit bounds how much of a retryable error body is read for
// the log line. The body is discarded rather than relayed in that path, so only
// enough to explain the failure is needed; upstreamErrorSummary truncates further.
const upstreamErrorBodyLimit = 64 << 10

// upstreamErrorSummary extracts a short, human-readable reason from an upstream
// error body. It understands the common JSON shapes ({"error":{"message":...}},
// {"error":"..."}, {"message":...}) and otherwise falls back to the raw text.
//
// The result is truncated: it is stored in a bounded in-memory ring and rendered
// in a table cell, so an upstream returning an HTML error page or a megabyte of
// prose must not bloat the metrics store.
func upstreamErrorSummary(body []byte) string {
	const maxLen = 300

	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}

	var env struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal(body, &env) == nil {
		if len(env.Error) > 0 {
			// "error" is either a string or an object carrying the message.
			var asString string
			if json.Unmarshal(env.Error, &asString) == nil && strings.TrimSpace(asString) != "" {
				return truncateForLog(asString, maxLen)
			}
			var asObject struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			}
			if json.Unmarshal(env.Error, &asObject) == nil {
				if m := strings.TrimSpace(asObject.Message); m != "" {
					if k := strings.TrimSpace(asObject.Type); k != "" {
						return truncateForLog(k+": "+m, maxLen)
					}
					return truncateForLog(m, maxLen)
				}
			}
		}
		if m := strings.TrimSpace(env.Message); m != "" {
			return truncateForLog(m, maxLen)
		}
		if m := strings.TrimSpace(env.Detail); m != "" {
			return truncateForLog(m, maxLen)
		}
	}
	return truncateForLog(trimmed, maxLen)
}

// truncateForLog shortens s to at most max runes, marking that it was cut.
// It counts runes, not bytes, so a multi-byte message is never split mid-character.
func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// forwardedUsageTokens decides what to charge the API key's quota for a
// forwarded request. Precedence:
//
//  1. usage parsed from the upstream response body (the accurate source, and now
//     the common case for both stream and non-stream relays);
//  2. the X-Usage-Input-Tokens / X-Usage-Output-Tokens response headers, which a
//     few gateways set;
//  3. a single request unit (outputTokens=1) so a forwarded request still
//     advances the key's usage counter.
//
// It never fabricates token pricing.
func forwardedUsageTokens(resp *http.Response, parsed usageCounts) (inTok, outTok int) {
	if parsed.known() {
		return int(parsed.Input), int(parsed.Output)
	}
	inTok = atoiHeader(resp.Header.Get("X-Usage-Input-Tokens"))
	outTok = atoiHeader(resp.Header.Get("X-Usage-Output-Tokens"))
	if inTok == 0 && outTok == 0 {
		// No upstream usage signal: charge one request unit so usage is non-zero
		// and per-key limits still apply to forwarded traffic.
		outTok = 1
	}
	return inTok, outTok
}

// forwardEndpointKind labels a forwarded request for the stats breakdown.
func forwardEndpointKind(isClaudeRoute bool) string {
	if isClaudeRoute {
		return "claude"
	}
	return "openai"
}

func atoiHeader(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// forwardableRequestHeaders is the allow-list of client request headers relayed to
// the upstream. The client's Authorization / X-Api-Key are deliberately excluded
// (they authenticate to this proxy, not the upstream).
var forwardableRequestHeaders = []string{
	"Anthropic-Beta",
	"Anthropic-Version",
	"Idempotency-Key",
	"X-Request-Id",
	"X-Stainless-Os",
	"X-Stainless-Lang",
	"X-Stainless-Package-Version",
}

func forwardRequestHeaders(dst *http.Request, src *http.Request) {
	for _, h := range forwardableRequestHeaders {
		if v := src.Header.Get(h); v != "" {
			dst.Header.Set(h, v)
		}
	}
}

// forwardableResponseHeaders is the allow-list of upstream response headers relayed
// back to the client (rate-limit / retry / request-id / content metadata).
var forwardableResponseHeaders = []string{
	"Retry-After",
	"X-Request-Id",
	"X-Ratelimit-Limit-Requests",
	"X-Ratelimit-Limit-Tokens",
	"X-Ratelimit-Remaining-Requests",
	"X-Ratelimit-Remaining-Tokens",
	"X-Ratelimit-Reset-Requests",
	"X-Ratelimit-Reset-Tokens",
}

func copyForwardableResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, h := range forwardableResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}

// streamUpstreamResponse relays an already-SSE upstream response to the client,
// flushing after each chunk. The upstream emits fully-formed event/data frames,
// so no re-marshaling is needed.
//
// observer, when non-nil, receives a copy of every relayed chunk after it has
// been written and flushed to the client. It is used to extract token usage from
// the trailing usage frame; it can neither modify nor delay the relay, and its
// writes never fail the request.
func (h *Handler) streamUpstreamResponse(w http.ResponseWriter, resp *http.Response, observer io.Writer) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return nil
	}
	copyForwardableResponseHeaders(w, resp)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	buf := make([]byte, 16*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			flusher.Flush()
			if observer != nil {
				// Errors are impossible for usageScanner and irrelevant in any
				// case: the client already has these bytes.
				_, _ = observer.Write(buf[:n])
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				logger.Warnf("[Forward] stream read error: %v", readErr)
				return readErr
			}
			return nil
		}
	}
}

// copyUpstreamResponse relays a non-streaming upstream response, preserving the
// status code, content type/encoding, and an allow-list of metadata headers.
func copyUpstreamResponse(w http.ResponseWriter, resp *http.Response) error {
	copyForwardableResponseHeaders(w, resp)
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		w.Header().Set("Content-Encoding", enc)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Warnf("[Forward] response copy error: %v", err)
		return err
	}
	return nil
}

// writeUpstreamResponseBytes relays an already-buffered non-stream response body,
// preserving the status code and the same header set as copyUpstreamResponse. Used
// on the capture path, where the body was read up front so it could be teed.
func writeUpstreamResponseBytes(w http.ResponseWriter, resp *http.Response, body []byte) error {
	copyForwardableResponseHeaders(w, resp)
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		w.Header().Set("Content-Encoding", enc)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		logger.Warnf("[Forward] response write error: %v", err)
		return err
	}
	return nil
}

// extractForwardedAssistantText pulls the assistant's answer text out of a
// forwarded non-stream response body for memory capture. It tolerates both the
// Anthropic Messages shape ({"content":[{"type":"text","text":...}]}) and the
// OpenAI chat shape ({"choices":[{"message":{"content":...}}]}). Returns "" when
// no text is found (capture then skips). Best-effort: never errors.
func extractForwardedAssistantText(body []byte) string {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	// Anthropic Messages: content is an array of blocks.
	if blocks, ok := m["content"].([]interface{}); ok {
		var sb strings.Builder
		for _, b := range blocks {
			block, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t != "" && t != "text" {
				continue
			}
			if txt, ok := block["text"].(string); ok {
				sb.WriteString(txt)
			}
		}
		if s := strings.TrimSpace(sb.String()); s != "" {
			return s
		}
	}
	// OpenAI chat: choices[].message.content (string).
	if choices, ok := m["choices"].([]interface{}); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			msg, ok := choice["message"].(map[string]interface{})
			if !ok {
				continue
			}
			if txt, ok := msg["content"].(string); ok && strings.TrimSpace(txt) != "" {
				return strings.TrimSpace(txt)
			}
		}
	}
	return ""
}
