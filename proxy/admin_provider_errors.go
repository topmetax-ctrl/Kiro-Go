package proxy

import (
	"encoding/json"
	"errors"
	"kiro-go/apikey"
	"net/http"
	"strings"
)

// apiGetProviderError GET /admin/api/provider-errors?requestId=
// or /admin/api/provider-errors/{requestId}
// Admin-only (handleAdminAPI already authenticated). Portal must never call this.
func (h *Handler) apiGetProviderError(w http.ResponseWriter, r *http.Request) {
	if h.keys == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unavailable"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("requestId"))
	if id == "" {
		path := r.URL.Path
		for _, prefix := range []string{"/admin/api/provider-errors/", "/provider-errors/"} {
			if strings.HasPrefix(path, prefix) {
				id = strings.Trim(strings.TrimPrefix(path, prefix), "/")
				break
			}
		}
		id = strings.TrimSpace(id)
	}
	if id == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "requestId required"})
		return
	}
	items, err := h.keys.GetProviderErrorDetails(id)
	if err != nil {
		if errors.Is(err, apikey.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "lookup failed"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"requestId": id,
		"attempts":  items,
	})
}
