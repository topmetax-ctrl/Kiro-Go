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

// setupUpstreamTransfer initializes a temp config seeded with two providers and
// two routes. The export/import handlers touch neither the pool nor the token
// manager, so a bare Handler is enough.
func setupUpstreamTransfer(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	providers := []config.UpstreamProvider{
		{ID: "p1", Name: "9router", BaseURL: "http://a/v1", ApiKey: "sk-secret-a", Enabled: true},
		{ID: "p2", Name: "xpiki", BaseURL: "http://b/v1", ApiKey: "sk-secret-b", Enabled: true},
	}
	routes := []config.ModelRoute{
		{ID: "r1", Model: "coding", UpstreamID: "p1", Enabled: true},
		{ID: "r2", Model: "writing", UpstreamID: "p2", Enabled: true},
	}
	if err := config.UpdateUpstreamConfig(providers, routes); err != nil {
		t.Fatalf("seed upstreams: %v", err)
	}
	return &Handler{}
}

func postImport(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/import", strings.NewReader(body))
	h.apiImportUpstreams(rec, r)
	return rec
}

func TestApiExportUpstreamsReturnsRealApiKeys(t *testing.T) {
	h := setupUpstreamTransfer(t)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/api/upstreams/export", nil)
	h.apiExportUpstreams(rec, r)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "sk-secret-a") || !strings.Contains(body, "sk-secret-b") {
		t.Errorf("export missing real API keys: %s", body)
	}
	if strings.Contains(body, "****") {
		t.Errorf("export contains masked keys: %s", body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	var b config.UpstreamBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.Kind != config.UpstreamBundleKind {
		t.Errorf("kind = %q, want %q", b.Kind, config.UpstreamBundleKind)
	}
}

func TestApiImportUpstreamsMergeCounts(t *testing.T) {
	h := setupUpstreamTransfer(t)

	// p1 duplicates the seeded 9router; its route "coding" also duplicates.
	// p3 and route "vision" are new.
	rec := postImport(t, h, `{
		"kind": "kiro-go-upstreams", "schema": 1,
		"providers": [
			{"id":"x1","name":"9router","baseUrl":"http://a/v1","apiKey":"sk-other","enabled":true},
			{"id":"x2","name":"vision-gw","baseUrl":"http://c/v1","apiKey":"sk-c","enabled":true}
		],
		"routes": [
			{"id":"y1","model":"coding","upstreamId":"x1","enabled":true},
			{"id":"y2","model":"vision","upstreamId":"x2","enabled":true},
			{"id":"y3","model":"vision-alt","upstreamId":"x1","enabled":true}
		]
	}`)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var d struct {
		Success          bool `json:"success"`
		ProvidersAdded   int  `json:"providersAdded"`
		ProvidersSkipped int  `json:"providersSkipped"`
		RoutesAdded      int  `json:"routesAdded"`
		RoutesSkipped    int  `json:"routesSkipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !d.Success || d.ProvidersAdded != 1 || d.ProvidersSkipped != 1 || d.RoutesAdded != 2 || d.RoutesSkipped != 1 {
		t.Fatalf("counts = +%dp/-%dp +%dr/-%dr, want +1p/-1p +2r/-1r (body=%s)",
			d.ProvidersAdded, d.ProvidersSkipped, d.RoutesAdded, d.RoutesSkipped, rec.Body.String())
	}

	// The route pointed at the skipped duplicate must resolve to the EXISTING provider.
	_, up := config.FindEnabledRoute("vision-alt")
	if up == nil {
		t.Fatal("vision-alt route does not resolve to a provider")
	}
	if up.ID != "p1" || up.ApiKey != "sk-secret-a" {
		t.Errorf("resolved provider = %+v, want existing p1 with its original key", up)
	}
}

func TestApiImportUpstreamsRejectsMalformedJSON(t *testing.T) {
	h := setupUpstreamTransfer(t)
	rec := postImport(t, h, `{not json`)
	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// Both exports are JSON with a "version" field, so pasting the accounts export
// here is the realistic user error.
func TestApiImportUpstreamsRejectsAccountsExport(t *testing.T) {
	h := setupUpstreamTransfer(t)
	rec := postImport(t, h, `{"version":"1.2.5","exportedAt":1,"accounts":[{"id":"a","email":"x@y.z"}],"groups":[],"tags":[]}`)
	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	// The message must name the actual mistake, not just "no providers or routes".
	if !strings.Contains(rec.Body.String(), "accounts export") {
		t.Errorf("error should identify the pasted file: %s", rec.Body.String())
	}
	providers, routes := config.GetUpstreamConfig()
	if len(providers) != 2 || len(routes) != 2 {
		t.Fatalf("config mutated: %d providers, %d routes", len(providers), len(routes))
	}
}

func TestApiImportUpstreamsRejectsEmptyBaseURL(t *testing.T) {
	h := setupUpstreamTransfer(t)
	rec := postImport(t, h, `{"kind":"kiro-go-upstreams","providers":[{"id":"x","name":"n","baseUrl":"","enabled":true}]}`)
	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "baseUrl") {
		t.Errorf("error should name the offending field: %s", rec.Body.String())
	}
	providers, routes := config.GetUpstreamConfig()
	if len(providers) != 2 || len(routes) != 2 {
		t.Fatalf("config mutated: %d providers, %d routes", len(providers), len(routes))
	}
}

func TestApiImportUpstreamsEmptySkipListsMarshalAsArrays(t *testing.T) {
	h := setupUpstreamTransfer(t)
	rec := postImport(t, h, `{
		"kind": "kiro-go-upstreams",
		"providers": [{"id":"x2","name":"fresh","baseUrl":"http://c/v1","apiKey":"sk-c","enabled":true}],
		"routes": [{"id":"y2","model":"brand-new","upstreamId":"x2","enabled":true}]
	}`)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"skippedProviders":[]`) || !strings.Contains(body, `"skippedRoutes":[]`) {
		t.Errorf("empty skip lists must marshal as [], not null: %s", body)
	}
}
