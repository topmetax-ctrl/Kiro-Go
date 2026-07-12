package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/metrics"
	"net/http"
	"strings"
	"time"
)

// tryForwardUpstream checks whether the given client model matches an enabled
// forward route. If so, it passes the raw request body through to the route's
// upstream provider (an OpenAI/Anthropic-compatible endpoint such as 9router or
// xpiki), streams or copies the response back to the client, and returns true.
//
// When there is no matching enabled route it returns false and the caller
// continues with the normal Kiro dispatch path. Forwarded requests bypass the
// account pool and per-API-key quota, but are still counted in the global
// request/success/failure stats. Token counts are not tracked for passthroughs.
//
// subPath is the upstream path appended to the provider BaseURL (e.g.
// "/messages", "/chat/completions", "/responses"). isClaudeRoute selects the
// error-response shape (Anthropic vs OpenAI) on failure.
func (h *Handler) tryForwardUpstream(r *http.Request, w http.ResponseWriter, body []byte, model string, stream bool, subPath string, isClaudeRoute bool) bool {
	route, up := config.FindEnabledRoute(model)
	if route == nil || up == nil {
		return false
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	start := time.Now()
	// recordMetric is called once, at the end of the request (after the body/stream
	// finishes), so LatencyMs reflects the full relay — not just time-to-headers.
	recordMetric := func(status int, ok bool, errMsg string) {
		metrics.Record(metrics.Event{
			ClientModel:  model,
			TargetModel:  strings.TrimSpace(route.TargetModel),
			RouteID:      route.ID,
			ProviderID:   up.ID,
			ProviderName: up.Name,
			Status:       status,
			LatencyMs:    time.Since(start).Milliseconds(),
			Stream:       stream,
			Ok:           ok,
			ErrorMsg:     errMsg,
		})
	}

	// sendErr returns a generic client-facing message (never the raw upstream
	// error, which can leak upstream host/IP/proxy topology). The detailed error is
	// logged internally by the caller.
	sendErr := func(status int, clientMsg, internalMsg string) {
		h.recordFailure()
		recordMetric(status, false, internalMsg)
		if isClaudeRoute {
			h.sendClaudeError(w, status, "api_error", clientMsg)
		} else {
			h.sendOpenAIError(w, status, "server_error", clientMsg)
		}
	}

	// Optionally rewrite the "model" field before forwarding.
	payload := body
	if tm := strings.TrimSpace(route.TargetModel); tm != "" {
		var m map[string]interface{}
		if err := json.Unmarshal(body, &m); err != nil {
			sendErr(400, "invalid request body", "invalid_request_error: "+err.Error())
			return true
		}
		m["model"] = tm
		rewritten, err := json.Marshal(m)
		if err != nil {
			sendErr(500, "failed to prepare upstream request", "failed to rewrite model: "+err.Error())
			return true
		}
		payload = rewritten
	}

	url := strings.TrimRight(up.BaseURL, "/") + subPath
	// Propagate the client's context so a client disconnect cancels the upstream
	// call and the relayed stream (no orphaned upstream request, no wasted quota).
	req, err := http.NewRequestWithContext(r.Context(), "POST", url, bytes.NewReader(payload))
	if err != nil {
		sendErr(500, "failed to prepare upstream request", "build request: "+err.Error())
		return true
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
		// Client cancellation is expected, not an upstream failure.
		if r.Context().Err() != nil {
			logger.Debugf("[Forward] client canceled request to %s", up.Name)
			recordMetric(499, false, "client canceled")
			return true
		}
		logger.Warnf("[Forward] upstream request to %s failed: %v", up.Name, err)
		sendErr(502, "upstream request failed", "upstream do: "+err.Error())
		return true
	}
	defer resp.Body.Close()

	ok := resp.StatusCode == 200

	// A non-200 upstream response is an error body (usually JSON), not an SSE
	// stream — copy it through verbatim regardless of the client's stream flag.
	var relayErr error
	if stream && resp.StatusCode == 200 {
		relayErr = h.streamUpstreamResponse(w, resp)
	} else {
		relayErr = copyUpstreamResponse(w, resp)
	}

	// Account usage AFTER a successful relay. Forwarded traffic still consumes the
	// key's quota: previously recordSuccess(0,0,0) left the per-key counter flat,
	// so a within-limit key could forward without ever approaching its limit. We
	// count the forwarded response's tokens when the upstream reports them, else
	// fall back to a request-count charge (1 unit) so usage still advances. No
	// fabricated pricing; multiplier defaults to 1.
	if ok && relayErr == nil {
		inTok, outTok := forwardedUsageTokens(resp)
		h.recordSuccessForApiKey(apiKeyID, inTok, outTok, 0)
	} else if !ok {
		h.recordFailure()
	}

	// Record the metric at the very end, so LatencyMs covers the full relay
	// (time-to-first-byte + stream duration), not just time-to-headers.
	internalErr := ""
	if relayErr != nil {
		internalErr = relayErr.Error()
	}
	recordMetric(resp.StatusCode, ok && relayErr == nil, internalErr)
	return true
}

// forwardedUsageTokens extracts input/output token counts from an upstream
// response when present. It reads the X-Usage-Input-Tokens / X-Usage-Output-Tokens
// response headers if the upstream provides them; otherwise it charges a single
// request unit (outputTokens=1) so a forwarded request still advances the key's
// usage counter. It never fabricates token pricing.
func forwardedUsageTokens(resp *http.Response) (inTok, outTok int) {
	inTok = atoiHeader(resp.Header.Get("X-Usage-Input-Tokens"))
	outTok = atoiHeader(resp.Header.Get("X-Usage-Output-Tokens"))
	if inTok == 0 && outTok == 0 {
		// No upstream usage signal: charge one request unit so usage is non-zero
		// and per-key limits still apply to forwarded traffic.
		outTok = 1
	}
	return inTok, outTok
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
func (h *Handler) streamUpstreamResponse(w http.ResponseWriter, resp *http.Response) error {
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
