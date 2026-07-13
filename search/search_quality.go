package search

import (
	"net/url"
	"strings"
	"time"
)

// Quality is the deterministic verdict on a provider's result set. The
// router uses Acceptable to decide whether to fall back to another provider.
type Quality struct {
	// Acceptable is the go/no-go: enough distinct, usable, on-topic results.
	Acceptable bool
	// Score is a 0..1 heuristic used only for logging/ranking, never as a gate by
	// itself (Acceptable is the gate).
	Score float64
	// Reason is a short human-readable explanation when not Acceptable.
	Reason string

	ValidResults    int
	DistinctDomains int
	DuplicateRatio  float64
}

// SearchQualityEvaluator scores a provider result set against the query. It is
// deterministic and free (no LLM), so it is safe on the hot path and easy to
// test.
type SearchQualityEvaluator interface {
	Evaluate(query string, results []Result) Quality
}

// heuristicQualityEvaluator is the default evaluator. minResults is the floor
// below which a result set is rejected (triggers fallback).
type heuristicQualityEvaluator struct {
	minResults        int
	minDistinctDomain int
	maxDuplicateRatio float64
}

func newHeuristicQualityEvaluator(minResults int) *heuristicQualityEvaluator {
	if minResults <= 0 {
		minResults = 1
	}
	return &heuristicQualityEvaluator{
		minResults:        minResults,
		minDistinctDomain: 1,
		maxDuplicateRatio: 0.8,
	}
}

// Evaluate computes the verdict. A result is "valid" when it has an http(s) URL
// and a non-empty title or snippet. The set is Acceptable when it has at least
// minResults valid results, at least one distinct domain, and is not almost
// entirely duplicates.
func (e *heuristicQualityEvaluator) Evaluate(query string, results []Result) Quality {
	q := Quality{}
	if len(results) == 0 {
		q.Reason = "no results"
		return q
	}

	domains := make(map[string]int)
	valid := 0
	overlapHits := 0
	queryTerms := tokenize(query)

	for _, r := range results {
		u := strings.TrimSpace(r.URL)
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			continue
		}
		if strings.TrimSpace(r.Title) == "" && strings.TrimSpace(r.Content) == "" {
			continue
		}
		valid++
		domains[strings.ToLower(parsed.Host)]++
		if lexicalOverlap(queryTerms, r.Title+" "+r.Content) > 0 {
			overlapHits++
		}
	}

	q.ValidResults = valid
	q.DistinctDomains = len(domains)
	if valid == 0 {
		q.Reason = "no valid results (missing http(s) URL or empty title/snippet)"
		return q
	}

	// Duplicate ratio: how concentrated the set is on its single most common
	// domain. 1 valid result on 1 domain is ratio 1.0 but not a "duplicate"
	// problem, so only penalize when there are several results piled on one domain.
	maxOnOneDomain := 0
	for _, c := range domains {
		if c > maxOnOneDomain {
			maxOnOneDomain = c
		}
	}
	q.DuplicateRatio = float64(maxOnOneDomain) / float64(valid)

	// Score blends coverage, diversity, and topical overlap. Advisory only.
	coverage := ratio(valid, e.minResults) // capped at 1 below
	diversity := ratio(len(domains), 2)
	overlap := float64(overlapHits) / float64(valid)
	q.Score = clamp01(0.5*coverage + 0.25*diversity + 0.25*overlap)

	switch {
	case valid < e.minResults:
		q.Reason = "below minimum result count"
	case len(domains) < e.minDistinctDomain:
		q.Reason = "insufficient domain diversity"
	case valid >= 3 && q.DuplicateRatio > e.maxDuplicateRatio:
		q.Reason = "results dominated by a single domain"
	default:
		q.Acceptable = true
	}
	return q
}

// tokenize lowercases and splits a string into word tokens, dropping very short
// tokens that carry little topical signal.
func tokenize(s string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(f) >= 3 {
			out[f] = true
		}
	}
	return out
}

// lexicalOverlap counts how many query terms appear in the text.
func lexicalOverlap(queryTerms map[string]bool, text string) int {
	if len(queryTerms) == 0 {
		return 0
	}
	textTerms := tokenize(text)
	n := 0
	for t := range queryTerms {
		if textTerms[t] {
			n++
		}
	}
	return n
}

// QueryWantsFreshness reports whether a query implies it wants recent results,
// so a caller can bias a time_range. Deterministic keyword match. Exported so
// the tool-loop tier can apply the same freshness heuristic when building a
// request.
func QueryWantsFreshness(query string) bool {
	q := strings.ToLower(query)
	for _, kw := range []string{"latest", "today", "current", "newest", "recent", "this year", "right now"} {
		if strings.Contains(q, kw) {
			return true
		}
	}
	return false
}

func ratio(n, d int) float64 {
	if d <= 0 {
		return 1
	}
	return clamp01(float64(n) / float64(d))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// parsePublishedDate best-effort parses common published-date formats to a time
// for freshness reranking. Returns zero time when unparseable.
func parsePublishedDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02", "2006/01/02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
