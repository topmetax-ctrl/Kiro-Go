package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultTavilyEndpoint is Tavily's public search endpoint. The provider takes
// the endpoint as a constructor arg so tests can point at an httptest.Server;
// production config does not expose it (no SSRF surface).
const defaultTavilyEndpoint = "https://api.tavily.com/search"

// tavilyMaxResponseBytes caps the response body we read, guarding against a
// misbehaving or hostile upstream.
const tavilyMaxResponseBytes = 1 << 20

// TavilyProvider implements SearchProvider against the Tavily Search API.
type TavilyProvider struct {
	endpoint string
	apiKey   func() string // indirection so key rotation/env override is picked up per call
	client   func() *http.Client
	// retry knobs, resolved from config at construction.
	retryMax    int
	retryBaseMs int
	health      *healthTracker
}

// tavilyAPIRequest is the wire payload. api_key travels in the Authorization
// header (Bearer), not the body — verified against Tavily's current API spec.
// include_usage requests the usage.credits object so we can track free-tier spend.
type tavilyAPIRequest struct {
	Query             string   `json:"query"`
	SearchDepth       string   `json:"search_depth,omitempty"`
	MaxResults        int      `json:"max_results"`
	IncludeAnswer     bool     `json:"include_answer"`
	IncludeRawContent bool     `json:"include_raw_content"`
	IncludeImages     bool     `json:"include_images"`
	AutoParameters    bool     `json:"auto_parameters"`
	IncludeUsage      bool     `json:"include_usage"`
	IncludeDomains    []string `json:"include_domains,omitempty"`
	ExcludeDomains    []string `json:"exclude_domains,omitempty"`
}

type tavilyAPIResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Content       string  `json:"content"`
	Score         float64 `json:"score"`
	PublishedDate string  `json:"published_date"`
}

// tavilyUsage carries the credit cost of a call when include_usage is set. There
// is no per-response "remaining balance" field, only the credits spent here.
type tavilyUsage struct {
	Credits int `json:"credits"`
}

type tavilyAPIResponse struct {
	Query        string            `json:"query"`
	Answer       string            `json:"answer"`
	Results      []tavilyAPIResult `json:"results"`
	ResponseTime float64           `json:"response_time"`
	RequestID    string            `json:"request_id"`
	Usage        *tavilyUsage      `json:"usage"`
}

// NewTavilyProvider builds a provider. endpoint may be "" to use the default.
func NewTavilyProvider(endpoint string) *TavilyProvider {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultTavilyEndpoint
	}
	ws := config.GetWebSearchConfig()
	return &TavilyProvider{
		endpoint:    endpoint,
		apiKey:      config.TavilyAPIKeyResolved,
		client:      func() *http.Client { return GetRestClientForProxy(config.GetProxyURL()) },
		retryMax:    ws.Tavily.RetryMax,
		retryBaseMs: ws.Tavily.RetryBaseDelayMs,
		health:      newHealthTracker(3, 30*time.Second),
	}
}

func (p *TavilyProvider) Name() string { return "tavily" }

// Health reports the provider's circuit state.
func (p *TavilyProvider) Health() ProviderHealth {
	if p.health == nil {
		return ProviderHealth{State: ProviderHealthy}
	}
	return p.health.health()
}

// Search executes one Tavily query with bounded, context-cancelable retry.
func (p *TavilyProvider) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	apiKey := strings.TrimSpace(p.apiKey())
	if apiKey == "" {
		return SearchResponse{}, &SearchConfigError{Reason: "tavily api key not configured"}
	}

	attempts := p.retryMax + 1
	if attempts < 1 {
		attempts = 1
	}
	started := time.Now()
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return SearchResponse{}, err
		}
		resp, err := p.doSearch(ctx, apiKey, req)
		if err == nil {
			p.observe(nil, time.Since(started))
			return resp, nil
		}
		lastErr = err
		var provErr *SearchProviderError
		if !errors.As(err, &provErr) || !provErr.Kind.retryable() {
			p.observe(err, time.Since(started))
			return SearchResponse{}, err
		}
		// Retryable: back off (honoring Retry-After) unless this was the last attempt.
		if attempt == attempts-1 {
			break
		}
		delay := p.backoff(attempt, provErr)
		select {
		case <-ctx.Done():
			return SearchResponse{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	p.observe(lastErr, time.Since(started))
	return SearchResponse{}, lastErr
}

// observe feeds the health tracker. Context cancellation is not a provider fault,
// so it is not recorded as a failure.
func (p *TavilyProvider) observe(err error, latency time.Duration) {
	if p.health == nil {
		return
	}
	if err == nil {
		p.health.observeSuccess(latency)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	p.health.observeFailure(latency, err.Error())
}

// doSearch performs a single HTTP attempt and classifies the outcome.
func (p *TavilyProvider) doSearch(ctx context.Context, apiKey string, req SearchRequest) (SearchResponse, error) {
	depth := req.SearchDepth
	if depth == "" {
		depth = "basic"
	}
	body, _ := json.Marshal(tavilyAPIRequest{
		Query:          req.Query,
		SearchDepth:    depth,
		MaxResults:     req.MaxResults,
		IncludeAnswer:  req.IncludeAnswer,
		AutoParameters: false,
		IncludeUsage:   true,
		IncludeDomains: req.AllowedDomains,
		ExcludeDomains: req.BlockedDomains,
	})

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint, bytes.NewReader(body))
	if err != nil {
		return SearchResponse{}, &SearchProviderError{Kind: SearchErrInvalid, Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.client().Do(httpReq)
	if err != nil {
		// Context cancellation surfaces as-is (caller distinguishes it); other
		// transport errors are treated as transient timeouts.
		if ctx.Err() != nil {
			return SearchResponse{}, ctx.Err()
		}
		return SearchResponse{}, &SearchProviderError{Kind: SearchErrTimeout, Err: err}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, tavilyMaxResponseBytes))

	if resp.StatusCode != 200 {
		return SearchResponse{}, classifyTavilyStatus(resp, respBody)
	}

	var tr tavilyAPIResponse
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return SearchResponse{}, &SearchProviderError{Kind: SearchErrMalformed, StatusCode: 200, Err: err}
	}

	out := SearchResponse{Query: req.Query, Answer: tr.Answer, Provider: "tavily"}
	if tr.Usage != nil && tr.Usage.Credits > 0 {
		out.Credits = tr.Usage.Credits
	}
	for _, r := range tr.Results {
		out.Results = append(out.Results, SearchResult{
			Title:         r.Title,
			URL:           r.URL,
			Content:       r.Content,
			Score:         r.Score,
			PublishedDate: r.PublishedDate,
		})
	}

	logger.Debugf("[WebSearch] tavily ok: query_len=%d results=%d credits=%d latency=%.2fs", len(req.Query), len(out.Results), out.Credits, tr.ResponseTime)
	return out, nil
}

// classifyTavilyStatus maps a non-200 status to a typed provider error.
func classifyTavilyStatus(resp *http.Response, body []byte) *SearchProviderError {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	e := &SearchProviderError{StatusCode: resp.StatusCode, Err: fmt.Errorf("tavily http %d: %s", resp.StatusCode, msg)}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		e.Kind = SearchErrAuth
	case resp.StatusCode == 429:
		e.Kind = SearchErrRateLimit
	case resp.StatusCode >= 500:
		e.Kind = SearchErrUpstream5xx
	default:
		e.Kind = SearchErrInvalid
	}
	if ra, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
		e.RetryAfter = ra
	}
	return e
}

// backoff returns the delay before the next retry: Retry-After if the provider
// sent one, else exponential base*2^attempt with jitter.
func (p *TavilyProvider) backoff(attempt int, provErr *SearchProviderError) time.Duration {
	// A provider Retry-After hint wins over computed backoff.
	if provErr != nil && provErr.RetryAfter > 0 {
		return provErr.RetryAfter
	}
	base := p.retryBaseMs
	if base <= 0 {
		base = 300
	}
	// exponential
	d := time.Duration(base) * time.Millisecond * (1 << attempt)
	// jitter: +/- 20%
	jitter := time.Duration(rand.Int63n(int64(d/5) + 1))
	if rand.Intn(2) == 0 {
		d += jitter
	} else {
		d -= jitter
	}
	if d < 0 {
		d = time.Duration(base) * time.Millisecond
	}
	return d
}

// parseRetryAfter reads a Retry-After header (seconds form) if present.
func parseRetryAfter(h string) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	return 0, false
}
