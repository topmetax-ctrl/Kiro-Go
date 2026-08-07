package search

import (
	"errors"
	"testing"
)

// asProviderError is a test helper wrapping errors.As for *ProviderError.
func asProviderError(err error, target **ProviderError) bool {
	return errors.As(err, target)
}

func TestQualityRejectsEmpty(t *testing.T) {
	e := newHeuristicQualityEvaluator(3)
	q := e.Evaluate("go release", nil)
	if q.Acceptable {
		t.Fatal("empty result set must not be acceptable")
	}
}

func TestQualityRejectsBelowMinimum(t *testing.T) {
	e := newHeuristicQualityEvaluator(3)
	q := e.Evaluate("go release", []Result{
		{Title: "A", URL: "https://a.example", Content: "go release info"},
		{Title: "B", URL: "https://b.example", Content: "more go"},
	})
	if q.Acceptable {
		t.Fatalf("2 results below min 3 must be rejected, got %+v", q)
	}
	if q.ValidResults != 2 {
		t.Errorf("valid=%d", q.ValidResults)
	}
}

func TestQualityAcceptsGoodSet(t *testing.T) {
	e := newHeuristicQualityEvaluator(2)
	q := e.Evaluate("go release version", []Result{
		{Title: "Go Downloads", URL: "https://go.dev/dl", Content: "go release version 1.26"},
		{Title: "Go Blog", URL: "https://blog.example", Content: "go release notes"},
		{Title: "Wiki", URL: "https://wiki.example", Content: "go version history"},
	})
	if !q.Acceptable {
		t.Fatalf("good set should be acceptable: %+v", q)
	}
	if q.DistinctDomains != 3 {
		t.Errorf("distinct domains=%d", q.DistinctDomains)
	}
}

func TestQualityRejectsSingleDomainDominance(t *testing.T) {
	e := newHeuristicQualityEvaluator(2)
	// 4 results all on one domain: dominated.
	q := e.Evaluate("topic", []Result{
		{Title: "1", URL: "https://x.example/a", Content: "topic a"},
		{Title: "2", URL: "https://x.example/b", Content: "topic b"},
		{Title: "3", URL: "https://x.example/c", Content: "topic c"},
		{Title: "4", URL: "https://x.example/d", Content: "topic d"},
	})
	if q.Acceptable {
		t.Fatalf("single-domain-dominated set should be rejected: %+v", q)
	}
}

func TestQualityDropsInvalidURLs(t *testing.T) {
	e := newHeuristicQualityEvaluator(1)
	q := e.Evaluate("x", []Result{
		{Title: "bad", URL: "ftp://nope", Content: "x"},
		{Title: "", URL: "https://ok.example", Content: ""}, // no title/snippet → invalid
		{Title: "good", URL: "https://good.example", Content: "x"},
	})
	if q.ValidResults != 1 {
		t.Fatalf("only 1 valid result expected, got %d (%+v)", q.ValidResults, q)
	}
}

func TestQueryWantsFreshness(t *testing.T) {
	for _, s := range []string{"latest go version", "what is TODAY's date", "current president", "newest iPhone"} {
		if !QueryWantsFreshness(s) {
			t.Errorf("%q should want freshness", s)
		}
	}
	for _, s := range []string{"history of rome", "how does tcp work"} {
		if QueryWantsFreshness(s) {
			t.Errorf("%q should not want freshness", s)
		}
	}
}
