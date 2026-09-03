package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// The Web Search settings card posts a partial patch to /admin/api/websearch.
// Pin the route and its contract: GET returns the current state with the Tavily
// key masked, and POST applies only the fields present, preserving the stored
// key when the client echoes a masked value back.

func setupWebSearchConfig(t *testing.T) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
}

func postWebSearch(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/websearch", strings.NewReader(body))
	req.Header.Set("X-Admin-Password", config.GetPassword())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	return rec
}

func getWebSearch(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/websearch", nil)
	req.Header.Set("X-Admin-Password", config.GetPassword())
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	return rec
}

func TestAdminWebSearchGetReturnsState(t *testing.T) {
	setupWebSearchConfig(t)
	rec := getWebSearch(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var v webSearchConfigView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Enabled {
		t.Fatalf("web search must default to disabled")
	}
	if !v.SearxngEnabled {
		t.Fatalf("searxng must default to enabled (free-first), got false")
	}
	// Raw view: no base URL is configured in a bare config (the resolver's
	// http://searxng:8080 default is not persisted, so the card must not show it
	// as if the operator set it).
	if v.SearxngBaseURL != "" {
		t.Fatalf("bare config must have no stored searxng base URL, got %q", v.SearxngBaseURL)
	}
	if v.AppendSources == false {
		t.Fatalf("appendSources must default to true (resolved)")
	}
}

func TestAdminWebSearchPatchEnabled(t *testing.T) {
	setupWebSearchConfig(t)
	rec := postWebSearch(t, `{"enabled":true,"searxngBaseUrl":"http://sx:8080"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !config.WebSearchToggledOn() {
		t.Fatalf("toggle must be on after patch")
	}
	if !config.SearXNGProviderEnabled() {
		t.Fatalf("searxng must be usable after patch (base URL set)")
	}

	// GET reflects the change back.
	got := getWebSearch(t)
	var v webSearchConfigView
	if err := json.Unmarshal(got.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !v.Enabled || v.SearxngBaseURL != "http://sx:8080" {
		t.Fatalf("GET after patch = %+v", v)
	}
}

func TestAdminWebSearchMaskedKeyPreserved(t *testing.T) {
	setupWebSearchConfig(t)
	// Set a real key first.
	if rec := postWebSearch(t, `{"tavilyEnabled":true,"tavilyApiKey":"tvly-realkey123"}`); rec.Code != http.StatusOK {
		t.Fatalf("initial patch status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := config.TavilyAPIKeyResolved(); got != "tvly-realkey123" {
		t.Fatalf("stored key = %q", got)
	}

	// A patch echoing a masked key must NOT clobber the stored secret.
	if rec := postWebSearch(t, `{"tavilyApiKey":"****masked****"}`); rec.Code != http.StatusOK {
		t.Fatalf("masked patch status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := config.TavilyAPIKeyResolved(); got != "tvly-realkey123" {
		t.Fatalf("masked echo clobbered the stored key: %q", got)
	}

	// GET must mask it.
	got := getWebSearch(t)
	var v webSearchConfigView
	if err := json.Unmarshal(got.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(v.TavilyApiKeyMasked, "****") || strings.Contains(v.TavilyApiKeyMasked, "realkey") {
		t.Fatalf("GET leaked the key: %q", v.TavilyApiKeyMasked)
	}
}

func TestAdminWebSearchPatchDoesNotDriftDefaults(t *testing.T) {
	setupWebSearchConfig(t)
	// A minimal patch (just the toggle) must not persist resolved defaults.
	if rec := postWebSearch(t, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	raw := config.GetWebSearchConfigRaw()
	if raw.Limits.MaxRounds != 0 || raw.Limits.MaxSearchesPerRequest != 0 {
		t.Fatalf("defaults leaked into stored config: %+v", raw.Limits)
	}
}
