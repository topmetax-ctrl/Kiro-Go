package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"kiro-go/logger"
)

// searxngMaxResponseBytes caps the response body read from SearXNG, guarding
// against a misbehaving instance.
const searxngMaxResponseBytes = 2 << 20

// SearXNGProvider implements Provider against a self-hosted SearXNG JSON
// search API. It is the free, primary discovery provider.
//
// The base URL is fixed at construction from server config and validated once;
// it is NEVER derived from a request (SSRF guard). SearXNG's JSON format must be
// enabled in the instance's settings.yml (search.formats includes "json"),
// otherwise it returns 403.
type SearXNGProvider struct {
	baseURL string
	client  func() *http.Client
	health  *healthTracker
}

// searxngResult mirrors one item of SearXNG's results[]. Only the fields we use
// are declared; unknown fields are ignored. publishedDate is camelCase in the
// current source (NOT published_date) and may be null.
type searxngResult struct {
	URL           string  `json:"url"`
	Title         string  `json:"title"`
	Content       string  `json:"content"`
	Engine        string  `json:"engine"`
	Score         float64 `json:"score"`
	PublishedDate *string `json:"publishedDate"`
	Category      string  `json:"category"`
}

// searxngResponse mirrors the top-level JSON shape. answers is a heterogeneous
// list in SearXNG; we only surface it opportunistically as a provider answer.
type searxngResponse struct {
	Query               string          `json:"query"`
	Results             []searxngResult `json:"results"`
	Answers             []interface{}   `json:"answers"`
	UnresponsiveEngines []interface{}   `json:"unresponsive_engines"`
}

// NewSearXNGProvider builds a provider bound to the configured base URL. It
// returns an error if the base URL is missing or not an http(s) URL, so the
// caller can decide whether SearXNG is eligible at wiring time. httpClient is
// the seam through which the proxy injects its rest client (proxy-aware); it
// keeps this package free of any dependency on the proxy transport layer.
func NewSearXNGProvider(baseURL string, httpClient func() *http.Client) (*SearXNGProvider, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, &ConfigError{Reason: "searxng base URL not configured"}
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &ConfigError{Reason: "searxng base URL must be an absolute http(s) URL"}
	}
	if httpClient == nil {
		httpClient = func() *http.Client { return http.DefaultClient }
	}
	return &SearXNGProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  httpClient,
		health:  newHealthTracker(3, 30*time.Second),
	}, nil
}

func (p *SearXNGProvider) Name() string { return "searxng" }

// Health reports the provider's circuit state.
func (p *SearXNGProvider) Health() ProviderHealth {
	if p.health == nil {
		return ProviderHealth{State: ProviderHealthy}
	}
	return p.health.health()
}

// Search executes one SearXNG query. SearXNG has no documented rate-limit
// retry contract for the JSON API (the limiter is off by default on a private
// instance), so a single attempt is made; transient failures surface as typed
// errors and the router decides fallback.
func (p *SearXNGProvider) Search(ctx context.Context, req Request) (Response, error) {
	started := time.Now()
	resp, err := p.doSearch(ctx, req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Response{}, err
		}
		if p.health != nil {
			p.health.observeFailure(time.Since(started), err.Error())
		}
		return Response{}, err
	}
	if p.health != nil {
		p.health.observeSuccess(time.Since(started))
	}
	return resp, nil
}

func (p *SearXNGProvider) doSearch(ctx context.Context, req Request) (Response, error) {
	q := url.Values{}
	q.Set("q", req.Query)
	q.Set("format", "json")
	if lang := strings.TrimSpace(req.Language); lang != "" && lang != "auto" {
		q.Set("language", lang)
	}
	// safesearch: 0/1/2. Always set explicitly so instance defaults don't surprise.
	q.Set("safesearch", strconv.Itoa(clampSafeSearch(req.SafeSearch)))
	if len(req.Categories) > 0 {
		q.Set("categories", strings.Join(req.Categories, ","))
	}
	if tr := normalizeTimeRange(req.TimeRange); tr != "" {
		q.Set("time_range", tr)
	}

	endpoint := p.baseURL + "/search?" + q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return Response{}, &ProviderError{Kind: ErrInvalid, Err: err}
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client().Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, &ProviderError{Kind: ErrTimeout, Err: err}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, searxngMaxResponseBytes))

	if resp.StatusCode != 200 {
		return Response{}, classifySearXNGStatus(resp.StatusCode, respBody)
	}

	var sr searxngResponse
	if err := json.Unmarshal(respBody, &sr); err != nil {
		return Response{}, &ProviderError{Kind: ErrMalformed, StatusCode: 200, Err: err}
	}

	out := Response{Query: req.Query, Provider: "searxng"}
	for _, r := range sr.Results {
		published := ""
		if r.PublishedDate != nil {
			published = strings.TrimSpace(*r.PublishedDate)
		}
		out.Results = append(out.Results, Result{
			Title:         strings.TrimSpace(r.Title),
			URL:           strings.TrimSpace(r.URL),
			Content:       strings.TrimSpace(r.Content),
			Score:         r.Score,
			PublishedDate: published,
			Engine:        strings.TrimSpace(r.Engine),
		})
	}
	// Opportunistically surface a plain-string answer if SearXNG returned one.
	for _, a := range sr.Answers {
		if s, ok := a.(string); ok && strings.TrimSpace(s) != "" {
			out.Answer = strings.TrimSpace(s)
			break
		}
	}

	logger.Debugf("[WebSearch] searxng ok: query_len=%d results=%d", len(req.Query), len(out.Results))
	return out, nil
}

// classifySearXNGStatus maps a non-200 status to a typed provider error. A 403
// almost always means the JSON format is not enabled in settings.yml — call it
// out explicitly so operators can fix the instance rather than chase a generic
// error.
func classifySearXNGStatus(status int, body []byte) *ProviderError {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	e := &ProviderError{StatusCode: status, Err: fmt.Errorf("searxng http %d: %s", status, msg)}
	switch {
	case status == 403:
		e.Kind = ErrAuth
		e.Err = fmt.Errorf("searxng http 403 (is JSON format enabled in settings.yml search.formats?): %s", msg)
	case status == 429:
		e.Kind = ErrRateLimit
	case status >= 500:
		e.Kind = ErrUpstream5xx
	default:
		e.Kind = ErrInvalid
	}
	return e
}

// clampSafeSearch bounds a safesearch level to SearXNG's accepted 0/1/2.
func clampSafeSearch(v int) int {
	if v < 0 {
		return 0
	}
	if v > 2 {
		return 2
	}
	return v
}

// normalizeTimeRange maps a freshness hint to SearXNG's accepted time_range
// values (day/month/year; SearXNG has no "week"). Returns "" for no restriction.
func normalizeTimeRange(tr string) string {
	switch strings.ToLower(strings.TrimSpace(tr)) {
	case "day", "d":
		return "day"
	case "week", "w", "month", "m":
		// SearXNG accepts day/month/year; collapse week onto month.
		return "month"
	case "year", "y":
		return "year"
	default:
		return ""
	}
}
