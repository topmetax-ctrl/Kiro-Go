package proxy

import (
	"strings"
	"sync"

	"kiro-go/cachetrace"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/metrics"
)

// Glue between the forwarding relay and package cachetrace. The relay asks for a
// trace when it builds the upstream request and reads it when it records the
// event; the digests are computed in between, beside the upstream call.

// cacheTracer returns the shared Tracer, built on first use. Its key derives from
// the api-key pepper, which already persists across restarts, so digests stay
// comparable across a redeploy. Without that secret tracing stays off.
func (h *Handler) cacheTracer() *cachetrace.Tracer {
	h.cacheTraceOnce.Do(func() {
		pepper, err := config.GetOrCreateAPIKeyPepper()
		if err != nil {
			logger.Warnf("[CacheTrace] off: no signing secret: %v", err)
			return
		}
		h.cacheTraceTracer = cachetrace.New(pepper)
	})
	return h.cacheTraceTracer
}

// startCacheTrace begins digesting a Claude Messages body and returns the reader
// for the result: nil when tracing is off, the route is not Claude-shaped, or the
// body is not a Messages request. The reader may be called any number of times.
// It never fails the relay: a panic while hashing yields no trace.
func (h *Handler) startCacheTrace(body []byte, isClaudeRoute bool) func() *metrics.CacheTrace {
	none := func() *metrics.CacheTrace { return nil }
	if !isClaudeRoute || !config.CacheTraceEnabled() {
		return none
	}
	t := h.cacheTracer()
	if t == nil {
		return none
	}
	policy := tokenOptPolicyLabel(config.GetTokenOptimization())
	done := make(chan *metrics.CacheTrace, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[CacheTrace] hashing panicked; event recorded without a trace: %v", r)
				done <- nil
			}
		}()
		done <- t.Trace(body, policy)
	}()
	var (
		once sync.Once
		res  *metrics.CacheTrace
	)
	return func() *metrics.CacheTrace {
		once.Do(func() { res = <-done })
		return res
	}
}

// tokenOptPolicyLabel names the Token Optimization settings in force, in a form
// that changes whenever they change what the prompt contains: "off", "observe",
// or "on" followed by each active part, e.g. "on+headroom-cache+caveman".
func tokenOptPolicyLabel(cfg config.TokenOptimizationConfig) string {
	switch cfg.Mode {
	case config.TokenOptModeOn:
	case config.TokenOptModeObserve:
		return config.TokenOptModeObserve
	default:
		return config.TokenOptModeOff
	}
	parts := []string{config.TokenOptModeOn}
	if cfg.InputCompression == config.TokenOptInputHeadroom {
		parts = append(parts, "headroom-"+cfg.Headroom.Mode)
	}
	if cfg.Caveman != "" && cfg.Caveman != config.TokenOptStyleOff {
		parts = append(parts, cfg.Caveman)
	}
	if cfg.Ponytail != "" && cfg.Ponytail != config.TokenOptPonytailOff {
		parts = append(parts, "ponytail-"+cfg.Ponytail)
	}
	return strings.Join(parts, "+")
}
