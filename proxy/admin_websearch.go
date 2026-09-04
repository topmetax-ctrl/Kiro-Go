package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"strings"
)

// webSearchConfigView is the admin payload for the web search settings. The
// TavilyApiKey is masked so the admin panel can display it without exposing
// the secret.
type webSearchConfigView struct {
	Enabled           bool   `json:"enabled"`
	SearxngEnabled    bool   `json:"searxngEnabled"`
	SearxngBaseURL    string `json:"searxngBaseUrl"`
	TavilyEnabled     bool   `json:"tavilyEnabled"`
	TavilyApiKeyMasked string `json:"tavilyApiKeyMasked"`
	AllowPaidUsage    bool   `json:"allowPaidUsage"`
	AppendSources     bool   `json:"appendSources"`
	EmitNativeBlocks  bool   `json:"emitNativeToolBlocks"`
	MCPFallback       bool   `json:"mcpFallback"`
	MaxRounds         int    `json:"maxRounds"`
	MaxSearches       int    `json:"maxSearchesPerRequest"`
	MaxResults        int    `json:"maxResultsPerSearch"`
}

// toWebSearchConfigView builds the view from the RAW stored config, falling
// back to resolved defaults only for pointer fields that are nil (never saved
// by the operator). This avoids "ghost resolution" where the card shows
// a toggle as ON even after the operator explicitly turned it OFF — because
// the raw false is overwritten by the resolver's true-when-nil default in
// the GET path.
func toWebSearchConfigView(ws config.WebSearchConfig) webSearchConfigView {
	// SearXNG Enabled pointer: nil → default (true); otherwise use stored value.
	sxEnabled := true
	if ws.SearXNG.Enabled != nil {
		sxEnabled = *ws.SearXNG.Enabled
	}
	// AppendSources / EmitNativeToolBlocks: nil → default true; stored false → false.
	appendSrc := config.WebSearchAppendSources()
	emitBlocks := config.WebSearchEmitNativeToolBlocks()
	// MCPFallback: nil → default false; stored true → true.
	mcpFallback := config.WebSearchMCPFallback()

	return webSearchConfigView{
		Enabled:            ws.Enabled,
		SearxngEnabled:     sxEnabled,
		SearxngBaseURL:     ws.SearXNG.BaseURL,
		TavilyEnabled:      ws.Tavily.Enabled,
		TavilyApiKeyMasked: config.MaskApiKey(ws.Tavily.APIKey),
		AllowPaidUsage:     ws.Routing.AllowPaidUsage,
		AppendSources:      appendSrc,
		EmitNativeBlocks:   emitBlocks,
		MCPFallback:        mcpFallback,
		MaxRounds:          ws.Limits.MaxRounds,
		MaxSearches:        ws.Limits.MaxSearchesPerRequest,
		MaxResults:         ws.Limits.MaxResultsPerSearch,
	}
}

// apiGetWebSearchConfig returns the web search settings with the Tavily API key
// masked. The view is built from the raw config so operator changes are
// reflected accurately without ghost-default resolution.
func (h *Handler) apiGetWebSearchConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(toWebSearchConfigView(config.GetWebSearchConfigRaw()))
}

// apiUpdateWebSearchConfig applies a partial patch to the web search settings.
// Only fields present in the request body are changed; the rest keep their
// stored values. An incoming Tavily API key that still looks masked (contains
// "****") is treated as "unchanged" and the stored secret is preserved. After
// saving the config takes effect on the next request (no restart needed).
func (h *Handler) apiUpdateWebSearchConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled          *bool   `json:"enabled,omitempty"`
		SearxngEnabled   *bool   `json:"searxngEnabled,omitempty"`
		SearxngBaseURL   *string `json:"searxngBaseUrl,omitempty"`
		TavilyEnabled    *bool   `json:"tavilyEnabled,omitempty"`
		TavilyApiKey     *string `json:"tavilyApiKey,omitempty"`
		AllowPaidUsage   *bool   `json:"allowPaidUsage,omitempty"`
		AppendSources    *bool   `json:"appendSources,omitempty"`
		EmitNativeBlocks *bool   `json:"emitNativeToolBlocks,omitempty"`
		MCPFallback      *bool   `json:"mcpFallback,omitempty"`
		MaxRounds        *int    `json:"maxRounds,omitempty"`
		MaxSearches      *int    `json:"maxSearchesPerRequest,omitempty"`
		MaxResults       *int    `json:"maxResultsPerSearch,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Start from the raw stored config (not the defaults-resolved copy) so we do
	// not persist resolved defaults as if the operator set them (default-drift).
	cur := config.GetWebSearchConfigRaw()

	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}

	// SearXNG
	if req.SearxngEnabled != nil {
		if cur.SearXNG.Enabled == nil {
			cur.SearXNG.Enabled = new(bool)
		}
		*cur.SearXNG.Enabled = *req.SearxngEnabled
	}
	if req.SearxngBaseURL != nil {
		cur.SearXNG.BaseURL = strings.TrimSpace(*req.SearxngBaseURL)
	}

	// Tavily
	if req.TavilyEnabled != nil {
		cur.Tavily.Enabled = *req.TavilyEnabled
	}
	// Preserve the stored key when the client sends back a masked/empty value.
	if req.TavilyApiKey != nil {
		v := strings.TrimSpace(*req.TavilyApiKey)
		if v != "" && !strings.Contains(v, "****") {
			cur.Tavily.APIKey = v
		}
	}

	// Routing
	if req.AllowPaidUsage != nil {
		cur.Routing.AllowPaidUsage = *req.AllowPaidUsage
	}

	// Boolean features (nil → keep stored; present → update)
	if req.AppendSources != nil {
		cur.AppendSources = req.AppendSources
	}
	if req.EmitNativeBlocks != nil {
		cur.EmitNativeToolBlocks = req.EmitNativeBlocks
	}
	if req.MCPFallback != nil {
		cur.MCPFallback = req.MCPFallback
	}

	// Limits
	if req.MaxRounds != nil && *req.MaxRounds > 0 {
		cur.Limits.MaxRounds = *req.MaxRounds
	}
	if req.MaxSearches != nil && *req.MaxSearches > 0 {
		cur.Limits.MaxSearchesPerRequest = *req.MaxSearches
	}
	if req.MaxResults != nil && *req.MaxResults > 0 {
		cur.Limits.MaxResultsPerSearch = *req.MaxResults
	}

	if err := config.UpdateWebSearchConfig(cur); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
