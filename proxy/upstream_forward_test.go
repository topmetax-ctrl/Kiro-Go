package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

func setupForwardRoute(t *testing.T, upstreamURL, clientModel, targetModel string) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	up := config.UpstreamProvider{
		ID: "up-1", Name: "test-upstream", BaseURL: upstreamURL, ApiKey: "upstream-secret", Enabled: true,
	}
	route := config.ModelRoute{
		ID: "route-1", Model: clientModel, UpstreamID: up.ID, TargetModel: targetModel, Enabled: true,
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{up}, []config.ModelRoute{route}); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
}

func forwardRequest(t *testing.T, apiKeyID, clientModel string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+clientModel+`","messages":[]}`))
	if apiKeyID != "" {
		r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, apiKeyID))
	}
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(`{"model":"`+clientModel+`","messages":[]}`), clientModel, false, "/chat/completions", false, "") {
		t.Fatal("expected route to match and forward")
	}
	return rec
}

func TestForwardAccountsUsageForWithinLimitKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert the upstream credential (not the client's) is sent.
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("expected upstream credential, got %q", got)
		}
		w.Header().Set("X-Usage-Input-Tokens", "12")
		w.Header().Set("X-Usage-Output-Tokens", "34")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	setupForwardRoute(t, upstream.URL, "gpt-forward", "")
	key, err := config.AddApiKey(config.ApiKeyEntry{Name: "k", Key: "sk-x", Enabled: true, TokenLimit: 1000})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	rec := forwardRequest(t, key.ID, "gpt-forward")
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// The forwarded response reported 12+34 tokens; the key's usage must advance.
	got := config.FindApiKeyByValue("sk-x")
	if got == nil {
		t.Fatal("key vanished")
	}
	if got.TokensUsed != 46 {
		t.Fatalf("expected 46 tokens accounted (12+34), got %d", got.TokensUsed)
	}
}

func TestForwardChargesOneUnitWhenUpstreamOmitsUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	setupForwardRoute(t, upstream.URL, "gpt-forward", "")
	key, _ := config.AddApiKey(config.ApiKeyEntry{Name: "k", Key: "sk-x", Enabled: true, TokenLimit: 1000})

	rec := forwardRequest(t, key.ID, "gpt-forward")
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got := config.FindApiKeyByValue("sk-x")
	if got.TokensUsed != 1 {
		t.Fatalf("expected 1 request-unit charge when upstream omits usage, got %d", got.TokensUsed)
	}
}

func TestForwardRedactsUpstreamError(t *testing.T) {
	// No upstream server: dial fails. The client must not see internal error text.
	setupForwardRoute(t, "http://127.0.0.1:1/unreachable", "gpt-forward", "")
	rec := forwardRequest(t, "", "gpt-forward")
	if rec.Code != 502 {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "127.0.0.1") || strings.Contains(body, "dial") || strings.Contains(body, "connection refused") {
		t.Fatalf("client-facing error leaked internal detail: %s", body)
	}
	if !strings.Contains(body, "temporarily unavailable") && !strings.Contains(body, "upstream request failed") {
		t.Fatalf("expected generic error message, got: %s", body)
	}
}

func TestForwardCancelledClientDoesNotError(t *testing.T) {
	setupForwardRoute(t, "http://127.0.0.1:1/unreachable", "gpt-forward", "")
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-forward","messages":[]}`)).WithContext(ctx)
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(`{"model":"gpt-forward","messages":[]}`), "gpt-forward", false, "/chat/completions", false, "") {
		t.Fatal("expected route to match")
	}
	// A cancelled client should not produce a 502 upstream-failure body.
	if rec.Code == 502 {
		t.Fatalf("cancelled client should not be reported as upstream failure")
	}
}

func TestForwardHeaderAllowListDropsClientAuthorization(t *testing.T) {
	var gotAnthropicBeta, gotClientAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAnthropicBeta = r.Header.Get("Anthropic-Beta")
		gotClientAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	setupForwardRoute(t, upstream.URL, "gpt-forward", "")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-forward"}`))
	r.Header.Set("Anthropic-Beta", "beta-flag")
	r.Header.Set("Authorization", "Bearer client-key-should-not-forward")
	h := &Handler{}
	h.tryForwardUpstream(r, rec, []byte(`{"model":"gpt-forward"}`), "gpt-forward", false, "/chat/completions", false, "")

	if gotAnthropicBeta != "beta-flag" {
		t.Errorf("expected anthropic-beta forwarded, got %q", gotAnthropicBeta)
	}
	// The upstream credential replaces the client's; client key must not leak.
	if gotClientAuth == "Bearer client-key-should-not-forward" {
		t.Errorf("client Authorization must not be forwarded upstream")
	}
	if gotClientAuth != "Bearer upstream-secret" {
		t.Errorf("expected upstream credential, got %q", gotClientAuth)
	}
}
