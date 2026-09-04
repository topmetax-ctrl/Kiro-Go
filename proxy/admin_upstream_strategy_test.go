package proxy

// The admin round-trip contract for UpstreamProvider.WebSearchStrategy.
//
// The panel loads every provider with GET /upstreams and writes them all back
// with POST /upstreams, so a field the GET projection omits is a field the next
// save silently clears. That failure mode is invisible in the UI — an operator
// editing one provider's price would wipe every configured strategy — so it is
// pinned here rather than left to review.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kiro-go/config"
)

func TestPublicProviderExposesWebSearchStrategy(t *testing.T) {
	p := config.UpstreamProvider{ID: "p1", Name: "n", BaseURL: "http://x", WebSearchStrategy: "local"}
	out := publicProvider(p)
	got, present := out["webSearchStrategy"]
	if !present {
		t.Fatal("webSearchStrategy missing from the GET projection: the next admin save would clear it")
	}
	if got != "local" {
		t.Errorf("webSearchStrategy = %v, want local", got)
	}

	// Unset must still be present (as empty), for the same reason.
	out2 := publicProvider(config.UpstreamProvider{ID: "p2"})
	if v, present := out2["webSearchStrategy"]; !present || v != "" {
		t.Errorf("unset strategy = %v present=%v, want \"\" present=true", v, present)
	}
}

// TestUpstreamStrategySurvivesAdminRoundTrip drives the real handlers: GET the
// providers, post them back unchanged, and require the strategy to still be
// there. This is the exact sequence the admin panel performs on every edit.
func TestUpstreamStrategySurvivesAdminRoundTrip(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateSettings("", false, "pw"); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	providers := []config.UpstreamProvider{
		{ID: "p-local", Name: "local-one", BaseURL: "http://a", ApiKey: "secret-a", Enabled: true, WebSearchStrategy: "local"},
		{ID: "p-native", Name: "native-one", BaseURL: "http://b", ApiKey: "secret-b", Enabled: true, WebSearchStrategy: "native"},
		{ID: "p-unset", Name: "unset-one", BaseURL: "http://c", ApiKey: "secret-c", Enabled: true},
	}
	if err := config.UpdateUpstreamConfig(providers, nil); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}

	h := &Handler{}

	// GET, as the panel does on load.
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/admin/api/upstreams", nil)
	getReq.Header.Set("X-Admin-Password", "pw")
	h.handleAdminAPI(getRec, getReq)
	if getRec.Code != 200 {
		t.Fatalf("GET status = %d: %s", getRec.Code, getRec.Body.String())
	}
	var loaded struct {
		Providers []map[string]interface{} `json:"providers"`
		Routes    []config.ModelRoute      `json:"routes"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &loaded); err != nil {
		t.Fatalf("GET body: %v", err)
	}
	if len(loaded.Providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(loaded.Providers))
	}

	// POST them straight back, exactly as the panel does after any edit. The
	// panel drops `connections` so the server keeps the stored list.
	for _, p := range loaded.Providers {
		delete(p, "connections")
	}
	body, err := json.Marshal(map[string]interface{}{"providers": loaded.Providers, "routes": loaded.Routes})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	postRec := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/admin/api/upstreams", bytes.NewReader(body))
	postReq.Header.Set("X-Admin-Password", "pw")
	h.handleAdminAPI(postRec, postReq)
	if postRec.Code != 200 {
		t.Fatalf("POST status = %d: %s", postRec.Code, postRec.Body.String())
	}

	stored, _ := config.GetUpstreamConfig()
	byID := map[string]config.UpstreamProvider{}
	for _, p := range stored {
		byID[p.ID] = p
	}
	for id, want := range map[string]string{"p-local": "local", "p-native": "native", "p-unset": ""} {
		p, ok := byID[id]
		if !ok {
			t.Errorf("provider %s vanished across the round trip", id)
			continue
		}
		if p.WebSearchStrategy != want {
			t.Errorf("provider %s strategy = %q after round trip, want %q", id, p.WebSearchStrategy, want)
		}
		// The resolved enum is what runtime code branches on; check it agrees.
		if p.WebSearchStrategyResolved() != config.ParseProviderWebSearchStrategy(want) {
			t.Errorf("provider %s resolved strategy disagrees with raw %q", id, p.WebSearchStrategy)
		}
	}
	// And the masked API keys must not have been written back as literal "****".
	for id, want := range map[string]string{"p-local": "secret-a", "p-native": "secret-b", "p-unset": "secret-c"} {
		if got := byID[id].ApiKey; got != want {
			t.Errorf("provider %s apiKey = %q after round trip, want the stored secret preserved", id, got)
		}
	}
}
