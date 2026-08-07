package proxy

import (
	"net/http"
	"testing"

	"kiro-go/metrics"
)

// The Kiro upstream reports no token counts of its own, so every handler success
// tail falls back to the local estimator through usageSplit. The pool-metrics
// bridge must be fed those *accounted* values, not the raw upstream variables:
// with the raw ones the Stats dashboard renders every pool request as 0 in / 0
// out even while the per-account and per-key counters are correct.
//
// TestRecordSuccessLogSplitFeedsMetrics covers recordSuccessLogSplit in
// isolation, which is exactly why it could not catch that regression — the
// defect was in what the four callers handed it. These cases drive the real
// handler tails end to end against an upstream that reports no usage at all.
func TestPoolMetricsUseAccountedTokensWhenUpstreamReportsNone(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    string
		account string
		apiKey  string
	}{
		{
			name:    "claude-stream",
			path:    "/v1/messages",
			body:    `{"model":"` + bigModel + `","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
			account: "acct-pm-cs",
			apiKey:  "key-pm-cs",
		},
		{
			name:    "claude-nonstream",
			path:    "/v1/messages",
			body:    `{"model":"` + bigModel + `","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
			account: "acct-pm-cn",
			apiKey:  "key-pm-cn",
		},
		{
			name:    "openai-stream",
			path:    "/v1/chat/completions",
			body:    `{"model":"` + bigModel + `","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			account: "acct-pm-os",
			apiKey:  "key-pm-os",
		},
		{
			name:    "openai-nonstream",
			path:    "/v1/chat/completions",
			body:    `{"model":"` + bigModel + `","stream":false,"messages":[{"role":"user","content":"hello"}]}`,
			account: "acct-pm-on",
			apiKey:  "key-pm-on",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics.Reset()
			env := newIntegrationEnv(t, tc.account, tc.apiKey, nil)
			// No usage frame and no contextUsageEvent: this is what a real Kiro
			// turn looks like on the wire, so every token number the proxy has
			// comes from its own estimator.
			fb := newFakeKiroBackend(t,
				kiroFrame{eventType: "assistantResponseEvent", payload: map[string]interface{}{"content": "A visible answer of some length."}},
				kiroFrame{eventType: "meteringEvent", payload: map[string]interface{}{"usage": caseACred}},
			)
			defer swapKiroEndpointsForTest(t, fb.server)()

			res := env.serveHTTP(t, fb, http.MethodPost, tc.path, tc.body)
			if res.HTTPStatus != http.StatusOK {
				t.Fatalf("status=%d body=%s", res.HTTPStatus, res.RawBody)
			}

			d, ok := metrics.ProviderDetailFor(metrics.KiroPoolID, 60)
			if !ok {
				t.Fatal("kiro pool not recorded in metrics")
			}
			if d.Requests != 1 || d.Success != 1 {
				t.Fatalf("requests/success = %d/%d, want 1/1", d.Requests, d.Success)
			}
			if d.InputTokens <= 0 || d.OutputTokens <= 0 {
				t.Fatalf("pool metrics tokens = %d in / %d out, want both > 0 (estimator fallback must reach the dashboard)",
					d.InputTokens, d.OutputTokens)
			}
			// The dashboard must agree with the counters the operator already
			// trusts: same split, same total as the per-key accounting sink.
			if got, want := d.InputTokens+d.OutputTokens, res.APIKeyTokensDelta; got != want {
				t.Fatalf("pool metrics total = %d (%d in / %d out), want per-key accounted total %d",
					got, d.InputTokens, d.OutputTokens, want)
			}
			if d.CostUSD != caseACred {
				t.Fatalf("pool metrics credits = %v, want %v", d.CostUSD, caseACred)
			}
			if len(d.Accounts) != 1 || d.Accounts[0].AccountID != tc.account {
				t.Fatalf("account breakdown = %+v, want single entry for %s", d.Accounts, tc.account)
			}
		})
	}
}
