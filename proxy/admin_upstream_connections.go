package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
)

const upstreamTestTimeout = 15 * time.Second

func parseUpstreamConnectionPath(path string) (providerID, connectionID, action string, ok bool) {
	rest := strings.TrimPrefix(path, "/upstreams/")
	if rest == path {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "connections" || parts[0] == "" || parts[0] == "export" || parts[0] == "import" {
		return "", "", "", false
	}
	providerID = parts[0]
	switch len(parts) {
	case 2:
		return providerID, "", "", true
	case 3:
		if parts[2] == "preview" || parts[2] == "import" {
			return providerID, "", parts[2], true
		}
		return providerID, parts[2], "", true
	case 4:
		if parts[3] == "test" {
			return providerID, parts[2], "test", true
		}
	}
	return "", "", "", false
}

func publicConnection(p config.UpstreamProvider, c config.UpstreamConnection) map[string]interface{} {
	health, until := connectionHealthPublic(p.ID, c.ID, time.Now())
	out := map[string]interface{}{
		"id":           c.ID,
		"name":         c.Name,
		"apiKeyMasked": config.MaskConnectionSecret(c.ApiKey),
		"hasApiKey":    strings.TrimSpace(c.ApiKey) != "",
		"enabled":      c.Enabled,
		"health":       health,
	}
	if until > 0 {
		out["cooldownUntil"] = until
	}
	if c.Weight != 0 {
		out["weight"] = c.Weight
	}
	return out
}

func publicProvider(p config.UpstreamProvider) map[string]interface{} {
	conns := config.ResolvedConnections(p)
	pub := make([]map[string]interface{}, 0, len(conns))
	for _, c := range conns {
		pub = append(pub, publicConnection(p, c))
	}
	legacy := p.ApiKey
	if legacy == "" && len(conns) > 0 {
		legacy = conns[0].ApiKey
	}
	out := map[string]interface{}{
		"id":                 p.ID,
		"name":               p.Name,
		"baseUrl":            p.BaseURL,
		"apiKey":             config.MaskConnectionSecret(legacy),
		"proxyURL":           p.ProxyURL,
		"enabled":            p.Enabled,
		"hidden":             p.Hidden,
		"priceInPerM":        p.PriceInPerM,
		"priceOutPerM":       p.PriceOutPerM,
		"connections":        pub,
		"connectionStrategy": p.ConnectionStrategy,
	}
	return out
}

func findStoredProvider(id string) (config.UpstreamProvider, bool) {
	providers, _ := config.GetUpstreamConfig()
	for _, p := range providers {
		if p.ID == id {
			return p, true
		}
	}
	return config.UpstreamProvider{}, false
}

func writeConnError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	msg := "request failed"
	if err != nil {
		msg = err.Error()
	}
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *Handler) handleUpstreamConnectionAPI(w http.ResponseWriter, r *http.Request, path string) bool {
	providerID, connectionID, action, ok := parseUpstreamConnectionPath(path)
	if !ok {
		return false
	}
	switch {
	case action == "preview" && r.Method == http.MethodPost:
		h.apiPreviewUpstreamConnections(w, r, providerID)
	case action == "import" && r.Method == http.MethodPost:
		h.apiImportUpstreamConnections(w, r, providerID)
	case action == "test" && r.Method == http.MethodPost:
		h.apiTestUpstreamConnection(w, r, providerID, connectionID)
	case connectionID == "" && r.Method == http.MethodPost:
		h.apiAddUpstreamConnection(w, r, providerID)
	case connectionID != "" && r.Method == http.MethodPatch:
		h.apiPatchUpstreamConnection(w, r, providerID, connectionID)
	case connectionID != "" && r.Method == http.MethodDelete:
		h.apiDeleteUpstreamConnection(w, r, providerID, connectionID)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
	}
	return true
}

func (h *Handler) apiAddUpstreamConnection(w http.ResponseWriter, r *http.Request, providerID string) {
	var req struct {
		Name    string `json:"name"`
		ApiKey  string `json:"apiKey"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConnError(w, 400, errors.New("invalid JSON"))
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	conn, err := config.AddUpstreamConnection(providerID, config.UpstreamConnection{
		Name:    req.Name,
		ApiKey:  req.ApiKey,
		Enabled: enabled,
	})
	if err != nil {
		status := 400
		if errors.Is(err, config.ErrUpstreamProviderNotFound) {
			status = 404
		}
		writeConnError(w, status, err)
		return
	}
	p, _ := findStoredProvider(providerID)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"connection": publicConnection(p, conn),
	})
}

func (h *Handler) apiPatchUpstreamConnection(w http.ResponseWriter, r *http.Request, providerID, connectionID string) {
	var req struct {
		Name    *string `json:"name"`
		ApiKey  *string `json:"apiKey"`
		Enabled *bool   `json:"enabled"`
		Weight  *int    `json:"weight"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConnError(w, 400, errors.New("invalid JSON"))
		return
	}
	conn, err := config.UpdateUpstreamConnection(providerID, connectionID, config.UpstreamConnectionPatch{
		Name:    req.Name,
		ApiKey:  req.ApiKey,
		Enabled: req.Enabled,
		Weight:  req.Weight,
	})
	if err != nil {
		status := 400
		if errors.Is(err, config.ErrUpstreamProviderNotFound) || errors.Is(err, config.ErrUpstreamConnectionNotFound) {
			status = 404
		}
		writeConnError(w, status, err)
		return
	}
	p, _ := findStoredProvider(providerID)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"connection": publicConnection(p, conn),
	})
}

func (h *Handler) apiDeleteUpstreamConnection(w http.ResponseWriter, r *http.Request, providerID, connectionID string) {
	if err := config.DeleteUpstreamConnection(providerID, connectionID); err != nil {
		status := 400
		if errors.Is(err, config.ErrUpstreamProviderNotFound) || errors.Is(err, config.ErrUpstreamConnectionNotFound) {
			status = 404
		}
		writeConnError(w, status, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiPreviewUpstreamConnections(w http.ResponseWriter, r *http.Request, providerID string) {
	p, ok := findStoredProvider(providerID)
	if !ok {
		writeConnError(w, 404, config.ErrUpstreamProviderNotFound)
		return
	}
	var req struct {
		Text        string                              `json:"text"`
		Delimiter   string                              `json:"delimiter"`
		Naming      string                              `json:"naming"`
		Resolutions []config.ConnectionImportResolution `json:"resolutions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConnError(w, 400, errors.New("invalid JSON"))
		return
	}
	prev := config.ParseConnectionImport(req.Text, req.Delimiter, req.Naming, config.ExistingConnectionKeys(p), config.ConnectionNames(p), req.Resolutions)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"preview": prev,
	})
}

func (h *Handler) apiImportUpstreamConnections(w http.ResponseWriter, r *http.Request, providerID string) {
	p, ok := findStoredProvider(providerID)
	if !ok {
		writeConnError(w, 404, config.ErrUpstreamProviderNotFound)
		return
	}
	var req struct {
		Text        string                              `json:"text"`
		Delimiter   string                              `json:"delimiter"`
		Naming      string                              `json:"naming"`
		Resolutions []config.ConnectionImportResolution `json:"resolutions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConnError(w, 400, errors.New("invalid JSON"))
		return
	}
	// Re-parse on the server. The browser preview is never trusted as truth.
	prev := config.ParseConnectionImport(req.Text, req.Delimiter, req.Naming, config.ExistingConnectionKeys(p), config.ConnectionNames(p), req.Resolutions)
	toAdd := make([]config.UpstreamConnection, 0, prev.Ready)
	for _, row := range prev.Lines {
		if row.Status != config.ImportStatusReady || row.Key == "" {
			continue
		}
		toAdd = append(toAdd, config.UpstreamConnection{
			Name:    row.Name,
			ApiKey:  row.Key,
			Enabled: true,
		})
	}
	added, err := config.AddUpstreamConnections(providerID, toAdd)
	if err != nil {
		writeConnError(w, 400, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"added":     len(added),
		"duplicate": prev.Duplicate,
		"ambiguous": prev.Ambiguous,
		"invalid":   prev.Invalid,
		"preview":   prev,
	})
}

func (h *Handler) apiTestUpstreamConnection(w http.ResponseWriter, r *http.Request, providerID, connectionID string) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeConnError(w, 400, errors.New("invalid JSON"))
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeConnError(w, 400, errors.New("model is required"))
		return
	}
	h.runUpstreamTest(w, r, providerID, connectionID, "", "", "", req.Model)
}
