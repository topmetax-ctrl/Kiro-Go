package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestTavily builds a provider pointed at a test server with a fixed key and
// tight retry so tests stay fast. It bypasses config-derived key resolution.
func newTestTavily(endpoint string, retryMax int) *TavilyProvider {
	return &TavilyProvider{
		endpoint:    endpoint,
		apiKey:      func() string { return "tvly-test-key-SECRET" },
		client:      func() *http.Client { return &http.Client{Timeout: 5 * time.Second} },
		retryMax:    retryMax,
		retryBaseMs: 1,
	}
}

func TestTavilySearchHappyPath(t *testing.T) {
	var gotAuth string
	var gotBody tavilyAPIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tavilyAPIResponse{
			Query:  "go version",
			Answer: "Go 1.26.5",
			Results: []tavilyAPIResult{
				{Title: "Go Downloads", URL: "https://go.dev/dl/", Content: "latest is 1.26.5", Score: 0.9, PublishedDate: "2026-06-01"},
			},
		})
	}))
	defer srv.Close()

	p := newTestTavily(srv.URL, 0)
	resp, err := p.Search(context.Background(), SearchRequest{
		Query:          "go version",
		MaxResults:     5,
		SearchDepth:    "basic",
		IncludeAnswer:  true,
		AllowedDomains: []string{"go.dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer tvly-test-key-SECRET" {
		t.Errorf("expected Bearer auth header, got %q", gotAuth)
	}
	if gotBody.Query != "go version" || gotBody.SearchDepth != "basic" || gotBody.MaxResults != 5 {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
	if gotBody.AutoParameters {
		t.Errorf("auto_parameters must be false")
	}
	if len(gotBody.IncludeDomains) != 1 || gotBody.IncludeDomains[0] != "go.dev" {
		t.Errorf("expected include_domains=[go.dev], got %v", gotBody.IncludeDomains)
	}
	if resp.Answer != "Go 1.26.5" || len(resp.Results) != 1 || resp.Results[0].URL != "https://go.dev/dl/" {
		t.Errorf("unexpected parsed response: %+v", resp)
	}
	if resp.Provider != "tavily" {
		t.Errorf("expected provider tavily, got %q", resp.Provider)
	}
}

func TestTavilyEmptyResultsIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tavilyAPIResponse{Query: "x", Results: nil})
	}))
	defer srv.Close()
	p := newTestTavily(srv.URL, 0)
	resp, err := p.Search(context.Background(), SearchRequest{Query: "x", MaxResults: 5})
	if err != nil {
		t.Fatalf("empty results must not be an error, got %v", err)
	}
	if len(resp.Results) != 0 {
		t.Errorf("expected 0 results, got %d", len(resp.Results))
	}
}

func TestTavilyMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()
	p := newTestTavily(srv.URL, 0)
	_, err := p.Search(context.Background(), SearchRequest{Query: "x", MaxResults: 5})
	var provErr *SearchProviderError
	if !errors.As(err, &provErr) || provErr.Kind != SearchErrMalformed {
		t.Fatalf("expected malformed provider error, got %v", err)
	}
}

func TestTavilyStatusClassification(t *testing.T) {
	cases := []struct {
		status int
		kind   SearchProviderErrorKind
		retry  bool
	}{
		{400, SearchErrInvalid, false},
		{401, SearchErrAuth, false},
		{403, SearchErrAuth, false},
		{429, SearchErrRateLimit, true},
		{500, SearchErrUpstream5xx, true},
		{503, SearchErrUpstream5xx, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte("error body"))
		}))
		p := newTestTavily(srv.URL, 0)
		_, err := p.Search(context.Background(), SearchRequest{Query: "x", MaxResults: 5})
		srv.Close()
		var provErr *SearchProviderError
		if !errors.As(err, &provErr) {
			t.Errorf("status %d: expected provider error, got %v", tc.status, err)
			continue
		}
		if provErr.Kind != tc.kind {
			t.Errorf("status %d: expected kind %s, got %s", tc.status, tc.kind, provErr.Kind)
		}
		if provErr.Kind.retryable() != tc.retry {
			t.Errorf("status %d: expected retryable=%v", tc.status, tc.retry)
		}
	}
}

func TestTavilyAuthErrorNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(401)
	}))
	defer srv.Close()
	p := newTestTavily(srv.URL, 3)
	_, err := p.Search(context.Background(), SearchRequest{Query: "x", MaxResults: 5})
	if err == nil {
		t.Fatal("expected auth error")
	}
	if calls != 1 {
		t.Errorf("auth error must not retry; got %d calls", calls)
	}
}

func TestTavilyRateLimitRetries(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(429)
			return
		}
		json.NewEncoder(w).Encode(tavilyAPIResponse{Query: "x"})
	}))
	defer srv.Close()
	p := newTestTavily(srv.URL, 3)
	_, err := p.Search(context.Background(), SearchRequest{Query: "x", MaxResults: 5})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (2 retries), got %d", calls)
	}
}

func TestTavilyRetriesExhausted(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer srv.Close()
	p := newTestTavily(srv.URL, 2)
	_, err := p.Search(context.Background(), SearchRequest{Query: "x", MaxResults: 5})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (1 + 2 retries), got %d", calls)
	}
}

func TestTavilyContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()
	p := newTestTavily(srv.URL, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := p.Search(ctx, SearchRequest{Query: "x", MaxResults: 5})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestTavilyMissingKeyIsConfigError(t *testing.T) {
	p := &TavilyProvider{
		endpoint: "https://unused.example",
		apiKey:   func() string { return "" },
		client:   func() *http.Client { return &http.Client{} },
	}
	_, err := p.Search(context.Background(), SearchRequest{Query: "x"})
	var cfgErr *SearchConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected SearchConfigError, got %v", err)
	}
}

func TestTavilyKeyNeverInError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte("unauthorized"))
	}))
	defer srv.Close()
	p := newTestTavily(srv.URL, 0)
	_, err := p.Search(context.Background(), SearchRequest{Query: "x", MaxResults: 5})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "tvly-test-key-SECRET") {
		t.Errorf("API key leaked into error: %v", err)
	}
}

func TestTavilyDomainMappingExclude(t *testing.T) {
	var gotBody tavilyAPIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		json.NewEncoder(w).Encode(tavilyAPIResponse{Query: "x"})
	}))
	defer srv.Close()
	p := newTestTavily(srv.URL, 0)
	_, err := p.Search(context.Background(), SearchRequest{
		Query:          "x",
		MaxResults:     5,
		BlockedDomains: []string{"spam.example"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotBody.ExcludeDomains) != 1 || gotBody.ExcludeDomains[0] != "spam.example" {
		t.Errorf("expected exclude_domains=[spam.example], got %v", gotBody.ExcludeDomains)
	}
	if len(gotBody.IncludeDomains) != 0 {
		t.Errorf("expected no include_domains, got %v", gotBody.IncludeDomains)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("5"); !ok || d != 5*time.Second {
		t.Errorf("expected 5s, got %v ok=%v", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Errorf("empty must not parse")
	}
	if _, ok := parseRetryAfter("garbage"); ok {
		t.Errorf("garbage must not parse")
	}
}
