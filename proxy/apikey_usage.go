package proxy

import (
	"context"
	"sync"
	"unicode/utf8"

	"kiro-go/apikey"
)

// apiKeyUsageAcc collects the best-known resource consumption for one
// inference while the handler is still running. It is flushed onto the lease
// on exit so cancel/fail paths still charge observed tokens.
type apiKeyUsageAcc struct {
	mu              sync.Mutex
	providerStarted bool
	estimatedInput  int
	upstreamIn      int
	upstreamOut     int
	credits         float64
	observed        []byte
	observedThink   []byte
}

func newAPIKeyUsageAcc(estimatedInput int) *apiKeyUsageAcc {
	return &apiKeyUsageAcc{estimatedInput: estimatedInput}
}

func (a *apiKeyUsageAcc) markProviderStarted() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.providerStarted = true
	a.mu.Unlock()
}

func (a *apiKeyUsageAcc) setUpstream(in, out int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if in > a.upstreamIn {
		a.upstreamIn = in
	}
	if out > a.upstreamOut {
		a.upstreamOut = out
	}
	a.mu.Unlock()
}

func (a *apiKeyUsageAcc) setCredits(c float64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if c > a.credits {
		a.credits = c
	}
	a.mu.Unlock()
}

func (a *apiKeyUsageAcc) observeText(text string, thinking bool) {
	if a == nil || text == "" {
		return
	}
	a.mu.Lock()
	if thinking {
		a.observedThink = append(a.observedThink, text...)
	} else {
		a.observed = append(a.observed, text...)
	}
	a.mu.Unlock()
}

func (a *apiKeyUsageAcc) resetObserved() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.upstreamIn = 0
	a.upstreamOut = 0
	a.credits = 0
	a.observed = a.observed[:0]
	a.observedThink = a.observedThink[:0]
	a.mu.Unlock()
}

func (a *apiKeyUsageAcc) snapshot() (in, out int, credits float64, source string, estimated bool) {
	if a == nil {
		return 0, 0, 0, apikey.UsageSourceNone, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.providerStarted {
		return 0, 0, 0, apikey.UsageSourceNone, false
	}
	observedOut := estimateApproxTokens(string(a.observed)) + estimateApproxTokens(string(a.observedThink))
	if utf8.RuneCount(a.observed)+utf8.RuneCount(a.observedThink) > 0 && observedOut < 1 {
		observedOut = 1
	}
	in = a.upstreamIn
	out = a.upstreamOut
	credits = a.credits
	switch {
	case a.upstreamIn > 0 || a.upstreamOut > 0:
		source = apikey.UsageSourceUpstream
		estimated = false
		if in <= 0 {
			in = a.estimatedInput
			estimated = true
		}
		if out <= 0 && observedOut > 0 {
			out = observedOut
			estimated = true
		}
	case observedOut > 0:
		in = a.estimatedInput
		out = observedOut
		source = apikey.UsageSourceStreamObserved
		estimated = true
	case a.estimatedInput > 0 || credits > 0:
		in = a.estimatedInput
		if credits > 0 && in == 0 && out == 0 {
			source = apikey.UsageSourceMetering
			estimated = false
		} else {
			source = apikey.UsageSourceEstimator
			estimated = true
		}
	default:
		source = apikey.UsageSourceNone
	}
	return in, out, credits, source, estimated
}

func usageProvenance(upstreamIn, upstreamOut, accountedIn, accountedOut int, credits float64) (source string, estimated bool) {
	if upstreamIn > 0 || upstreamOut > 0 {
		return apikey.UsageSourceUpstream, false
	}
	if accountedIn > 0 || accountedOut > 0 {
		return apikey.UsageSourceEstimator, true
	}
	if credits > 0 {
		return apikey.UsageSourceMetering, false
	}
	return apikey.UsageSourceNone, false
}

func noteLeaseUsageOnExit(ctx context.Context, acc *apiKeyUsageAcc) {
	if leaseFromContext(ctx) == nil || acc == nil {
		return
	}
	if l := leaseFromContext(ctx); l != nil {
		l.mu.Lock()
		alreadySuccess := l.noted && l.input.Outcome == apikey.OutcomeSuccess
		l.mu.Unlock()
		if alreadySuccess {
			return
		}
	}
	in, out, credits, source, estimated := acc.snapshot()
	if in == 0 && out == 0 && credits == 0 {
		return
	}
	noteAPIKeyUsage(ctx, int64(in), int64(out), credits, source, estimated)
}

// observeKiroCallback wraps stream callbacks so cancel/fail still see observed
// text and any upstream usage/credits that arrived before the stream died.
func observeKiroCallback(acc *apiKeyUsageAcc, cb *KiroStreamCallback) *KiroStreamCallback {
	if acc == nil || cb == nil {
		return cb
	}
	origText := cb.OnText
	cb.OnText = func(text string, isThinking bool) {
		acc.observeText(text, isThinking)
		if origText != nil {
			origText(text, isThinking)
		}
	}
	origComplete := cb.OnComplete
	cb.OnComplete = func(in, out int) {
		acc.setUpstream(in, out)
		if origComplete != nil {
			origComplete(in, out)
		}
	}
	origCredits := cb.OnCredits
	cb.OnCredits = func(c float64) {
		acc.setCredits(c)
		if origCredits != nil {
			origCredits(c)
		}
	}
	return cb
}
