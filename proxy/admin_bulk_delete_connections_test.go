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

// The key list's "Delete Selected" button posts one request for the whole
// selection. Pin the route and its contract: the path parser has to treat
// bulk-delete as an action rather than a connection ID, and the response has to
// report how many rows actually went, since the selection can be stale.

func setupBulkDeleteProvider(t *testing.T) []config.UpstreamConnection {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	conns := []config.UpstreamConnection{
		{ID: "c1", Name: "Key 1", ApiKey: "sk-FIXTUREnotarealkey000000001", Enabled: true},
		{ID: "c2", Name: "Key 2", ApiKey: "sk-FIXTUREnotarealkey000000002", Enabled: true},
		{ID: "c3", Name: "Key 3", ApiKey: "sk-FIXTUREnotarealkey000000003", Enabled: true},
	}
	p := config.UpstreamProvider{ID: "p1", Name: "prov", BaseURL: "https://h/v1", Enabled: true, Connections: conns}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{p}, nil); err != nil {
		t.Fatal(err)
	}
	return conns
}

func postBulkDelete(t *testing.T, providerID, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost,
		"/admin/api/upstreams/"+providerID+"/connections/bulk-delete", strings.NewReader(body))
	req.Header.Set("X-Admin-Password", config.GetPassword())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	return rec
}

func TestAdminBulkDeleteConnectionsRoute(t *testing.T) {
	setupBulkDeleteProvider(t)
	rec := postBulkDelete(t, "p1", `{"ids":["c1","c3"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool `json:"success"`
		Removed int  `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Success || body.Removed != 2 {
		t.Fatalf("body = %+v, want success with removed=2", body)
	}
	providers, _ := config.GetUpstreamConfig()
	if len(providers[0].Connections) != 1 || providers[0].Connections[0].ID != "c2" {
		t.Fatalf("kept %+v, want only c2", providers[0].Connections)
	}
}

func TestAdminBulkDeleteConnectionsReportsStaleSelection(t *testing.T) {
	setupBulkDeleteProvider(t)
	rec := postBulkDelete(t, "p1", `{"ids":["c1","already-gone"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Removed != 1 {
		t.Fatalf("removed = %d, want 1 (the unknown id is skipped, not fatal)", body.Removed)
	}
}

func TestAdminBulkDeleteConnectionsNoMatch404(t *testing.T) {
	setupBulkDeleteProvider(t)
	rec := postBulkDelete(t, "p1", `{"ids":["nope"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	providers, _ := config.GetUpstreamConfig()
	if len(providers[0].Connections) != 3 {
		t.Fatal("a failed bulk delete must leave the pool alone")
	}
}

func TestAdminBulkDeleteConnectionsUnknownProvider404(t *testing.T) {
	setupBulkDeleteProvider(t)
	if rec := postBulkDelete(t, "nope", `{"ids":["c1"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminBulkDeleteConnectionsRejectsBadJSON(t *testing.T) {
	setupBulkDeleteProvider(t)
	if rec := postBulkDelete(t, "p1", `{"ids":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBulkDeletePathParsesAsActionNotConnectionID(t *testing.T) {
	pid, cid, action, ok := parseUpstreamConnectionPath("/upstreams/p1/connections/bulk-delete")
	if !ok || pid != "p1" || cid != "" || action != "bulk-delete" {
		t.Fatalf("parse = (%q, %q, %q, %v)", pid, cid, action, ok)
	}
	// A real connection ID on the same shape must still route to the row handlers.
	pid, cid, action, ok = parseUpstreamConnectionPath("/upstreams/p1/connections/c1")
	if !ok || pid != "p1" || cid != "c1" || action != "" {
		t.Fatalf("parse = (%q, %q, %q, %v)", pid, cid, action, ok)
	}
}
