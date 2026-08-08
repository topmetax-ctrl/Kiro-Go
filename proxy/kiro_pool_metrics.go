package proxy

// Bridges Kiro account-pool outcomes into the shared metrics store, so pool
// traffic appears in the admin dashboard alongside the upstream-forwarding
// providers instead of only in the 500-entry in-memory request log.
//
// The pool is recorded under the synthetic provider id metrics.KiroPoolID. Every
// Kiro request converges on recordSuccessLog / recordFailureWithDetails, which
// already carry endpoint, model, account id, tokens and duration — so this is the
// one place instrumentation is needed rather than the ~29 individual call sites.
//
// Two limitations are inherent to where those functions sit and are documented
// rather than papered over:
//
//   - Intermediate account attempts during failover are invisible here: the
//     executor retries internally and only the final outcome is recorded.
//   - The exhaustion paths pass an empty account id (the failing account is not
//     in scope by then), so such failures are attributed to the pool as a whole
//     and not to any single account.

import (
	"strings"

	"kiro-go/config"
	"kiro-go/metrics"
)

// kiroMetric is one Kiro-pool outcome, in the vocabulary the record* helpers
// already speak. It is translated into a metrics.Event by recordKiroMetric.
type kiroMetric struct {
	Endpoint     string
	Model        string
	AccountID    string
	Ok           bool
	InputTokens  int
	OutputTokens int
	Credits      float64
	DurationMs   int64
	ErrorMsg     string
	ErrorType    string

	// RouteID attributes this outcome to a forwarding route whose chain ended at
	// the Kiro-pool sentinel. Empty for ordinary pool traffic (no route matched),
	// and empty is what metrics.Record uses to skip per-route aggregation — so
	// leaving it unset is free. See pool_route_context.go for why it must be
	// captured from the raw client model rather than resolved here.
	RouteID string
}

// kiroStatusFor maps a pool outcome onto an HTTP-ish status code so the pool
// shares the status histogram used by forwarding providers. The pool's own
// renderers choose the real wire status separately; these mirror the categories
// classifyError produces.
func kiroStatusFor(m kiroMetric) int {
	if m.Ok {
		return 200
	}
	switch m.ErrorType {
	case "quota", "overage":
		return 429
	case "auth":
		return 401
	case "suspended":
		return 403
	case "profile":
		return 424
	default:
		return 500
	}
}

// kiroAccountLabel resolves an account id to a human-readable label for the
// per-account breakdown. It prefers the nickname, falls back to the email, and
// finally to a short id prefix so the UI always has something to show.
func kiroAccountLabel(accountID string) string {
	if accountID == "" {
		return ""
	}
	if acc := config.GetAccountByID(accountID); acc != nil {
		if n := strings.TrimSpace(acc.Nickname); n != "" {
			return n
		}
		if e := strings.TrimSpace(acc.Email); e != "" {
			return e
		}
	}
	if len(accountID) > 8 {
		return accountID[:8]
	}
	return accountID
}

// recordKiroMetric records one Kiro-pool outcome into the shared metrics store.
//
// Credits are carried through as CostUSD: for the Kiro pool the operator's real
// unit of spend is credits, not dollars, and the dashboard labels the pool's
// cost column accordingly.
func recordKiroMetric(m kiroMetric) {
	ev := metrics.Event{
		ClientModel:  m.Model,
		RouteID:      m.RouteID,
		ProviderID:   metrics.KiroPoolID,
		ProviderName: metrics.KiroPoolName,
		AccountID:    m.AccountID,
		AccountLabel: kiroAccountLabel(m.AccountID),
		Endpoint:     m.Endpoint,
		Status:       kiroStatusFor(m),
		LatencyMs:    m.DurationMs,
		InputTokens:  int64(m.InputTokens),
		OutputTokens: int64(m.OutputTokens),
		CostUSD:      m.Credits,
		Ok:           m.Ok,
		ErrorMsg:     m.ErrorMsg,
	}
	metrics.Record(ev)
}
