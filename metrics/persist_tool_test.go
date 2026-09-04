package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestToolStatsPersistRoundTrip(t *testing.T) {
	reset(t)
	ResetToolStats()

	RecordToolUsage(ToolUsage{
		TimeMs:     time.Now().UnixMilli(),
		ToolKind:   ToolKindWebSearch,
		Origin:     ToolOriginKiroOrchestrator,
		ProviderID: KiroPoolID,
		Backend:    "tavily",
		Uses:       4,
		Executions: 3,
		CacheHits:  1,
		LatencyMs:  800,
		Credits:    4,
	})

	path := filepath.Join(t.TempDir(), "metrics.json")
	if err := Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Load resets the store; confirm tool stats restored.
	if len(ts.overall) == 0 {
		t.Fatal("tool stats not loaded")
	}
	a := ts.overall[ToolKindWebSearch]
	if a == nil {
		t.Fatalf("tool kind web_search missing after load, have %v", keysOfToolAggs())
	}
	if a.uses != 4 || a.executions != 3 || a.cacheHits != 1 {
		t.Fatalf("restored uses/executions/cacheHits = %d/%d/%d, want 4/3/1", a.uses, a.executions, a.cacheHits)
	}
	if a.credits != 4 {
		t.Fatalf("restored credits = %d, want 4", a.credits)
	}
	// Backend breakdown must survive too
	if bc := a.byBackend["tavily"]; bc == nil || bc.executions != 3 {
		t.Fatalf("restored byBackend tavily = %+v", bc)
	}
}

func TestLoadLegacyMetricsWithoutToolStats(t *testing.T) {
	reset(t)
	ResetToolStats()

	// A legacy file with no toolStats field.
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.json")
	legacy := `{"overall":{"requests":5,"success":4,"failed":1},"byProvider":{"p1":{"requests":5}},"byRoute":{},"byIp":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	// Tool stats must be empty (additive schema, missing = zero).
	if len(ts.overall) != 0 {
		t.Fatalf("legacy file must load with zero tool stats, got %v", keysOfToolAggs())
	}
	// Request stats still load fine.
	if s.overall.requests != 5 {
		t.Fatalf("requests = %d, want 5", s.overall.requests)
	}
}

func keysOfToolAggs() []ToolKind {
	out := make([]ToolKind, 0, len(ts.overall))
	for k := range ts.overall {
		out = append(out, k)
	}
	return out
}
