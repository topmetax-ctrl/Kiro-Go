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
	"net/http"
	"net/url"
	"strings"
	"time"
)

// mem0MaxResponseBytes caps the response body we read from the Mem0 backend,
// guarding against a misbehaving or hostile upstream.
const mem0MaxResponseBytes = 1 << 20

// Mem0HTTPProvider implements MemoryProvider against a self-hosted Mem0 OSS
// server (REST API, no Python SDK). It is a pure adapter: it returns typed
// *MemoryProviderError on failure and never swallows errors — fail-open policy
// is composed on top by the factory (see memory_factory.go) so this type stays
// honest and testable.
type Mem0HTTPProvider struct {
	baseURL         string
	apiKey          func() string      // indirection so key rotation is picked up per call
	client          func() *http.Client
	searchTimeout   time.Duration
	writeTimeout    time.Duration
	retrievalLimit  int
	redactSecrets   func() bool
	storeSourceCode func() bool
	health          *healthTracker
}

// NewMem0HTTPProvider builds a provider bound to the configured base URL. It
// returns an error if the base URL is missing or not an absolute http(s) URL,
// so the caller can decide eligibility at wiring time (mirrors NewSearXNGProvider).
func NewMem0HTTPProvider(baseURL string) (*Mem0HTTPProvider, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, &MemoryConfigError{Reason: "mem0 base URL not configured"}
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &MemoryConfigError{Reason: "mem0 base URL must be an absolute http(s) URL"}
	}
	m := config.GetMemoryConfig()
	return &Mem0HTTPProvider{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          func() string { return strings.TrimSpace(config.GetMemoryConfig().APIKey) },
		client:          func() *http.Client { return GetRestClientForProxy(config.GetProxyURL()) },
		searchTimeout:   time.Duration(m.Timeouts.SearchMs) * time.Millisecond,
		writeTimeout:    time.Duration(m.Timeouts.WriteMs) * time.Millisecond,
		retrievalLimit:  m.RetrievalLimit,
		redactSecrets:   config.MemoryRedactSecrets,
		storeSourceCode: func() bool { return config.GetMemoryConfig().Redaction.StoreSourceCode },
		health:          newHealthTracker(3, 30*time.Second),
	}, nil
}

func (p *Mem0HTTPProvider) Name() string { return "mem0" }

// --- wire types (Mem0 OSS REST API) ---

type mem0Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type mem0AddRequest struct {
	Messages []mem0Message `json:"messages"`
	UserID   string        `json:"user_id"`
}

type mem0SearchRequest struct {
	Query  string `json:"query"`
	UserID string `json:"user_id"`
	Limit  int    `json:"limit,omitempty"`
}

// mem0SearchResult is one memory in a search response. Mem0 returns either a
// bare array or an object with a "results" array depending on version; we decode
// both (see decodeSearchResults).
type mem0SearchResult struct {
	ID       string         `json:"id"`
	Memory   string         `json:"memory"`
	Score    float64        `json:"score"`
	Metadata map[string]any `json:"metadata"`
}

// Add persists a memory scoped to the caller principal. Redaction runs here so
// it cannot be bypassed by any caller, including automatic write mode. If every
// message is dropped by redaction, the write is skipped (no error).
func (p *Mem0HTTPProvider) Add(ctx context.Context, in AddMemoryInput) error {
	if strings.TrimSpace(in.Scope.Principal) == "" {
		return &MemoryConfigError{Reason: "memory add requires a non-empty scope principal"}
	}
	safe := applyMemoryRedaction(in.Messages, p.redactSecrets(), p.storeSourceCode())
	if len(safe) == 0 {
		logger.Debugf("[Memory] add skipped: all messages dropped by redaction (principal_len=%d)", len(in.Scope.Principal))
		return nil
	}
	wire := mem0AddRequest{UserID: in.Scope.Principal}
	for _, m := range safe {
		wire.Messages = append(wire.Messages, mem0Message{Role: m.Role, Content: m.Content})
	}
	body, _ := json.Marshal(wire)

	callCtx, cancel := p.withTimeout(ctx, p.writeTimeout)
	defer cancel()
	started := time.Now()
	_, err := p.do(callCtx, "POST", "/memories", body)
	p.observe(err, time.Since(started))
	if err != nil {
		return err
	}
	logger.Debugf("[Memory] add ok: messages=%d latency=%s", len(safe), time.Since(started))
	return nil
}

// Search retrieves memories scoped to the caller principal, most relevant first.
func (p *Mem0HTTPProvider) Search(ctx context.Context, q SearchQuery) ([]Memory, error) {
	if strings.TrimSpace(q.Scope.Principal) == "" {
		return nil, &MemoryConfigError{Reason: "memory search requires a non-empty scope principal"}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = p.retrievalLimit
	}
	body, _ := json.Marshal(mem0SearchRequest{Query: q.Query, UserID: q.Scope.Principal, Limit: limit})

	callCtx, cancel := p.withTimeout(ctx, p.searchTimeout)
	defer cancel()
	started := time.Now()
	respBody, err := p.do(callCtx, "POST", "/search", body)
	p.observe(err, time.Since(started))
	if err != nil {
		return nil, err
	}
	results, err := decodeSearchResults(respBody)
	if err != nil {
		return nil, &MemoryProviderError{Kind: MemoryErrMalformed, StatusCode: 200, Err: err}
	}
	out := make([]Memory, 0, len(results))
	for _, r := range results {
		out = append(out, Memory{ID: r.ID, Text: r.Memory, Score: r.Score, Metadata: r.Metadata})
	}
	logger.Debugf("[Memory] search ok: query_len=%d results=%d latency=%s", len(q.Query), len(out), time.Since(started))
	return out, nil
}

// Delete removes all memories scoped to the caller principal. Unlike Search/Add
// this is not fail-open at the factory layer: a silent delete failure would
// mislead an operator into thinking data was purged.
func (p *Mem0HTTPProvider) Delete(ctx context.Context, scope MemoryScope) error {
	if strings.TrimSpace(scope.Principal) == "" {
		return &MemoryConfigError{Reason: "memory delete requires a non-empty scope principal"}
	}
	callCtx, cancel := p.withTimeout(ctx, p.writeTimeout)
	defer cancel()
	path := "/memories?user_id=" + url.QueryEscape(scope.Principal)
	started := time.Now()
	_, err := p.do(callCtx, "DELETE", path, nil)
	p.observe(err, time.Since(started))
	return err
}

// Health probes the backend via its open OpenAPI endpoint (no auth required),
// so a healthy check does not depend on a valid API key.
func (p *Mem0HTTPProvider) Health(ctx context.Context) error {
	callCtx, cancel := p.withTimeout(ctx, p.searchTimeout)
	defer cancel()
	_, err := p.do(callCtx, "GET", "/openapi.json", nil)
	return err
}

// withTimeout derives a per-call context bounded by d (if d > 0), so a slow
// backend cannot hang the request beyond the configured budget.
func (p *Mem0HTTPProvider) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// do performs a single HTTP call to the Mem0 backend and classifies the outcome.
// method is GET/POST/DELETE; path is the API path beginning with "/".
func (p *Mem0HTTPProvider) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return nil, &MemoryProviderError{Kind: MemoryErrInvalid, Err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if key := p.apiKey(); key != "" {
		req.Header.Set("X-Api-Key", key)
	}

	resp, err := p.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &MemoryProviderError{Kind: MemoryErrTimeout, Err: err}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, mem0MaxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyMem0Status(resp, respBody)
	}
	return respBody, nil
}

// observe feeds the health tracker. Context cancellation is not a backend fault,
// so it is not recorded as a failure (mirrors the Tavily provider).
func (p *Mem0HTTPProvider) observe(err error, latency time.Duration) {
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

// healthState exposes the circuit state for the factory's fail-open decorator
// and observability. Not part of MemoryProvider (which uses a live Health probe).
func (p *Mem0HTTPProvider) healthState() ProviderHealth {
	if p.health == nil {
		return ProviderHealth{State: ProviderHealthy}
	}
	return p.health.health()
}

// classifyMem0Status maps a non-2xx status to a typed provider error.
func classifyMem0Status(resp *http.Response, body []byte) *MemoryProviderError {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	e := &MemoryProviderError{StatusCode: resp.StatusCode, Err: fmt.Errorf("mem0 http %d: %s", resp.StatusCode, msg)}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		e.Kind = MemoryErrAuth
	case resp.StatusCode == 429:
		e.Kind = MemoryErrRateLimit
	case resp.StatusCode >= 500:
		e.Kind = MemoryErrUpstream5xx
	default:
		e.Kind = MemoryErrInvalid
	}
	if ra, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
		e.RetryAfter = ra
	}
	return e
}

// decodeSearchResults tolerates both Mem0 response shapes: a bare array of
// results, or an object of the form {"results": [...]}.
func decodeSearchResults(body []byte) ([]mem0SearchResult, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []mem0SearchResult
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var obj struct {
		Results []mem0SearchResult `json:"results"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, err
	}
	return obj.Results, nil
}
