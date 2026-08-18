package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/apikey"
	"kiro-go/config"
	"kiro-go/metrics"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	from, to := portalRange(r.URL.Query().Get("range"), r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	series, err := h.keys.UsageSeries(rec.Key.ID, from, to)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "code": "internal_error"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"range": r.URL.Query().Get("range"), "from": from.Unix(), "to": to.Unix(), "points": series})
}

func portalRange(named, fromS, toS string) (time.Time, time.Time) {
	now := time.Now().UTC()
	to := now
	from := now.Add(-24 * time.Hour)
	switch strings.ToUpper(named) {
	case "LIVE", "1H":
		from = now.Add(-1 * time.Hour)
	case "6H":
		from = now.Add(-6 * time.Hour)
	case "24H", "":
		from = now.Add(-24 * time.Hour)
	case "7D":
		from = now.Add(-7 * 24 * time.Hour)
	case "30D":
		from = now.Add(-30 * 24 * time.Hour)
	case "CUSTOM":
		if n, err := strconv.ParseInt(fromS, 10, 64); err == nil && n > 0 {
			from = time.Unix(n, 0).UTC()
		}
		if n, err := strconv.ParseInt(toS, 10, 64); err == nil && n > 0 {
			to = time.Unix(n, 0).UTC()
		}
	}
	return from, to
}

func (h *Handler) portalEvents(w http.ResponseWriter, r *http.Request) {
	rec, err := h.portalSessionRecord(r)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	q := r.URL.Query()
	eq := apikey.EventQuery{
		Model:     q.Get("model"),
		Endpoint:  q.Get("endpoint"),
		Status:    q.Get("status"),
		ErrorCode: q.Get("error"),
	}
	if v := q.Get("limit"); v != "" {
		eq.Limit, _ = strconv.Atoi(v)
	}
	if v := q.Get("offset"); v != "" {
		eq.Offset, _ = strconv.Atoi(v)
	}
	if v := q.Get("stream"); v == "1" || v == "true" {
		t := true
		eq.Stream = &t
	} else if v == "0" || v == "false" {
		f := false
		eq.Stream = &f
	}
	from, to := portalRange(q.Get("range"), q.Get("from"), q.Get("to"))
	eq.From, eq.To = &from, &to
	items, total, err := h.keys.ListEvents(rec.Key.ID, eq)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "code": "internal_error"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"items": items, "total": total, "offset": eq.Offset, "limit": eq.Limit})
}

func (h *Handler) portalEventStream(w http.ResponseWriter, r *http.Request) {
	rec, err := h.portalSessionRecord(r)
	if err != nil {
		writePortalErr(w, err)
		return
	}
	keyID := rec.Key.ID
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Streaming not supported", "code": "internal_error"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writePub := func(ev apikey.PublicEvent) {
		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}

	recent, _, _ := h.keys.ListEvents(keyID, apikey.EventQuery{Limit: 20})
	for i := len(recent) - 1; i >= 0; i-- {
		writePub(recent[i])
	}
	flusher.Flush()

	ch, cancel := metrics.Subscribe()
	defer cancel()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.ApiKeyID != keyID {
				continue
			}
			status := apikey.OutcomeFailed
			if ev.Ok {
				status = apikey.OutcomeSuccess
			} else if ev.Canceled {
				status = apikey.OutcomeCancelled
			}
			writePub(apikey.ToPublic(ev.RequestID, time.UnixMilli(ev.TimeMs), ev.Endpoint, ev.ClientModel, ev.Status, status, ev.InputTokens, ev.OutputTokens, ev.CostUSD, ev.LatencyMs, ev.TTFBMs, ev.Stream, ""))
			flusher.Flush()
		}
	}
}
