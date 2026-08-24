package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

// GET /admin/api/forward-events backs the Forwarding tab's event table. It was
// silently dropped from the admin dispatcher once, leaving the tab on SSE-only
// data (empty until the next request arrives, and empty again on every reload),
// so the route itself is worth pinning.
func TestAdminForwardEventsRouteIsRegistered(t *testing.T) {
	metrics.Reset()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	metrics.Record(metrics.Event{
		ProviderID: "up-1", ProviderName: "p1", ClientModel: "m1", Endpoint: "claude",
		Ok: true, Status: 200, LatencyMs: 42,
	})

	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/forward-events?offset=0&limit=50", nil)
	req.Header.Set("X-Admin-Password", config.GetPassword())
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Total int                      `json:"total"`
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("total=%d items=%d, want the recorded event", body.Total, len(body.Items))
	}
	if body.Items[0]["providerName"] != "p1" {
		t.Fatalf("items[0] = %#v", body.Items[0])
	}
}
