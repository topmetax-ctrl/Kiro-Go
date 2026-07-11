package proxy

import (
	"net/url"
	"sort"
	"strings"
	"time"
)

// SearchReranker reorders and trims merged results. The default is a
// deterministic heuristic (no paid LLM), so it is testable and free.
type SearchReranker interface {
	Rerank(query string, results []SearchResult, maxFinal int) []SearchResult
}

// heuristicReranker scores each result on topical overlap with the query, the
// provider relevance score, and (for freshness-seeking queries) recency, then
// returns the top maxFinal in a stable order.
type heuristicReranker struct{}

func newHeuristicReranker() *heuristicReranker { return &heuristicReranker{} }

type rankedResult struct {
	res   SearchResult
	score float64
	idx   int // original index, for stable tie-breaking
}

// Rerank returns up to maxFinal results ordered by blended score. Ties keep
// original order (stable). maxFinal <= 0 keeps all.
func (r *heuristicReranker) Rerank(query string, results []SearchResult, maxFinal int) []SearchResult {
	if len(results) == 0 {
		return results
	}
	terms := tokenize(query)
	wantFresh := queryWantsFreshness(query)
	now := time.Now()

	ranked := make([]rankedResult, 0, len(results))
	var maxProvider float64
	for _, r := range results {
		if r.Score > maxProvider {
			maxProvider = r.Score
		}
	}

	for i, res := range results {
		overlap := 0.0
		if len(terms) > 0 {
			overlap = float64(lexicalOverlap(terms, res.Title+" "+res.Content)) / float64(len(terms))
		}
		// Title-hosted terms weigh a little more than snippet-only ones.
		titleOverlap := 0.0
		if len(terms) > 0 {
			titleOverlap = float64(lexicalOverlap(terms, res.Title)) / float64(len(terms))
		}

		providerScore := 0.0
		if maxProvider > 0 {
			providerScore = res.Score / maxProvider
		}

		freshness := 0.0
		if wantFresh {
			if t := parsePublishedDate(res.PublishedDate); !t.IsZero() {
				age := now.Sub(t)
				switch {
				case age < 30*24*time.Hour:
					freshness = 1.0
				case age < 365*24*time.Hour:
					freshness = 0.5
				default:
					freshness = 0.1
				}
			}
		}

		score := 0.4*overlap + 0.2*titleOverlap + 0.3*providerScore + 0.1*freshness
		ranked = append(ranked, rankedResult{res: res, score: score, idx: i})
	}

	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score == ranked[b].score {
			return ranked[a].idx < ranked[b].idx
		}
		return ranked[a].score > ranked[b].score
	})

	if maxFinal > 0 && len(ranked) > maxFinal {
		ranked = ranked[:maxFinal]
	}
	out := make([]SearchResult, 0, len(ranked))
	for _, rr := range ranked {
		out = append(out, rr.res)
	}
	return out
}

// mergeResults concatenates result sets from multiple providers and dedups by
// canonical URL, keeping the first occurrence (primary provider wins ties).
func mergeResults(sets ...[]SearchResult) []SearchResult {
	seen := make(map[string]bool)
	var out []SearchResult
	for _, set := range sets {
		for _, r := range set {
			u := strings.TrimSpace(r.URL)
			parsed, err := url.Parse(u)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				continue
			}
			key := canonicalURL(parsed)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}
