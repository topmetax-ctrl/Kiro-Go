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

// newTestMem0 builds a provider pointed at a test server with fixed policy, so
// tests don't depend on global config state. redactSecrets/storeSourceCode are
// fixed closures; the API key is a constant.
func newTestMem0(baseURL string, redactSecrets, storeSourceCode bool) *Mem0HTTPProvider {
	return &Mem0HTTPProvider{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          func() string { return "mem0-test-key-SECRET" },
		client:          func() *http.Client { return &http.Client{Timeout: 5 * time.Second} },
		searchTimeout:   2 * time.Second,
		writeTimeout:    5 * time.Second,
		retrievalLimit:  8,
		redactSecrets:   func() bool { return redactSecrets },
		storeSourceCode: func() bool { return storeSourceCode },
		health:          newHealthTracker(3, 30*time.Second),
	}
}

func TestMem0AddHappyPath(t *testing.T) {
	var gotPath, gotKey string
	var gotBody mem0AddRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"id":"m1","event":"ADD"}]}`))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	err := p.Add(context.Background(), AddMemoryInput{
		Scope:    MemoryScope{Principal: "apikey-uuid-1"},
		Messages: []MemoryMessage{{Role: "user", Content: "we use Postgres for the ledger"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/memories" {
		t.Errorf("expected POST /memories, got %q", gotPath)
	}
	if gotKey != "mem0-test-key-SECRET" {
		t.Errorf("expected X-Api-Key header, got %q", gotKey)
	}
	if gotBody.UserID != "apikey-uuid-1" {
		t.Errorf("expected user_id=apikey-uuid-1, got %q", gotBody.UserID)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "we use Postgres for the ledger" {
		t.Errorf("unexpected messages: %+v", gotBody.Messages)
	}
}

func TestMem0AddRequiresPrincipal(t *testing.T) {
	p := newTestMem0("http://unused.example", true, false)
	err := p.Add(context.Background(), AddMemoryInput{
		Messages: []MemoryMessage{{Role: "user", Content: "x"}},
	})
	var cfgErr *MemoryConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected MemoryConfigError for empty principal, got %v", err)
	}
}

func TestMem0AddRedactsSecrets(t *testing.T) {
	var gotBody mem0AddRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	err := p.Add(context.Background(), AddMemoryInput{
		Scope:    MemoryScope{Principal: "u1"},
		Messages: []MemoryMessage{{Role: "user", Content: "the key is sk-abcdef123456 keep it"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotBody.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(gotBody.Messages))
	}
	if strings.Contains(gotBody.Messages[0].Content, "sk-abcdef123456") {
		t.Errorf("secret leaked into stored memory: %q", gotBody.Messages[0].Content)
	}
	if !strings.Contains(gotBody.Messages[0].Content, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker, got %q", gotBody.Messages[0].Content)
	}
}

func TestMem0AddDropsSourceDumpWhenDisallowed(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// A single message that is a large fenced code block; with storeSourceCode
	// false it is dropped, leaving nothing to write, so no HTTP call is made.
	big := "```go\n" + strings.Repeat("x := doWork()\n", 40) + "```"
	p := newTestMem0(srv.URL, true, false)
	err := p.Add(context.Background(), AddMemoryInput{
		Scope:    MemoryScope{Principal: "u1"},
		Messages: []MemoryMessage{{Role: "user", Content: big}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Errorf("expected no backend call when all messages dropped by redaction")
	}
}

func TestMem0AddKeepsSourceWhenAllowed(t *testing.T) {
	var gotBody mem0AddRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	big := "```go\n" + strings.Repeat("x := doWork()\n", 40) + "```"
	p := newTestMem0(srv.URL, true, true) // storeSourceCode=true
	err := p.Add(context.Background(), AddMemoryInput{
		Scope:    MemoryScope{Principal: "u1"},
		Messages: []MemoryMessage{{Role: "user", Content: big}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotBody.Messages) != 1 {
		t.Errorf("expected source kept when storeSourceCode=true, got %d messages", len(gotBody.Messages))
	}
}

func TestMem0SearchParsesResultsArray(t *testing.T) {
	var gotBody mem0SearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("expected /search, got %q", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"results":[{"id":"m1","memory":"uses Postgres","score":0.8}]}`))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	mems, err := p.Search(context.Background(), SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "database", Limit: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody.UserID != "u1" || gotBody.Query != "database" || gotBody.Limit != 5 {
		t.Errorf("unexpected search body: %+v", gotBody)
	}
	if len(mems) != 1 || mems[0].ID != "m1" || mems[0].Text != "uses Postgres" {
		t.Errorf("unexpected memories: %+v", mems)
	}
}

func TestMem0SearchParsesBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"m2","memory":"prefers tabs","score":0.5}]`))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	mems, err := p.Search(context.Background(), SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "style"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mems) != 1 || mems[0].ID != "m2" {
		t.Errorf("expected bare-array parse, got %+v", mems)
	}
}

func TestMem0SearchFallsBackToConfiguredLimit(t *testing.T) {
	var gotBody mem0SearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false) // retrievalLimit=8
	_, err := p.Search(context.Background(), SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody.Limit != 8 {
		t.Errorf("expected fallback limit 8, got %d", gotBody.Limit)
	}
}

func TestMem0SearchMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	_, err := p.Search(context.Background(), SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "x"})
	var provErr *MemoryProviderError
	if !errors.As(err, &provErr) || provErr.Kind != MemoryErrMalformed {
		t.Fatalf("expected malformed provider error, got %v", err)
	}
}

func TestMem0StatusClassification(t *testing.T) {
	cases := []struct {
		status int
		kind   MemoryProviderErrorKind
		retry  bool
	}{
		{400, MemoryErrInvalid, false},
		{401, MemoryErrAuth, false},
		{403, MemoryErrAuth, false},
		{429, MemoryErrRateLimit, true},
		{500, MemoryErrUpstream5xx, true},
		{503, MemoryErrUpstream5xx, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte("error body"))
		}))
		p := newTestMem0(srv.URL, true, false)
		_, err := p.Search(context.Background(), SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "x"})
		srv.Close()
		var provErr *MemoryProviderError
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

func TestMem0KeyNeverInError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte("unauthorized"))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	_, err := p.Search(context.Background(), SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "mem0-test-key-SECRET") {
		t.Errorf("API key leaked into error: %v", err)
	}
}

func TestMem0ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := p.Search(ctx, SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// A cancelled call must not count as a health failure.
	if p.healthState().State == ProviderUnhealthy {
		t.Errorf("context cancellation must not trip the circuit")
	}
}

func TestMem0DeleteScoped(t *testing.T) {
	var gotMethod, gotUserID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotUserID = r.URL.Query().Get("user_id")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	err := p.Delete(context.Background(), MemoryScope{Principal: "u1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "DELETE" || gotUserID != "u1" {
		t.Errorf("expected DELETE with user_id=u1, got %s user_id=%q", gotMethod, gotUserID)
	}
}

func TestMem0HealthUsesOpenAPI(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/openapi.json" {
		t.Errorf("expected health probe on /openapi.json, got %q", gotPath)
	}
}

func TestMem0CircuitTripsAfterConsecutiveFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	p := newTestMem0(srv.URL, true, false)
	for i := 0; i < 3; i++ {
		_, _ = p.Search(context.Background(), SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "x"})
	}
	if p.healthState().State != ProviderUnhealthy {
		t.Errorf("expected circuit to trip unhealthy after 3 failures, got %v", p.healthState().State)
	}
}

func TestNewMem0HTTPProviderRejectsBadURL(t *testing.T) {
	if _, err := NewMem0HTTPProvider(""); err == nil {
		t.Error("expected error for empty base URL")
	}
	if _, err := NewMem0HTTPProvider("not-a-url"); err == nil {
		t.Error("expected error for non-absolute URL")
	}
	if _, err := NewMem0HTTPProvider("ftp://host/x"); err == nil {
		t.Error("expected error for non-http scheme")
	}
}
