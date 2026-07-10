package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
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
func (h *Handler) tryForwardUpstream(w http.ResponseWriter, body []byte, model string, stream bool, subPath string, isClaudeRoute bool) bool {
	route, up := config.FindEnabledRoute(model)
	if route == nil || up == nil {
		return false
	}

	sendErr := func(status int, message string) {
		h.recordFailure()
		if isClaudeRoute {
			h.sendClaudeError(w, status, "api_error", message)
		} else {
			h.sendOpenAIError(w, status, "server_error", message)
		}
	}

	// Optionally rewrite the "model" field before forwarding.
	payload := body
	if tm := strings.TrimSpace(route.TargetModel); tm != "" {
		var m map[string]interface{}
		if err := json.Unmarshal(body, &m); err != nil {
			sendErr(400, "invalid_request_error: "+err.Error())
			return true
		}
		m["model"] = tm
		rewritten, err := json.Marshal(m)
		if err != nil {
			sendErr(500, "failed to rewrite model: "+err.Error())
			return true
		}
		payload = rewritten
	}

	url := strings.TrimRight(up.BaseURL, "/") + subPath
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		sendErr(500, "failed to build upstream request: "+err.Error())
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

	proxyURL := up.ProxyURL
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	client := GetClientForProxy(proxyURL)

	logger.Infof("[Forward] %s -> %s (%s)", model, up.Name, url)

	resp, err := client.Do(req)
	if err != nil {
		logger.Warnf("[Forward] upstream request failed: %v", err)
		sendErr(502, "upstream request failed: "+err.Error())
		return true
	}
	defer resp.Body.Close()

	// Count the forward in the global stats (tokens are not tracked for
	// passthroughs). Per-API-key quota is intentionally left untouched.
	if resp.StatusCode == 200 {
		h.recordSuccess(0, 0, 0)
	} else {
		h.recordFailure()
	}

	// A non-200 upstream response is an error body (usually JSON), not an SSE
	// stream — copy it through verbatim regardless of the client's stream flag.
	if stream && resp.StatusCode == 200 {
		h.streamUpstreamResponse(w, resp)
	} else {
		copyUpstreamResponse(w, resp)
	}
	return true
}

// streamUpstreamResponse relays an already-SSE upstream response to the client,
// flushing after each chunk. The upstream emits fully-formed event/data frames,
// so no re-marshaling is needed.
func (h *Handler) streamUpstreamResponse(w http.ResponseWriter, resp *http.Response) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	buf := make([]byte, 16*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				logger.Warnf("[Forward] stream read error: %v", readErr)
			}
			return
		}
	}
}

// copyUpstreamResponse relays a non-streaming upstream response, preserving the
// status code and content type.
func copyUpstreamResponse(w http.ResponseWriter, resp *http.Response) {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Warnf("[Forward] response copy error: %v", err)
	}
}
