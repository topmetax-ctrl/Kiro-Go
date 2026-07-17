package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
)

// memoryConfigView is the admin payload for the memory sidecar settings. The
// APIKey is masked so the admin panel can display it without exposing the secret.
type memoryConfigView struct {
	Enabled         bool   `json:"enabled"`
	Provider        string `json:"provider"`
	BaseURL         string `json:"baseURL"`
	APIKeyMasked    string `json:"apiKeyMasked"`
	WriteMode       string `json:"writeMode"`
	RetrievalLimit  int    `json:"retrievalLimit"`
	Inject          bool   `json:"inject"`
	MaxInjectTokens int    `json:"maxInjectTokens"`
	RedactSecrets   bool   `json:"redactSecrets"`
	StoreSourceCode bool   `json:"storeSourceCode"`
	FailOpen        bool   `json:"failOpen"`
	SearchMs        int    `json:"searchMs"`
	WriteMs         int    `json:"writeMs"`
}

func toMemoryConfigView(m config.MemoryConfig) memoryConfigView {
	redact := m.Redaction.RedactSecrets == nil || *m.Redaction.RedactSecrets
	failOpen := m.FailOpen == nil || *m.FailOpen
	inject := m.Inject != nil && *m.Inject
	return memoryConfigView{
		Enabled:         m.Enabled,
		Provider:        m.Provider,
		BaseURL:         m.BaseURL,
		APIKeyMasked:    config.MaskApiKey(m.APIKey),
		WriteMode:       m.WriteMode,
		RetrievalLimit:  m.RetrievalLimit,
		Inject:          inject,
		MaxInjectTokens: m.MaxInjectTokens,
		RedactSecrets:   redact,
		StoreSourceCode: m.Redaction.StoreSourceCode,
		FailOpen:        failOpen,
		SearchMs:        m.Timeouts.SearchMs,
		WriteMs:         m.Timeouts.WriteMs,
	}
}

// apiGetMemoryConfig returns the memory sidecar settings with the API key masked.
func (h *Handler) apiGetMemoryConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(toMemoryConfigView(config.GetMemoryConfig()))
}

// apiUpdateMemoryConfig applies a partial patch to the memory settings. Only
// fields present in the request body are changed; the rest keep their stored
// values. An incoming API key that still looks masked (contains "****") is
// treated as "unchanged" and the stored secret is preserved — mirroring the
// upstreams update path. After saving, the live provider is rebuilt so the
// change takes effect at runtime without a restart.
func (h *Handler) apiUpdateMemoryConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled         *bool   `json:"enabled,omitempty"`
		Provider        *string `json:"provider,omitempty"`
		BaseURL         *string `json:"baseURL,omitempty"`
		APIKey          *string `json:"apiKey,omitempty"`
		WriteMode       *string `json:"writeMode,omitempty"`
		RetrievalLimit  *int    `json:"retrievalLimit,omitempty"`
		Inject          *bool   `json:"inject,omitempty"`
		MaxInjectTokens *int    `json:"maxInjectTokens,omitempty"`
		RedactSecrets   *bool   `json:"redactSecrets,omitempty"`
		StoreSourceCode *bool   `json:"storeSourceCode,omitempty"`
		FailOpen        *bool   `json:"failOpen,omitempty"`
		SearchMs        *int    `json:"searchMs,omitempty"`
		WriteMs         *int    `json:"writeMs,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Start from the raw stored config (not the defaults-resolved copy) so we do
	// not persist resolved defaults as if the operator set them (default-drift).
	cur := config.GetMemoryConfigRaw()

	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if req.Provider != nil {
		cur.Provider = strings.TrimSpace(*req.Provider)
	}
	if req.BaseURL != nil {
		cur.BaseURL = strings.TrimSpace(*req.BaseURL)
	}
	// Preserve the stored key when the client sends back a masked/empty value.
	if req.APIKey != nil {
		v := strings.TrimSpace(*req.APIKey)
		if v != "" && !strings.Contains(v, "****") {
			cur.APIKey = v
		}
	}
	if req.WriteMode != nil {
		mode := strings.TrimSpace(*req.WriteMode)
		switch mode {
		case config.MemoryWriteModeExplicit, config.MemoryWriteModeCurated, config.MemoryWriteModeAutomatic, "":
			cur.WriteMode = mode
		default:
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid writeMode: " + mode})
			return
		}
	}
	if req.RetrievalLimit != nil {
		cur.RetrievalLimit = *req.RetrievalLimit
	}
	if req.Inject != nil {
		cur.Inject = req.Inject
	}
	if req.MaxInjectTokens != nil {
		cur.MaxInjectTokens = *req.MaxInjectTokens
	}
	if req.RedactSecrets != nil {
		cur.Redaction.RedactSecrets = req.RedactSecrets
	}
	if req.StoreSourceCode != nil {
		cur.Redaction.StoreSourceCode = *req.StoreSourceCode
	}
	if req.FailOpen != nil {
		cur.FailOpen = req.FailOpen
	}
	if req.SearchMs != nil {
		cur.Timeouts.SearchMs = *req.SearchMs
	}
	if req.WriteMs != nil {
		cur.Timeouts.WriteMs = *req.WriteMs
	}

	if err := config.UpdateMemoryConfig(cur); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.rebuildMemoryProvider()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// memoryScopeFromRequest resolves the memory scope for a store/retrieve call.
// The admin API is single-operator (password-authed), so there is no per-caller
// principal in context. The operator names the scope to act on via the
// "principal" query/body field; empty defaults to the shared anonymous scope,
// matching the responses store's ownerless scope.
func memoryScopeFromRequest(explicit string) MemoryScope {
	p := strings.TrimSpace(explicit)
	if p == "" {
		p = anonymousOwner
	}
	return MemoryScope{Principal: p}
}

// apiMemorySearch handles GET /admin/api/memory/search?q=...&principal=...&limit=...
// It retrieves memories for the named scope. This does NOT touch the LLM request
// path — it is a standalone store/retrieve API.
func (h *Handler) apiMemorySearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing query parameter 'q'"})
		return
	}
	scope := memoryScopeFromRequest(r.URL.Query().Get("principal"))
	limit := 0
	if l := strings.TrimSpace(r.URL.Query().Get("limit")); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	mems, err := h.getMemory().Search(r.Context(), SearchQuery{Scope: scope, Query: q, Limit: limit})
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if mems == nil {
		mems = []Memory{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"memories": mems})
}

// apiMemoryAdd handles POST /admin/api/memory
// Body: {"principal":"...", "messages":[{"role":"user","content":"..."}]}
// Redaction is enforced inside the provider and cannot be bypassed here.
func (h *Handler) apiMemoryAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Principal string          `json:"principal,omitempty"`
		Messages  []MemoryMessage `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.Messages) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "messages must not be empty"})
		return
	}
	scope := memoryScopeFromRequest(req.Principal)
	if err := h.getMemory().Add(r.Context(), AddMemoryInput{Scope: scope, Messages: req.Messages}); err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiMemoryDelete handles DELETE /admin/api/memory?principal=...
// It removes all memories for the named scope. Delete is not fail-open: a
// failure surfaces so the operator is not misled into thinking data was purged.
func (h *Handler) apiMemoryDelete(w http.ResponseWriter, r *http.Request) {
	scope := memoryScopeFromRequest(r.URL.Query().Get("principal"))
	if err := h.getMemory().Delete(r.Context(), scope); err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
