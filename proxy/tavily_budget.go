package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// tavilyBudget tracks Tavily credit spend against a monthly free-tier limit and
// persists it across restarts. The free tier (1000 credits/month) has no
// per-response "remaining balance" field, so we sum usage.credits client-side
// and refuse further Tavily calls once the month's budget is spent — the core of
// the "no accidental paid usage" guarantee.
//
// The counter resets when the calendar month (UTC) rolls over. State is a small
// JSON file written atomically (tmp+rename, 0600) alongside the config.
type tavilyBudget struct {
	mu    sync.Mutex
	path  string
	month string // "YYYY-MM" (UTC) the Spent counter belongs to
	spent int    // credits spent this month
	limit int    // monthly cap; <=0 disables the budget gate
}

// tavilyBudgetState is the on-disk shape.
type tavilyBudgetState struct {
	Month string `json:"month"`
	Spent int    `json:"spent"`
}

var (
	tavilyBudgetOnce sync.Once
	tavilyBudgetInst *tavilyBudget
)

// getTavilyBudget returns the process-wide budget, loading persisted state once.
func getTavilyBudget() *tavilyBudget {
	tavilyBudgetOnce.Do(func() {
		path := filepath.Join(config.GetConfigDir(), "tavily_budget.json")
		b := &tavilyBudget{path: path, month: currentBudgetMonth()}
		b.load()
		tavilyBudgetInst = b
	})
	// Refresh the limit from config every call (operator may have changed it).
	ws := config.GetWebSearchConfig()
	tavilyBudgetInst.mu.Lock()
	tavilyBudgetInst.limit = ws.Tavily.MonthlyCreditLimit
	tavilyBudgetInst.mu.Unlock()
	return tavilyBudgetInst
}

func currentBudgetMonth() string {
	return time.Now().UTC().Format("2006-01")
}

// load reads persisted state; a missing/corrupt file is a fresh start (not an
// error — a lost counter must never block startup).
func (b *tavilyBudget) load() {
	data, err := os.ReadFile(b.path)
	if err != nil {
		return
	}
	var st tavilyBudgetState
	if err := json.Unmarshal(data, &st); err != nil {
		logger.Warnf("[WebSearch] tavily budget state unreadable, starting fresh: %v", err)
		return
	}
	if st.Month == currentBudgetMonth() {
		b.month = st.Month
		b.spent = st.Spent
	}
	// A stale month means the budget has already rolled over: start at 0.
}

// rolloverLocked resets the counter if the calendar month changed. Caller holds mu.
func (b *tavilyBudget) rolloverLocked() {
	now := currentBudgetMonth()
	if b.month != now {
		b.month = now
		b.spent = 0
	}
}

// allow reports whether at least one more Tavily credit may be spent this month.
// A non-positive limit disables the gate (allow always true).
func (b *tavilyBudget) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rolloverLocked()
	if b.limit <= 0 {
		return true
	}
	return b.spent < b.limit
}

// remaining returns credits left this month (0 when exhausted, math.MaxInt-ish
// semantics collapsed to limit when unlimited is not meaningful for callers).
func (b *tavilyBudget) remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rolloverLocked()
	if b.limit <= 0 {
		return -1 // sentinel: unlimited
	}
	if b.spent >= b.limit {
		return 0
	}
	return b.limit - b.spent
}

// record adds spent credits and persists. credits<=0 is a no-op (a search that
// reported no usage still counts as at least the caller-provided amount).
func (b *tavilyBudget) record(credits int) {
	if credits <= 0 {
		return
	}
	b.mu.Lock()
	b.rolloverLocked()
	b.spent += credits
	st := tavilyBudgetState{Month: b.month, Spent: b.spent}
	path := b.path
	b.mu.Unlock()

	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logger.Warnf("[WebSearch] tavily budget persist failed: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logger.Warnf("[WebSearch] tavily budget rename failed: %v", err)
	}
}
