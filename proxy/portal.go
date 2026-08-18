package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/apikey"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Overridable in tests so heartbeat / re-auth do not wait production intervals.
var (
	portalSSEPing   = 25 * time.Second
	portalSSEReauth = 5 * time.Second
)

const portalCookieName = "portal_session"
const portalCookiePath = "/portal"

func (h *Handler) handleUsagePage(w http.ResponseWriter, r *http.Request) {
	if !config.IsPortalEnabled() {
		http.NotFound(w, r)
		return
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/usage/p/") {
		token := strings.TrimPrefix(path, "/usage/p/")
		token = strings.Trim(token, "/")
		h.exchangePortalToken(w, r, token)
		return
	}
	if path == "/usage" || path == "/usage/" {
		http.ServeFile(w, r, "web/usage.html")
		return
	}
	// Static siblings: /usage/usage.js, /usage/usage.css
	rel := strings.TrimPrefix(path, "/usage/")
	if rel == "" || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "web/"+rel)
}

func (h *Handler) exchangePortalToken(w http.ResponseWriter, r *http.Request, token string) {
	if h.keys == nil {
		http.Error(w, "portal unavailable", http.StatusServiceUnavailable)
		return
	}
	sid, _, err := h.keys.OpenSessionByPortalToken(token)
	if err != nil {
		http.Redirect(w, r, "/usage?error=invalid", http.StatusSeeOther)
		return
	}
	h.setPortalCookie(w, r, sid)
	http.Redirect(w, r, "/usage", http.StatusSeeOther)
}

func (h *Handler) handlePortal(w http.ResponseWriter, r *http.Request) {
	if !config.IsPortalEnabled() || h.keys == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found", "code": "internal_error"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/portal/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch {
	case path == "/session" && r.Method == http.MethodPost:
		h.portalOpenSession(w, r)
	case path == "/session/token" && r.Method == http.MethodPost:
		h.portalOpenSessionToken(w, r)
	case path == "/session" && r.Method == http.MethodDelete:
		h.portalCloseSession(w, r)
	case path == "/me" && r.Method == http.MethodGet:
		h.portalMe(w, r)
	case path == "/summary" && r.Method == http.MethodGet:
		h.portalSummary(w, r)
	case path == "/usage" && r.Method == http.MethodGet:
		h.portalUsage(w, r)
	case path == "/events" && r.Method == http.MethodGet:
		h.portalEvents(w, r)
	case path == "/events/stream" && r.Method == http.MethodGet:
		h.portalEventStream(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found", "code": "internal_error"})
	}
}

func (h *Handler) setPortalCookie(w http.ResponseWriter, r *http.Request, sid string) {
	secure := config.IsTLSEnabled() || r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:     portalCookieName,
		Value:    sid,
		Path:     portalCookiePath,
		MaxAge:   int((12 * time.Hour).Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearPortalCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     portalCookieName,
		Value:    "",
		Path:     portalCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   config.IsTLSEnabled() || r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) portalSessionRecord(r *http.Request) (apikey.Record, error) {
	c, err := r.Cookie(portalCookieName)
	if err != nil || c.Value == "" {
		return apikey.Record{}, apikey.ErrSession
	}
	return h.keys.SessionRecord(c.Value)
}

func writePortalErr(w http.ResponseWriter, err error) {
	var qe *apikey.QueryError
	if errors.As(err, &qe) {
		apikey.IncPortalQueryError()
		w.WriteHeader(qe.Status)
		json.NewEncoder(w).Encode(map[string]string{"error": qe.Message, "code": qe.Code})
		return
	}
	if ae, ok := err.(*apikey.AuthError); ok {
		w.WriteHeader(ae.Status)
		json.NewEncoder(w).Encode(map[string]string{"error": ae.Message, "code": ae.Machine})
		return
	}
	if err == apikey.ErrSession {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "portal session expired", "code": "portal_session_expired"})
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "code": "invalid_api_key"})
}

func writePortalDataErr(w http.ResponseWriter, err error) {
	var qe *apikey.QueryError
	if errors.As(err, &qe) {
		writePortalErr(w, err)
		return
	}
	apikey.IncPortalQueryError()
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": "internal error", "code": "internal_error"})
}

func portalSessionID(r *http.Request) string {
	c, err := r.Cookie(portalCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func (h *Handler) portalOpenSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON", "code": "invalid_api_key"})
		return
	}
	sid, rec, err := h.keys.OpenSessionByKey(req.Key)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	h.setPortalCookie(w, r, sid)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "name": rec.Key.Name, "keyMasked": rec.Masked()})
}

func (h *Handler) portalOpenSessionToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON", "code": "invalid_portal_token"})
		return
	}
	sid, rec, err := h.keys.OpenSessionByPortalToken(req.Token)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	h.setPortalCookie(w, r, sid)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "name": rec.Key.Name, "keyMasked": rec.Masked()})
}

func (h *Handler) portalCloseSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(portalCookieName); err == nil {
		_ = h.keys.CloseSession(c.Value)
	}
	h.clearPortalCookie(w, r)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (h *Handler) portalMe(w http.ResponseWriter, r *http.Request) {
	rec, err := h.portalSessionRecord(r)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"name":      rec.Key.Name,
		"keyMasked": rec.Masked(),
		"status":    rec.Status(time.Now()),
		"enabled":   rec.Key.Enabled,
		"expiresAt": unixOrNil(rec.Key.ExpiresAt),
	})
}

func (h *Handler) portalSummary(w http.ResponseWriter, r *http.Request) {
	rec, err := h.portalSessionRecord(r)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	sum, err := h.keys.Summary(rec.Key.ID)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	json.NewEncoder(w).Encode(portalSummaryJSON(sum))
}

func portalSummaryJSON(sum apikey.Summary) map[string]interface{} {
	rec := sum.Record
	out := map[string]interface{}{
		"name":                  rec.Key.Name,
		"keyMasked":             rec.Masked(),
		"status":                sum.Status,
		"successRate":           sum.SuccessRate,
		"avgLatencyMs":          sum.AvgLatencyMs,
		"avgTtfbMs":             sum.AvgTTFBMs,
		"lastUsedAt":            unixOrNil(rec.Key.LastUsedAt),
		"expiresAt":             unixOrNil(rec.Key.ExpiresAt),
		"nextReset":             unixOrNil(sum.NextReset),
		"resetPolicy":           rec.Quota.ResetPolicy,
		"tokensUsed":            rec.Usage.TotalTokens,
		"tokenLimit":            rec.Quota.TokenLimit,
		"creditsUsed":           rec.Usage.Credits,
		"creditLimit":           rec.Quota.CreditLimit,
		"requestsUsed":          rec.Usage.RequestsTotal,
		"requestLimit":          rec.Quota.RequestLimit,
		"requestsSuccess":       rec.Usage.RequestsSuccess,
		"requestsFailed":        rec.Usage.RequestsFailed,
		"requestsCancelled":     rec.Usage.RequestsCancelled,
		"requestsRejected":      rec.Usage.RequestsRejected,
		"requestsAttempted":     rec.Usage.RequestsAttempted(),
		"requestsQuotaConsumed": rec.Usage.RequestsQuotaConsumed(),
		"tokenEnforcement":      apikey.V1TokenCreditEnforcement,
		"creditEnforcement":     apikey.V1TokenCreditEnforcement,
		"requestEnforcement":    apikey.RequestEnforcementFor(rec.Quota.RequestLimit),
	}
	if v := rec.RemainingTokens(); v != nil {
		out["tokensRemaining"] = *v
	}
	if v := rec.RemainingCredits(); v != nil {
		out["creditsRemaining"] = *v
	}
	if v := rec.RemainingRequests(); v != nil {
		out["requestsRemaining"] = *v
	}
	return out
}

func unixOrNil(t *time.Time) interface{} {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

func (h *Handler) portalUsage(w http.ResponseWriter, r *http.Request) {
	rec, err := h.portalSessionRecord(r)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	q, err := apikey.ParseEventQuery(r.URL.Query(), time.Now().UTC())
	if err != nil {
		writePortalErr(w, err)
		return
	}
	series, err := h.keys.UsageSeries(rec.Key.ID, q)
	if err != nil {
		writePortalDataErr(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"range":            q.Range,
		"from":             series.From,
		"to":               series.To,
		"resolution":       series.Resolution,
		"bucketSeconds":    series.BucketSeconds,
		"source":           series.Source,
		"truncated":        series.Truncated,
		"rawAvailableFrom": series.RawAvailableFrom,
		"dataRetention":    series.DataRetention,
		"metrics":          series.Metrics,
		"points":           series.Points,
	})
}

func (h *Handler) portalEvents(w http.ResponseWriter, r *http.Request) {
	rec, err := h.portalSessionRecord(r)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	q, err := apikey.ParseEventQuery(r.URL.Query(), time.Now().UTC())
	if err != nil {
		writePortalErr(w, err)
		return
	}
	page, err := h.keys.ListEvents(rec.Key.ID, q)
	if err != nil {
		writePortalDataErr(w, err)
		return
	}
	items := make([]apikey.PublicEvent, len(page.Items))
	for i, ev := range page.Items {
		items[i] = ev.PortalView()
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"items":            items,
		"nextCursor":       page.NextCursor,
		"hasMore":          page.HasMore,
		"from":             page.From,
		"to":               page.To,
		"resolution":       page.Resolution,
		"bucketSeconds":    page.BucketSeconds,
		"source":           page.Source,
		"truncated":        page.Truncated,
		"rawAvailableFrom": page.RawAvailableFrom,
		"dataRetention":    page.DataRetention,
	})
}

func (h *Handler) portalEventStream(w http.ResponseWriter, r *http.Request) {
	rec, err := h.portalSessionRecord(r)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	sid := portalSessionID(r)
	filter, err := apikey.ParseEventStreamQuery(r.URL.Query(), time.Now().UTC())
	if err != nil {
		writePortalErr(w, err)
		return
	}
	lastID := parseLastEventID(r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Streaming not supported", "code": "internal_error"})
		return
	}
	stream, err := h.keys.OpenPortalStream(rec.Key.ID, lastID, filter)
	if err != nil {
		writePortalDataErr(w, err)
		return
	}
	defer stream.Cancel()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	seen := make(map[int64]struct{}, len(stream.Replay)+16)
	writePub := func(ev apikey.PublicEvent) {
		if ev.EventID != 0 {
			if _, dup := seen[ev.EventID]; dup {
				return
			}
			seen[ev.EventID] = struct{}{}
			if len(seen) > 4096 {
				for id := range seen {
					if id <= stream.HighWater {
						delete(seen, id)
					}
				}
			}
		}
		if !filter.Match(ev) {
			return
		}
		data, _ := json.Marshal(ev.PortalView())
		fmt.Fprintf(w, "id: %d\nevent: request\ndata: %s\n\n", ev.EventID, data)
		flusher.Flush()
	}

	// retry: is required so EventSource does not use an undefined default
	// after a proxy drop. 4xx is forbidden here: browsers stop reconnecting.
	fmt.Fprintf(w, "retry: 3000\n: connected\n\n")
	flusher.Flush()
	for _, ev := range stream.Replay {
		writePub(ev)
	}
	if stream.Truncated {
		payload, _ := json.Marshal(map[string]interface{}{
			"reason":      "replay_truncated",
			"lastEventId": stream.LastEventID,
			"highWater":   stream.HighWater,
			"replayed":    len(stream.Replay),
		})
		// Advance Last-Event-ID to highWater so the next browser reconnect
		// does not re-request the same overflowed backlog.
		fmt.Fprintf(w, "id: %d\nevent: sync_required\ndata: %s\n\n", stream.HighWater, payload)
		flusher.Flush()
	}

	exp, expErr := h.keys.SessionExpiry(sid)
	deadline := time.Now().Add(12 * time.Hour)
	if expErr == nil && exp.Before(deadline) {
		deadline = exp
	}
	life := time.NewTimer(time.Until(deadline))
	defer life.Stop()

	ping := time.NewTicker(portalSSEPing)
	defer ping.Stop()
	reauth := time.NewTicker(portalSSEReauth)
	defer reauth.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-life.C:
			fmt.Fprintf(w, "event: session\ndata: {\"code\":\"portal_session_expired\"}\n\n")
			flusher.Flush()
			return
		case <-reauth.C:
			if _, err := h.keys.SessionRecord(sid); err != nil {
				fmt.Fprintf(w, "event: session\ndata: {\"code\":\"portal_session_expired\"}\n\n")
				flusher.Flush()
				return
			}
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-stream.Live:
			if !ok {
				return
			}
			writePub(ev)
		}
	}
}

func parseLastEventID(r *http.Request) int64 {
	if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("after")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}
