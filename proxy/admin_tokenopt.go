package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/tokenopt"
)

// Admin API for Token Optimization.
//
//	GET  /admin/api/token-optimization[?probe=1]  saved settings + what is actually true now
//	POST /admin/api/token-optimization            partial update
//	POST /admin/api/token-optimization/test       verify an engine (optionally with unsaved values)
//
// The GET deliberately returns two things: the SAVED settings, and the RUNTIME
// state. Saving "Headroom, on" does not make Headroom reachable; the UI must
// never show one as if it were the other.

type tokenOptHeadroomView struct {
	BaseURL      string `json:"baseUrl"`
	APIKeySet    bool   `json:"apiKeySet"`
	APIKeyMasked string `json:"apiKeyMasked"`
	TimeoutMs    int    `json:"timeoutMs"`
	Mode         string `json:"mode"`
	MinBytes     int    `json:"minBytes"`
}

type tokenOptSettingsView struct {
	Mode             string               `json:"mode"`
	InputCompression string               `json:"inputCompression"`
	Headroom         tokenOptHeadroomView `json:"headroom"`
	RTK              struct {
		ProxyURL string `json:"proxyUrl"`
	} `json:"rtk"`
	Caveman  string `json:"caveman"`
	Ponytail string `json:"ponytail"`
}

// tokenOptLimits lets the UI validate and describe fields without hard-coding
// numbers that live in config.
type tokenOptLimits struct {
	MinTimeoutMs          int    `json:"minTimeoutMs"`
	MaxTimeoutMs          int    `json:"maxTimeoutMs"`
	DefaultTimeoutMs      int    `json:"defaultTimeoutMs"`
	MinBytes              int    `json:"minBytes"`
	MaxBytes              int    `json:"maxBytes"`
	DefaultMinBytes       int    `json:"defaultMinBytes"`
	TestedHeadroomVersion string `json:"testedHeadroomVersion"`
}

type tokenOptAdminResponse struct {
	Settings tokenOptSettingsView `json:"settings"`
	Runtime  tokenopt.Status      `json:"runtime"`
	Limits   tokenOptLimits       `json:"limits"`
}

func toTokenOptSettingsView(c config.TokenOptimizationConfig) tokenOptSettingsView {
	v := tokenOptSettingsView{
		Mode: c.Mode, InputCompression: c.InputCompression, Caveman: c.Caveman, Ponytail: c.Ponytail,
		Headroom: tokenOptHeadroomView{
			BaseURL: c.Headroom.BaseURL, APIKeySet: c.Headroom.APIKey != "",
			APIKeyMasked: config.MaskSecret(c.Headroom.APIKey),
			TimeoutMs:    c.Headroom.TimeoutMs, Mode: c.Headroom.Mode, MinBytes: c.Headroom.MinBytes,
		},
	}
	v.RTK.ProxyURL = c.RTK.ProxyURL
	return v
}

func tokenOptProviders() []tokenopt.ProviderRef {
	providers, _ := config.GetUpstreamConfig()
	refs := make([]tokenopt.ProviderRef, 0, len(providers))
	for _, p := range providers {
		refs = append(refs, tokenopt.ProviderRef{Name: p.Name, BaseURL: p.BaseURL, Enabled: p.Enabled})
	}
	return refs
}

// tokenOptSnapshot builds the full admin payload from the current settings.
func (h *Handler) tokenOptSnapshot() tokenOptAdminResponse {
	cfg := config.GetTokenOptimization()
	return tokenOptAdminResponse{
		Settings: toTokenOptSettingsView(cfg),
		Runtime:  h.tokenOptManager().Status(cfg, tokenOptProviders()),
		Limits: tokenOptLimits{
			MinTimeoutMs: config.MinHeadroomTimeout, MaxTimeoutMs: config.MaxHeadroomTimeout, DefaultTimeoutMs: config.DefaultHeadroomTimeout,
			MinBytes: config.MinHeadroomMinBytes, MaxBytes: config.MaxHeadroomMinBytes, DefaultMinBytes: config.DefaultHeadroomMin,
			TestedHeadroomVersion: tokenopt.TestedHeadroomVersion,
		},
	}
}

// staleAfter is how old a stored check may be before a ?probe=1 refreshes it, so
// several open admin tabs cannot turn into a stream of probes against the engine.
const tokenOptProbeStaleAfter = 10 * time.Second

func (h *Handler) apiGetTokenOptimization(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("probe") == "1" {
		h.refreshTokenOptHealth(r.Context())
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(h.tokenOptSnapshot())
}

// refreshTokenOptHealth verifies whichever engines are configured, when their
// last check is stale. It never runs when the feature is off or nothing is
// configured.
func (h *Handler) refreshTokenOptHealth(ctx context.Context) {
	cfg := config.GetTokenOptimization()
	mgr := h.tokenOptManager()
	hh, rr := mgr.LastHealth()
	fresh := func(rep *tokenopt.HealthReport) bool {
		return rep != nil && time.Since(rep.CheckedAt) < tokenOptProbeStaleAfter
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if cfg.Mode != config.TokenOptModeOff && cfg.InputCompression == config.TokenOptInputHeadroom && cfg.Headroom.BaseURL != "" && !fresh(hh) {
		mgr.VerifyHeadroom(ctx, cfg.Headroom, true)
	}
	if cfg.RTK.ProxyURL != "" && !fresh(rr) {
		mgr.VerifyRTK(ctx, cfg.RTK.ProxyURL, true)
	}
}

// tokenOptPatch is a partial update: a field that is absent keeps its stored
// value. The Headroom token is never read back; to keep it, omit it (or send the
// masked value); to remove it, send clearApiKey.
type tokenOptPatch struct {
	Mode             *string `json:"mode"`
	InputCompression *string `json:"inputCompression"`
	Headroom         *struct {
		BaseURL     *string `json:"baseUrl"`
		APIKey      *string `json:"apiKey"`
		ClearAPIKey *bool   `json:"clearApiKey"`
		TimeoutMs   *int    `json:"timeoutMs"`
		Mode        *string `json:"mode"`
		MinBytes    *int    `json:"minBytes"`
	} `json:"headroom"`
	RTK *struct {
		ProxyURL *string `json:"proxyUrl"`
	} `json:"rtk"`
	Caveman  *string `json:"caveman"`
	Ponytail *string `json:"ponytail"`
}

func (p tokenOptPatch) apply(c *config.TokenOptimizationConfig) {
	if p.Mode != nil {
		c.Mode = *p.Mode
	}
	if p.InputCompression != nil {
		c.InputCompression = *p.InputCompression
	}
	if p.Caveman != nil {
		c.Caveman = *p.Caveman
	}
	if p.Ponytail != nil {
		c.Ponytail = *p.Ponytail
	}
	if p.RTK != nil && p.RTK.ProxyURL != nil {
		c.RTK.ProxyURL = strings.TrimSpace(*p.RTK.ProxyURL)
	}
	if h := p.Headroom; h != nil {
		if h.BaseURL != nil {
			c.Headroom.BaseURL = strings.TrimSpace(*h.BaseURL)
		}
		if h.TimeoutMs != nil {
			c.Headroom.TimeoutMs = *h.TimeoutMs
		}
		if h.Mode != nil {
			c.Headroom.Mode = *h.Mode
		}
		if h.MinBytes != nil {
			c.Headroom.MinBytes = *h.MinBytes
		}
		if h.ClearAPIKey != nil && *h.ClearAPIKey {
			c.Headroom.APIKey = ""
		} else if h.APIKey != nil {
			// An empty or masked value means "unchanged", as in the other settings cards.
			if v := strings.TrimSpace(*h.APIKey); v != "" && !strings.Contains(v, "****") {
				c.Headroom.APIKey = v
			}
		}
	}
}

func writeTokenOptError(w http.ResponseWriter, status int, err error) {
	body := map[string]string{"error": err.Error()}
	var te *config.TokenOptimizationError
	if errors.As(err, &te) {
		body["field"] = te.Field
		body["error"] = te.Msg
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (h *Handler) apiUpdateTokenOptimization(w http.ResponseWriter, r *http.Request) {
	var patch tokenOptPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if err := config.PatchTokenOptimization(patch.apply); err != nil {
		var te *config.TokenOptimizationError
		if errors.As(err, &te) {
			writeTokenOptError(w, http.StatusBadRequest, err)
		} else {
			writeTokenOptError(w, http.StatusInternalServerError, err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// tokenOptTestRequest verifies an engine. Values left out fall back to the saved
// settings, so "Test" can run before "Save".
type tokenOptTestRequest struct {
	Target   string `json:"target"` // "headroom" or "rtk"
	Headroom *struct {
		BaseURL   *string `json:"baseUrl"`
		APIKey    *string `json:"apiKey"`
		TimeoutMs *int    `json:"timeoutMs"`
	} `json:"headroom"`
	RTK *struct {
		ProxyURL *string `json:"proxyUrl"`
	} `json:"rtk"`
}

func (h *Handler) apiTestTokenOptimization(w http.ResponseWriter, r *http.Request) {
	var req tokenOptTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	stored := config.GetTokenOptimization()
	mgr := h.tokenOptManager()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var report tokenopt.HealthReport
	switch req.Target {
	case "headroom":
		hc := stored.Headroom
		if p := req.Headroom; p != nil {
			if p.BaseURL != nil {
				hc.BaseURL = strings.TrimRight(strings.TrimSpace(*p.BaseURL), "/")
			}
			if p.TimeoutMs != nil && *p.TimeoutMs > 0 {
				hc.TimeoutMs = *p.TimeoutMs
			}
			if p.APIKey != nil {
				if v := strings.TrimSpace(*p.APIKey); v != "" && !strings.Contains(v, "****") {
					hc.APIKey = v
				}
			}
		}
		// The stored token is only ever sent to the stored URL: a typed-in address
		// must not receive the saved secret.
		if hc.BaseURL != stored.Headroom.BaseURL && (req.Headroom == nil || req.Headroom.APIKey == nil || strings.Contains(*req.Headroom.APIKey, "****") || strings.TrimSpace(*req.Headroom.APIKey) == "") {
			hc.APIKey = ""
		}
		if _, err := config.NormalizeServiceURL(hc.BaseURL); err != nil {
			writeTokenOptError(w, http.StatusBadRequest, &config.TokenOptimizationError{Field: "headroom.baseUrl", Msg: err.Error()})
			return
		}
		persist := hc.BaseURL == stored.Headroom.BaseURL && hc.APIKey == stored.Headroom.APIKey && hc.TimeoutMs == stored.Headroom.TimeoutMs
		report = mgr.VerifyHeadroom(ctx, hc, persist)
	case "rtk":
		url := stored.RTK.ProxyURL
		if req.RTK != nil && req.RTK.ProxyURL != nil {
			url = strings.TrimRight(strings.TrimSpace(*req.RTK.ProxyURL), "/")
		}
		if _, err := config.NormalizeServiceURL(url); err != nil {
			writeTokenOptError(w, http.StatusBadRequest, &config.TokenOptimizationError{Field: "rtk.proxyUrl", Msg: err.Error()})
			return
		}
		report = mgr.VerifyRTK(ctx, url, url == stored.RTK.ProxyURL)
	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": `target must be "headroom" or "rtk"`})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"report": report, "state": h.tokenOptSnapshot()})
}
