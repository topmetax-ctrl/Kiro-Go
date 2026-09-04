package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
	"kiro-go/metrics"
)

// GET /admin/api/tool-stats returns the web_search tool aggregate with the
// uses/executions/cache split the dashboard card renders.
func TestAdminToolStatsRoute(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(dir + "/config.json"); err != nil {
		t.Fatal(err)
	}
	metrics.ResetToolStats()
	metrics.RecordToolUsage(metrics.ToolUsage{
		ToolKind:   metrics.ToolKindWebSearch,
		Origin:     metrics.ToolOriginKiroOrchestrator,
		ProviderID: metrics.KiroPoolID,
		Backend:    "searxng",
		Uses:       2,
		Executions: 1,
		CacheHits:  1,
	})

	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/tool-stats?kind=web_search", nil)
	req.Header.Set("X-Admin-Password", config.GetPassword())
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var d struct {
		Stats metrics.ToolStat `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Stats.Uses != 2 || d.Stats.Executions != 1 || d.Stats.CacheHits != 1 {
		t.Fatalf("tool stats = uses %d execs %d cache %d, want 2/1/1",
			d.Stats.Uses, d.Stats.Executions, d.Stats.CacheHits)
	}
	if d.Stats.ToolKind != metrics.ToolKindWebSearch {
		t.Fatalf("toolKind = %q", d.Stats.ToolKind)
	}
}

// The list form (no ?kind=) returns every tool kind.
func TestAdminToolStatsAllKinds(t *testing.T) {
	metrics.ResetToolStats()
	metrics.RecordToolUsage(metrics.ToolUsage{
		ToolKind: metrics.ToolKindWebSearch, Origin: metrics.ToolOriginKiroOrchestrator, Uses: 1,
	})
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/tool-stats", nil)
	req.Header.Set("X-Admin-Password", config.GetPassword())
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
}
