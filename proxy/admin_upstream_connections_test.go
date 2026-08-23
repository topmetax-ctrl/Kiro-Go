package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func setupConnAdmin(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{{
		ID: "p1", Name: "cun.ai", BaseURL: "https://api.example/v1",
		ApiKey: "sk-stored-secret-key", Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	return &Handler{}
}

func TestApiGetUpstreamsMasksConnectionSecrets(t *testing.T) {
	h := setupConnAdmin(t)
	rec := httptest.NewRecorder()
	h.apiGetUpstreams(rec, httptest.NewRequest(http.MethodGet, "/admin/api/upstreams", nil))
	body := rec.Body.String()
	if strings.Contains(body, "sk-stored-secret-key") {
		t.Fatalf("GET leaked raw key: %s", body)
	}
	if !strings.Contains(body, "apiKeyMasked") && !strings.Contains(body, "****") {
		t.Fatalf("expected masked view: %s", body)
	}
	var d struct {
		Providers []map[string]interface{} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Providers) != 1 {
		t.Fatalf("providers = %d", len(d.Providers))
	}
	conns, _ := d.Providers[0]["connections"].([]interface{})
	if len(conns) != 1 {
		t.Fatalf("connections = %d, want migrated 1", len(conns))
	}
}

func TestApiUpdateUpstreamsPreservesConnectionsWhenOmitted(t *testing.T) {
	h := setupConnAdmin(t)
	rec := httptest.NewRecorder()
	body := `{"providers":[{"id":"p1","name":"renamed","baseUrl":"https://api.example/v1","apiKey":"sk-sto****-key","enabled":true}],"routes":[]}`
	h.apiUpdateUpstreams(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	providers, _ := config.GetUpstreamConfig()
	if providers[0].Name != "renamed" {
		t.Fatalf("name = %q", providers[0].Name)
	}
	if len(providers[0].Connections) != 1 || providers[0].Connections[0].ApiKey != "sk-stored-secret-key" {
		t.Fatalf("connections wiped: %+v", providers[0].Connections)
	}
}

func TestConnectionCRUDAndImport(t *testing.T) {
	h := setupConnAdmin(t)
	add := httptest.NewRecorder()
	h.apiAddUpstreamConnection(add, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"name":"user2","apiKey":"sk-second-secret-key","enabled":true}`)), "p1")
	if add.Code != 200 {
		t.Fatalf("add %d %s", add.Code, add.Body.String())
	}
	if strings.Contains(add.Body.String(), "sk-second-secret-key") {
		t.Fatal("add response leaked raw key")
	}

	prev := httptest.NewRecorder()
	h.apiPreviewUpstreamConnections(prev, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"text":"user3 | password | sk-third-secret-key-abcdefgh","naming":"first_non_key"}`)), "p1")
	if prev.Code != 200 || !strings.Contains(prev.Body.String(), `"ready":1`) {
		t.Fatalf("preview %s", prev.Body.String())
	}
	if strings.Contains(prev.Body.String(), "sk-third-secret-key-abcdefgh") {
		t.Fatal("preview leaked raw key")
	}

	imp := httptest.NewRecorder()
	h.apiImportUpstreamConnections(imp, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"text":"user3 | password | sk-third-secret-key-abcdefgh\nsk-stored-secret-key","naming":"first_non_key"}`)), "p1")
	if imp.Code != 200 {
		t.Fatalf("import %s", imp.Body.String())
	}
	var ir map[string]interface{}
	if err := json.Unmarshal(imp.Body.Bytes(), &ir); err != nil {
		t.Fatal(err)
	}
	if ir["added"] != float64(1) {
		t.Fatalf("added = %v", ir["added"])
	}
	if ir["duplicate"] != float64(1) {
		t.Fatalf("duplicate = %v", ir["duplicate"])
	}
	providers, _ := config.GetUpstreamConfig()
	if len(providers[0].Connections) != 3 {
		t.Fatalf("want 3 connections, got %d", len(providers[0].Connections))
	}
}

func TestConnectionTestUsesStoredSecretAndTimeout(t *testing.T) {
	h := setupConnAdmin(t)
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer srv.Close()
	providers, routes := config.GetUpstreamConfig()
	providers[0].BaseURL = srv.URL
	if err := config.UpdateUpstreamConfig(providers, routes); err != nil {
		t.Fatal(err)
	}
	cid := providers[0].Connections[0].ID
	rec := httptest.NewRecorder()
	h.apiTestUpstreamConnection(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"model":"glm-5.3"}`)), "p1", cid)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if sawAuth != "Bearer sk-stored-secret-key" {
		t.Fatalf("auth = %q", sawAuth)
	}
	if strings.Contains(rec.Body.String(), "sk-stored-secret-key") {
		t.Fatal("test response leaked key")
	}

	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Second)
	}))
	defer hang.Close()
	providers[0].BaseURL = hang.URL
	if err := config.UpdateUpstreamConfig(providers, nil); err != nil {
		t.Fatal(err)
	}
	slow := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"model":"m"}`))
	start := time.Now()
	h.apiTestUpstreamConnection(slow, req, "p1", cid)
	if time.Since(start) > 18*time.Second {
		t.Fatal("timeout did not bound the test")
	}
	if !strings.Contains(slow.Body.String(), "timeout") {
		t.Fatalf("want timeout, got %s", slow.Body.String())
	}
}
