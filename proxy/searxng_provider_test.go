package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newSearXNGTestProvider points a provider at an httptest.Server (no SSRF: the
// base URL is fixed at construction, exactly as production does from config).
func newSearXNGTestProvider(t *testing.T, srv *httptest.Server) *SearXNGProvider {
	t.Helper()
	p, err := NewSearXNGProvider(srv.URL)
	if err != nil {
		t.Fatalf("construct searxng provider: %v", err)
	}
	return p
}

func TestSearXNGBuildsQueryParams(t *testing.T) {
	var gotQuery, gotFormat, gotSafe, gotLang, gotCats, gotTime string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotQuery = q.Get("q")
		gotFormat = q.Get("format")
		gotSafe = q.Get("safesearch")
		gotLang = q.Get("language")
		gotCats = q.Get("categories")
		gotTime = q.Get("time_range")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"x","results":[]}`))
	}))
	defer srv.Close()

	p := newSearXNGTestProvider(t, srv)
	_, _ = p.Search(context.Background(), SearchRequest{
		Query:      "go release",
		Language:   "en",
		SafeSearch: 2,
		Categories: []string{"general", "news"},
		TimeRange:  "day",
	})

	if gotQuery != "go release" {
		t.Errorf("q=%q", gotQuery)
	}
	if gotFormat != "json" {
		t.Errorf("format=%q", gotFormat)
	}
	if gotSafe != "2" {
		t.Errorf("safesearch=%q", gotSafe)
	}
	if gotLang != "en" {
		t.Errorf("language=%q", gotLang)
	}
	if gotCats != "general,news" {
		t.Errorf("categories=%q", gotCats)
	}
	if gotTime != "day" {
		t.Errorf("time_range=%q", gotTime)
	}
}

func TestSearXNGParsesResults(t *testing.T) {
	body := `{
	  "query": "go",
	  "results": [
	    {"url":"https://go.dev/dl/","title":"Downloads","content":"Go 1.26","engine":"google","score":1.5,"publishedDate":"2026-02-10T00:00:00"},
	    {"url":"https://go.dev/blog","title":"Blog","content":"news","engine":"bing","score":1.0,"publishedDate":null}
	  ],
	  "answers": ["Go 1.26 is latest"]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newSearXNGTestProvider(t, srv)
	resp, err := p.Search(context.Background(), SearchRequest{Query: "go"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Provider != "searxng" {
		t.Errorf("provider=%q", resp.Provider)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].URL != "https://go.dev/dl/" || resp.Results[0].Engine != "google" {
		t.Errorf("result0 wrong: %+v", resp.Results[0])
	}
	if resp.Results[0].PublishedDate != "2026-02-10T00:00:00" {
		t.Errorf("publishedDate not parsed: %q", resp.Results[0].PublishedDate)
	}
	if resp.Results[1].PublishedDate != "" {
		t.Errorf("null publishedDate should be empty, got %q", resp.Results[1].PublishedDate)
	}
	if resp.Answer != "Go 1.26 is latest" {
		t.Errorf("answer=%q", resp.Answer)
	}
}

func TestSearXNGEmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"nothing","results":[]}`))
	}))
	defer srv.Close()
	p := newSearXNGTestProvider(t, srv)
	resp, err := p.Search(context.Background(), SearchRequest{Query: "nothing"})
	if err != nil {
		t.Fatalf("empty results must not error: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("expected 0 results")
	}
}

func TestSearXNG403SignalsJSONDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Forbidden"))
	}))
	defer srv.Close()
	p := newSearXNGTestProvider(t, srv)
	_, err := p.Search(context.Background(), SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "settings.yml") {
		t.Errorf("403 error should hint at JSON format config, got: %v", err)
	}
}

func TestSearXNGMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	p := newSearXNGTestProvider(t, srv)
	_, err := p.Search(context.Background(), SearchRequest{Query: "x"})
	var provErr *SearchProviderError
	if !asProviderError(err, &provErr) || provErr.Kind != SearchErrMalformed {
		t.Fatalf("expected malformed error, got %v", err)
	}
}

func TestSearXNG5xxIsRetryableKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	p := newSearXNGTestProvider(t, srv)
	_, err := p.Search(context.Background(), SearchRequest{Query: "x"})
	var provErr *SearchProviderError
	if !asProviderError(err, &provErr) || provErr.Kind != SearchErrUpstream5xx {
		t.Fatalf("expected upstream_5xx, got %v", err)
	}
}

func TestSearXNGContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	p := newSearXNGTestProvider(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Search(ctx, SearchRequest{Query: "slow"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSearXNGRejectsBadBaseURL(t *testing.T) {
	if _, err := NewSearXNGProvider(""); err == nil {
		t.Error("empty base URL must error")
	}
	if _, err := NewSearXNGProvider("ftp://searxng"); err == nil {
		t.Error("non-http(s) base URL must error")
	}
	if _, err := NewSearXNGProvider("not a url"); err == nil {
		t.Error("malformed base URL must error")
	}
}
