package proxy

import (
	"encoding/json"
	"kiro-go/apikey"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// apiKeyView is the response payload for listing/inspecting API keys. The Key field
// is masked so admins can identify entries without exposing the secret.
type apiKeyView struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name,omitempty"`
	KeyMasked             string   `json:"keyMasked"`
	Enabled               bool     `json:"enabled"`
	Migrated              bool     `json:"migrated,omitempty"`
	CreatedAt             int64    `json:"createdAt"`
	LastUsedAt            int64    `json:"lastUsedAt,omitempty"`
	TokenLimit            int64    `json:"tokenLimit,omitempty"`
	CreditLimit           float64  `json:"creditLimit,omitempty"`
	RequestLimit          int64    `json:"requestLimit,omitempty"`
	ExpiresAt             int64    `json:"expiresAt,omitempty"`
	ResetPolicy           string   `json:"resetPolicy,omitempty"`
	EnforcementMode       string   `json:"enforcementMode,omitempty"`
	Status                string   `json:"status,omitempty"`
	TokensUsed            int64    `json:"tokensUsed"`
	CreditsUsed           float64  `json:"creditsUsed"`
	RequestsCount         int64    `json:"requestsCount"`
	RequestsSuccess       int64    `json:"requestsSuccess,omitempty"`
	RequestsFailed        int64    `json:"requestsFailed,omitempty"`
	RequestsCancelled     int64    `json:"requestsCancelled,omitempty"`
	RequestsRejected      int64    `json:"requestsRejected,omitempty"`
	RequestsAttempted     int64    `json:"requestsAttempted,omitempty"`
	RequestsQuotaConsumed int64    `json:"requestsQuotaConsumed,omitempty"`
	TokenEnforcement      string   `json:"tokenEnforcement,omitempty"`
	CreditEnforcement     string   `json:"creditEnforcement,omitempty"`
	RequestEnforcement    string   `json:"requestEnforcement,omitempty"`
	TokensRemaining       *int64   `json:"tokensRemaining,omitempty"`
	CreditsRemaining      *float64 `json:"creditsRemaining,omitempty"`
	RequestsRemaining     *int64   `json:"requestsRemaining,omitempty"`
	HasPortalToken        bool     `json:"hasPortalToken,omitempty"`
}

func toApiKeyView(e config.ApiKeyEntry) apiKeyView {
	return apiKeyView{
		ID:            e.ID,
		Name:          e.Name,
		KeyMasked:     config.MaskApiKey(e.Key),
		Enabled:       e.Enabled,
		Migrated:      e.Migrated,
		CreatedAt:     e.CreatedAt,
		LastUsedAt:    e.LastUsedAt,
		TokenLimit:    e.TokenLimit,
		CreditLimit:   e.CreditLimit,
		TokensUsed:    e.TokensUsed,
		CreditsUsed:   e.CreditsUsed,
		RequestsCount: e.RequestsCount,
	}
}

func toApiKeyViewRecord(rec apikey.Record, hasPortal bool) apiKeyView {
	v := apiKeyView{
		ID:                    rec.Key.ID,
		Name:                  rec.Key.Name,
		KeyMasked:             rec.Masked(),
		Enabled:               rec.Key.Enabled,
		Migrated:              rec.Key.Migrated,
		CreatedAt:             rec.Key.CreatedAt.Unix(),
		TokenLimit:            rec.Quota.TokenLimit,
		CreditLimit:           rec.Quota.CreditLimit,
		RequestLimit:          rec.Quota.RequestLimit,
		ResetPolicy:           rec.Quota.ResetPolicy,
		EnforcementMode:       rec.Quota.EnforcementMode,
		Status:                rec.Status(time.Now()),
		TokensUsed:            rec.Usage.TotalTokens,
		CreditsUsed:           rec.Usage.Credits,
		RequestsCount:         rec.Usage.RequestsTotal,
		RequestsSuccess:       rec.Usage.RequestsSuccess,
		RequestsFailed:        rec.Usage.RequestsFailed,
		RequestsCancelled:     rec.Usage.RequestsCancelled,
		RequestsRejected:      rec.Usage.RequestsRejected,
		RequestsAttempted:     rec.Usage.RequestsAttempted(),
		RequestsQuotaConsumed: rec.Usage.RequestsQuotaConsumed(),
		TokenEnforcement:      apikey.V1TokenCreditEnforcement,
		CreditEnforcement:     apikey.V1TokenCreditEnforcement,
		RequestEnforcement:    apikey.RequestEnforcementFor(rec.Quota.RequestLimit),
		TokensRemaining:       rec.RemainingTokens(),
		CreditsRemaining:      rec.RemainingCredits(),
		RequestsRemaining:     rec.RemainingRequests(),
		HasPortalToken:        hasPortal,
	}
	if rec.Key.LastUsedAt != nil {
		v.LastUsedAt = rec.Key.LastUsedAt.Unix()
	}
	if rec.Key.ExpiresAt != nil {
		v.ExpiresAt = rec.Key.ExpiresAt.Unix()
	}
	return v
}

func (h *Handler) apiListApiKeys(w http.ResponseWriter, r *http.Request) {
	if h.keys != nil {
		q := apikey.ListQuery{
			Q:      strings.TrimSpace(r.URL.Query().Get("q")),
			Status: r.URL.Query().Get("status"),
			Quota:  r.URL.Query().Get("quota"),
			Usage:  r.URL.Query().Get("usage"),
			Sort:   r.URL.Query().Get("sort"),
		}
		if v := r.URL.Query().Get("limit"); v != "" {
			q.Limit, _ = strconv.Atoi(v)
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			q.Offset, _ = strconv.Atoi(v)
		}
		items, total, err := h.keys.List(q)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		out := make([]apiKeyView, len(items))
		for i, rec := range items {
			out[i] = toApiKeyViewRecord(rec, h.keys.HasPortalToken(rec.Key.ID))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"apiKeys": out, "total": total})
		return
	}
	entries := config.ListApiKeys()
	out := make([]apiKeyView, len(entries))
	for i, e := range entries {
		out[i] = toApiKeyView(e)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"apiKeys": out, "total": len(out)})
}

func (h *Handler) apiGetApiKey(w http.ResponseWriter, r *http.Request, id string) {
	if h.keys != nil {
		rec, err := h.keys.Get(id)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
			return
		}
		json.NewEncoder(w).Encode(toApiKeyViewRecord(rec, h.keys.HasPortalToken(id)))
		return
	}
	entry := config.GetApiKeyEntry(id)
	if entry == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}
	json.NewEncoder(w).Encode(toApiKeyView(*entry))
}

type apiKeyCreateRequest struct {
	Name            string  `json:"name,omitempty"`
	Key             string  `json:"key,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
	TokenLimit      int64   `json:"tokenLimit,omitempty"`
	CreditLimit     float64 `json:"creditLimit,omitempty"`
	RequestLimit    int64   `json:"requestLimit,omitempty"`
	ExpiresAt       *int64  `json:"expiresAt,omitempty"`
	ResetPolicy     string  `json:"resetPolicy,omitempty"`
	EnforcementMode string  `json:"enforcementMode,omitempty"`
}

func (h *Handler) apiCreateApiKey(w http.ResponseWriter, r *http.Request) {
	var req apiKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	if h.keys != nil {
		in := apikey.CreateInput{
			Name: req.Name, Key: req.Key, Enabled: enabled,
			TokenLimit: req.TokenLimit, CreditLimit: req.CreditLimit, RequestLimit: req.RequestLimit,
			ResetPolicy: req.ResetPolicy, EnforcementMode: req.EnforcementMode,
		}
		if req.ExpiresAt != nil && *req.ExpiresAt > 0 {
			t := time.Unix(*req.ExpiresAt, 0).UTC()
			in.ExpiresAt = &t
		}
		rec, secret, err := h.keys.Create(in)
		if err != nil {
			status := http.StatusBadRequest
			if err == apikey.ErrDuplicate {
				status = http.StatusBadRequest
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"id":      rec.Key.ID,
			"key":     secret,
			"apiKey":  toApiKeyViewRecord(rec, false),
		})
		return
	}

	keyValue := req.Key
	if keyValue == "" {
		keyValue = config.GenerateApiKeyValue()
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Name: req.Name, Key: keyValue, Enabled: enabled,
		TokenLimit: req.TokenLimit, CreditLimit: req.CreditLimit,
	})
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      entry.ID,
		"key":     entry.Key,
		"apiKey":  toApiKeyView(entry),
	})
}

type apiKeyUpdateRequest struct {
	Name            *string  `json:"name,omitempty"`
	Key             *string  `json:"key,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
	TokenLimit      *int64   `json:"tokenLimit,omitempty"`
	CreditLimit     *float64 `json:"creditLimit,omitempty"`
	RequestLimit    *int64   `json:"requestLimit,omitempty"`
	ExpiresAt       *int64   `json:"expiresAt,omitempty"`
	ResetPolicy     *string  `json:"resetPolicy,omitempty"`
	EnforcementMode *string  `json:"enforcementMode,omitempty"`
}

func (h *Handler) apiUpdateApiKey(w http.ResponseWriter, r *http.Request, id string) {
	var req apiKeyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if h.keys != nil {
		in := apikey.UpdateInput{
			Name: req.Name, Enabled: req.Enabled,
			TokenLimit: req.TokenLimit, CreditLimit: req.CreditLimit, RequestLimit: req.RequestLimit,
			ResetPolicy: req.ResetPolicy, EnforcementMode: req.EnforcementMode,
		}
		if req.ExpiresAt != nil {
			if *req.ExpiresAt <= 0 {
				in.ClearExpires = true
			} else {
				t := time.Unix(*req.ExpiresAt, 0).UTC()
				in.ExpiresAt = &t
			}
		}
		rec, err := h.keys.Update(id, in)
		if err != nil {
			if err == apikey.ErrNotFound {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"apiKey":  toApiKeyViewRecord(rec, h.keys.HasPortalToken(id)),
		})
		return
	}

	existing := config.GetApiKeyEntry(id)
	if existing == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}
	patch := *existing
	if req.Name != nil {
		patch.Name = *req.Name
	}
	if req.Key != nil {
		patch.Key = *req.Key
	}
	if req.Enabled != nil {
		patch.Enabled = *req.Enabled
	}
	if req.TokenLimit != nil {
		patch.TokenLimit = *req.TokenLimit
	}
	if req.CreditLimit != nil {
		patch.CreditLimit = *req.CreditLimit
	}
	if err := config.UpdateApiKey(id, patch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	updated := config.GetApiKeyEntry(id)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "apiKey": toApiKeyView(*updated)})
}

func (h *Handler) apiDeleteApiKey(w http.ResponseWriter, r *http.Request, id string) {
	if h.keys != nil {
		if err := h.keys.Delete(id); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	if err := config.DeleteApiKey(id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiResetApiKeyUsage(w http.ResponseWriter, r *http.Request, id string) {
	if h.keys != nil {
		rec, err := h.keys.ResetUsage(id)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "apiKey": toApiKeyViewRecord(rec, h.keys.HasPortalToken(id))})
		return
	}
	if err := config.ResetApiKeyUsage(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	updated := config.GetApiKeyEntry(id)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "apiKey": toApiKeyView(*updated)})
}

func (h *Handler) apiRotateApiKey(w http.ResponseWriter, r *http.Request, id string) {
	if h.keys == nil {
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"error": "key store unavailable"})
		return
	}
	rec, secret, err := h.keys.Rotate(id)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "id": rec.Key.ID, "key": secret,
		"apiKey": toApiKeyViewRecord(rec, h.keys.HasPortalToken(id)),
	})
}

func (h *Handler) apiCreatePortalToken(w http.ResponseWriter, r *http.Request, id string) {
	if h.keys == nil {
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"error": "key store unavailable"})
		return
	}
	token, info, err := h.keys.CreatePortalToken(id, 0)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	scheme := "http"
	if config.IsTLSEnabled() || r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	url := scheme + "://" + host + "/usage/p/" + token
	resp := map[string]interface{}{"success": true, "token": token, "url": url, "prefix": info.Prefix}
	if info.ExpiresAt != nil {
		resp["expiresAt"] = info.ExpiresAt.Unix()
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) apiDeletePortalToken(w http.ResponseWriter, r *http.Request, id string) {
	if h.keys == nil {
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"error": "key store unavailable"})
		return
	}
	if err := h.keys.RevokePortalToken(id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
