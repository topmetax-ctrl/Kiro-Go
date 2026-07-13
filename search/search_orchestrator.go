package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"kiro-go/logger"
)

// queryHash is a short, stable fingerprint of a query for observability. The raw
// query is never logged (it may carry sensitive terms, spec §25/§26); the hash
// lets operators correlate log lines and cache behavior without leaking content.
func queryHash(query string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(query))))
	return hex.EncodeToString(sum[:])[:12]
}

// Orchestrator is the single seam the web_search executor depends on. It
// owns the whole "run one search" pipeline: cache lookup, provider routing
// (free-first), quality gating, reranking, and cache write. The executor never
// talks to a provider directly.
type Orchestrator interface {
	Search(ctx context.Context, req Request) (Response, Metadata, error)
}

// Metadata reports what one orchestrated search did, for accounting and
// observability. It carries no secrets and no raw result bodies.
type Metadata struct {
	Provider      string
	FallbackUsed  bool
	CacheHit      bool
	ResultCount   int
	LatencyMs     int64
	TavilyCredits int
	Quality       Quality
}

// searchOrchestrator is the production Orchestrator.
type searchOrchestrator struct {
	router   *ProviderRouter
	cache    SearchCache
	reranker SearchReranker
	// perCallTimeout bounds a single provider call inside the router. 0 means no
	// orchestrator-imposed per-call deadline (the parent ctx still applies).
	perCallTimeout time.Duration
	// maxFinalResults caps the reranked result set returned to the executor.
	maxFinalResults int
	// routingMode is folded into the cache key so responses from different routing
	// policies never collide.
	routingMode string
	// cacheTTL is the lifetime of a cached response.
	cacheTTL time.Duration
}

// newSearchOrchestrator wires the orchestrator from config-resolved parts.
func newSearchOrchestrator(router *ProviderRouter, cache SearchCache, reranker SearchReranker, perCallTimeout time.Duration, maxFinalResults int, routingMode string, cacheTTL time.Duration) *searchOrchestrator {
	if reranker == nil {
		reranker = newHeuristicReranker()
	}
	if maxFinalResults <= 0 {
		maxFinalResults = 8
	}
	if cacheTTL <= 0 {
		cacheTTL = 15 * time.Minute
	}
	return &searchOrchestrator{
		router:          router,
		cache:           cache,
		reranker:        reranker,
		perCallTimeout:  perCallTimeout,
		maxFinalResults: maxFinalResults,
		routingMode:     routingMode,
		cacheTTL:        cacheTTL,
	}
}

// Search runs the pipeline for one query.
func (o *searchOrchestrator) Search(ctx context.Context, req Request) (Response, Metadata, error) {
	started := time.Now()
	key := SearchCacheKey(req, o.routingMode)

	// Process cache: a hit skips the provider entirely (and any Tavily credit).
	if o.cache != nil {
		if cached, ok := o.cache.Get(key); ok {
			meta := Metadata{
				Provider:    cached.Provider,
				CacheHit:    true,
				ResultCount: len(cached.Results),
				LatencyMs:   time.Since(started).Milliseconds(),
			}
			logger.Debugf("[WebSearch] orchestrator cache hit: provider=%s results=%d", cached.Provider, len(cached.Results))
			return cached, meta, nil
		}
	}

	// Bound a single provider attempt so one slow provider cannot consume the
	// whole request budget. The router still honors the parent ctx.
	callCtx := ctx
	var cancel context.CancelFunc
	if o.perCallTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, o.perCallTimeout)
		defer cancel()
	}

	resp, outcome, err := o.router.Route(callCtx, req)
	if err != nil {
		return Response{}, Metadata{Provider: outcome.Provider, FallbackUsed: outcome.FallbackUsed, LatencyMs: time.Since(started).Milliseconds()}, err
	}

	// Book Tavily credits (if any) against the monthly budget.
	recordTavilyCredits(resp)

	// Rerank deterministically and cap to the final result count.
	resp.Results = o.reranker.Rerank(req.Query, resp.Results, o.maxFinalResults)

	meta := Metadata{
		Provider:      outcome.Provider,
		FallbackUsed:  outcome.FallbackUsed,
		ResultCount:   len(resp.Results),
		LatencyMs:     time.Since(started).Milliseconds(),
		TavilyCredits: resp.Credits,
		Quality:       outcome.Quality,
	}

	// Cache the reranked response (only when there is something worth caching).
	if o.cache != nil && len(resp.Results) > 0 {
		o.cache.Put(key, resp, o.cacheTTL)
	}

	// Structured, secret-free observability line (spec §25/§26): the query is
	// reduced to a short hash — never logged in the clear — while provider,
	// fallback, cache, result count, credits, and latency are surfaced.
	logger.Debugf("[WebSearch] orchestrator ok: query_hash=%s provider=%s fallback=%v cache_hit=%v results=%d tavily_credits=%d latency_ms=%d quality_score=%.2f",
		queryHash(req.Query), meta.Provider, meta.FallbackUsed, meta.CacheHit, meta.ResultCount, meta.TavilyCredits, meta.LatencyMs, meta.Quality.Score)
	return resp, meta, nil
}
