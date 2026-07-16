package search

import (
	"net/url"
	"sort"
	"strings"
	"time"
)

// SearchReranker reorders and trims merged results. The default is a
// deterministic heuristic (no paid LLM), so it is testable and free.
type SearchReranker interface {
	Rerank(query string, results []Result, maxFinal int) []Result
}

// heuristicReranker scores each result on topical overlap with the query, the
// provider relevance score, and (for freshness-seeking queries) recency, then
// returns the top maxFinal in a stable order.
type heuristicReranker struct{}

func newHeuristicReranker() *heuristicReranker { return &heuristicReranker{} }

type rankedResult struct {
	res   Result
	score float64
	idx   int // original index, for stable tie-breaking
}

// Rerank returns up to maxFinal results ordered by blended score. Ties keep
// original order (stable). maxFinal <= 0 keeps all.
func (r *heuristicReranker) Rerank(query string, results []Result, maxFinal int) []Result {
	if len(results) == 0 {
		return results
	}
	terms := tokenize(query)
	wantFresh := QueryWantsFreshness(query)
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
	out := make([]Result, 0, len(ranked))
	for _, rr := range ranked {
		out = append(out, rr.res)
	}
	return out
}

// CanonicalURL builds a dedup key: scheme+host+path, lowercased host, trailing
// slash trimmed, query/fragment dropped. Exported so the tool-loop tier (which
// normalizes and dedups results before handing them to Kiro) shares the exact
// canonicalization the core uses.
func CanonicalURL(u *url.URL) string {
	host := strings.ToLower(u.Host)
	path := strings.TrimRight(u.Path, "/")
	return u.Scheme + "://" + host + path
}

// mergeResults concatenates result sets from multiple providers and dedups by
// canonical URL, keeping the first occurrence (primary provider wins ties).
func mergeResults(sets ...[]Result) []Result {
	seen := make(map[string]bool)
	var out []Result
	for _, set := range sets {
		for _, r := range set {
			u := strings.TrimSpace(r.URL)
			parsed, err := url.Parse(u)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				continue
			}
			key := CanonicalURL(parsed)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}
