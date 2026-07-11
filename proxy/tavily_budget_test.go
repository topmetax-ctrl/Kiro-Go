package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestBudget builds an isolated budget backed by a temp file, bypassing the
// process-wide singleton so tests don't share state.
func newTestBudget(t *testing.T, limit int) *tavilyBudget {
	t.Helper()
	return &tavilyBudget{
		path:  filepath.Join(t.TempDir(), "tavily_budget.json"),
		month: currentBudgetMonth(),
		limit: limit,
	}
}

func TestTavilyBudgetAllowsUntilLimit(t *testing.T) {
	b := newTestBudget(t, 3)
	for i := 0; i < 3; i++ {
		if !b.allow() {
			t.Fatalf("expected allow before spending limit (i=%d)", i)
		}
		b.record(1)
	}
	if b.allow() {
		t.Fatalf("expected budget exhausted after spending the limit")
	}
	if got := b.remaining(); got != 0 {
		t.Fatalf("expected 0 remaining, got %d", got)
	}
}

func TestTavilyBudgetUnlimitedWhenNonPositive(t *testing.T) {
	b := newTestBudget(t, 0)
	b.record(1000)
	if !b.allow() {
		t.Fatalf("expected allow with non-positive (disabled) limit")
	}
	if got := b.remaining(); got != -1 {
		t.Fatalf("expected -1 (unlimited sentinel), got %d", got)
	}
}

func TestTavilyBudgetPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tavily_budget.json")

	b1 := &tavilyBudget{path: path, month: currentBudgetMonth(), limit: 10}
	b1.record(4)

	// A fresh instance pointed at the same file must see the spend.
	b2 := &tavilyBudget{path: path, month: currentBudgetMonth(), limit: 10}
	b2.load()
	if got := b2.remaining(); got != 6 {
		t.Fatalf("expected 6 remaining after reload, got %d", got)
	}
}

func TestTavilyBudgetRollsOverStaleMonth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tavily_budget.json")

	// Persist state for a month that is not the current one.
	if err := os.WriteFile(path, []byte(`{"month":"1999-01","spent":999}`), 0600); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	b := &tavilyBudget{path: path, month: currentBudgetMonth(), limit: 10}
	b.load()
	// Stale month is ignored: the counter starts fresh this month.
	if got := b.remaining(); got != 10 {
		t.Fatalf("expected full budget after month rollover, got %d", got)
	}
}
