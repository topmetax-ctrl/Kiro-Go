package metrics

import (
	"testing"
	"time"
)

func TestRecordToolUsageAggregates(t *testing.T) {
	ResetToolStats()
	now := time.Now().UnixMilli()

	RecordToolUsage(ToolUsage{
		TimeMs:     now,
		ToolKind:   ToolKindWebSearch,
		Origin:     ToolOriginKiroOrchestrator,
		ProviderID: KiroPoolID,
		Backend:    "searxng",
		Uses:       3,
		Executions: 2,
		CacheHits:  1,
		LatencyMs:  150,
	})
	RecordToolUsage(ToolUsage{
		TimeMs:     now + 1000,
		ToolKind:   ToolKindWebSearch,
		Origin:     ToolOriginUpstreamNative,
		ProviderID: "up-1",
		Uses:       5,
	})

	stats := ToolStatsFor(ToolKindWebSearch)
	if stats.Uses != 8 {
		t.Fatalf("Uses = %d, want 8", stats.Uses)
	}
	if stats.Executions != 2 {
		t.Fatalf("Executions = %d, want 2", stats.Executions)
	}
	if stats.CacheHits != 1 {
		t.Fatalf("CacheHits = %d, want 1", stats.CacheHits)
	}
	if stats.Requests != 2 {
		t.Fatalf("Requests = %d, want 2", stats.Requests)
	}

	// Check by-origin breakdown
	if stats.ByOrigin[ToolOriginKiroOrchestrator] != 3 {
		t.Fatalf("byOrigin kiro_orchestrator = %d, want 3", stats.ByOrigin[ToolOriginKiroOrchestrator])
	}
	if stats.ByOrigin[ToolOriginUpstreamNative] != 5 {
		t.Fatalf("byOrigin upstream_native = %d, want 5", stats.ByOrigin[ToolOriginUpstreamNative])
	}

	// Check by-backend breakdown
	if stats.ByBackend["searxng"] != 2 {
		t.Fatalf("byBackend searxng = %d, want 2 (executions)", stats.ByBackend["searxng"])
	}
}

func TestToolStatsForEmptyKind(t *testing.T) {
	ResetToolStats()
	stats := ToolStatsFor("nonexistent")
	if stats.Uses != 0 || stats.Requests != 0 {
		t.Fatalf("empty kind should return zeros, got %+v", stats)
	}
}

func TestResetToolStatsClearsAll(t *testing.T) {
	ResetToolStats()
	RecordToolUsage(ToolUsage{ToolKind: ToolKindWebSearch, Origin: ToolOriginKiroOrchestrator, Uses: 10})
	ResetToolStats()
	stats := ToolStatsFor(ToolKindWebSearch)
	if stats.Uses != 0 {
		t.Fatalf("after reset Uses = %d, want 0", stats.Uses)
	}
}

func TestResetToolKindClearsOneKind(t *testing.T) {
	ResetToolStats()
	RecordToolUsage(ToolUsage{ToolKind: ToolKindWebSearch, Origin: ToolOriginKiroOrchestrator, Uses: 5})
	ResetToolKind(ToolKindWebSearch)
	stats := ToolStatsFor(ToolKindWebSearch)
	if stats.Uses != 0 {
		t.Fatalf("after ResetToolKind Uses = %d, want 0", stats.Uses)
	}
}

func TestToolSubscribeReceivesEvents(t *testing.T) {
	ResetToolStats()
	ch, cancel := SubscribeTool()
	defer cancel()

	RecordToolUsage(ToolUsage{ToolKind: ToolKindWebSearch, Origin: ToolOriginKiroOrchestrator, Uses: 1})

	select {
	case ev := <-ch:
		if ev.ToolKind != ToolKindWebSearch || ev.Uses != 1 {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tool event")
	}
}
