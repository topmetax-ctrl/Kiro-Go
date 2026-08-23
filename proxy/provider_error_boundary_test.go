package proxy

import (
	"encoding/json"
	"errors"
	"kiro-go/apikey"
	"kiro-go/config"
	"kiro-go/providererr"
	"kiro-go/search"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const upstreamSecretCanary = "UPSTREAM_SECRET_CANARY_12345"
const bearerTokenCanary = "Bearer SECRET_TOKEN_CANARY"

func TestForwardNonStreamHidesProviderCanary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"account abc suspended token=` + upstreamSecretCanary + `","code":"ACCOUNT_SUSPENDED"}}`))
	}))
	defer upstream.Close()

	setupForwardRoute(t, upstream.URL, "gpt-canary", "")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-canary","messages":[]}`))
	r = withRequestIDContext(r, "req-canary-1")
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(`{"model":"gpt-canary","messages":[]}`), "gpt-canary", false, "/chat/completions", false, "") {
		t.Fatal("expected forward")
	}
	body := rec.Body.String()
	if strings.Contains(body, upstreamSecretCanary) || strings.Contains(body, "account abc") {
		t.Fatalf("client leaked provider error: %s", body)
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("upstream 403 must not become client 401")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, providererr.MsgError) && !strings.Contains(body, `"code":"provider_error"`) {
		t.Fatalf("expected public provider_error, got %s", body)
	}
}

func TestForward401DoesNotLookLikeClientAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid API key"}}`))
	}))
	defer upstream.Close()
	setupForwardRoute(t, upstream.URL, "gpt-auth", "")
	rec := forwardRequest(t, "", "gpt-auth")
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("upstream 401 leaked as client auth: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "invalid API key") {
		t.Fatalf("leaked: %s", rec.Body.String())
	}
}

func TestForward429UsesProviderCode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted for account X"}}`))
	}))
	defer upstream.Close()
	setupForwardRoute(t, upstream.URL, "gpt-429", "")
	rec := forwardRequest(t, "", "gpt-429")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "account X") || strings.Contains(body, "quota exhausted") {
		t.Fatalf("leaked: %s", body)
	}
	if !strings.Contains(body, "provider_rate_limited") && !strings.Contains(body, "rate limited") {
		t.Fatalf("expected provider rate limit envelope: %s", body)
	}
}

func TestForwardStreamRewritesErrorFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"" + upstreamSecretCanary + "\"}}\n\n"))
		fl.Flush()
	}))
	defer upstream.Close()
	setupForwardRoute(t, upstream.URL, "gpt-stream-canary", "")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-stream-canary","messages":[]}`))
	h := &Handler{}
	if !h.tryForwardUpstream(r, rec, []byte(`{"model":"gpt-stream-canary"}`), "gpt-stream-canary", true, "/chat/completions", false, "") {
		t.Fatal("expected forward")
	}
	body := rec.Body.String()
	if strings.Contains(body, upstreamSecretCanary) {
		t.Fatalf("stream leaked canary: %s", body)
	}
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("expected rewritten error frame: %s", body)
	}
}

func TestAdminProviderErrorRedactsBearerButKeepsDiagnostic(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	svc, err := apikey.Open(filepath.Join(t.TempDir(), "k.db"), []byte("pepper-32-bytes-long-for-tests!!!!"), apikey.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	h := &Handler{keys: svc}

	in := providererr.FromHTTP(403, []byte(`{"error":{"message":"account abc suspended `+bearerTokenCanary+`","code":"ACCOUNT_SUSPENDED"}}`), nil)
	in.RequestID = "req-admin-1"
	in.ProviderName = "9router"
	h.persistProviderError(in)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/provider-errors?requestId=req-admin-1", nil)
	req.Header.Set("X-Admin-Password", config.GetPassword())
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != 200 {
		t.Fatalf("admin %d %s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if strings.Contains(got, "SECRET_TOKEN_CANARY") {
		t.Fatalf("admin stored credential: %s", got)
	}
	if !strings.Contains(got, "ACCOUNT_SUSPENDED") && !strings.Contains(got, "account abc") {
		t.Fatalf("admin lost diagnostic: %s", got)
	}

	anon := httptest.NewRequest(http.MethodGet, "/admin/api/provider-errors?requestId=req-admin-1", nil)
	anonRec := httptest.NewRecorder()
	h.handleAdminAPI(anonRec, anon)
	if anonRec.Code != 401 {
		t.Fatalf("anonymous got %d", anonRec.Code)
	}

	portal := httptest.NewRequest(http.MethodGet, "/portal/api/events?range=24H", nil)
	portalRec := httptest.NewRecorder()
	h.ServeHTTP(portalRec, portal)
	if strings.Contains(portalRec.Body.String(), "account abc") || strings.Contains(portalRec.Body.String(), "SECRET_TOKEN") {
		t.Fatalf("portal leaked: %s", portalRec.Body.String())
	}
}

func TestLocalAuthErrorsUnchanged(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	on := true
	if err := config.UpdateSettingsPatch(nil, &on, ""); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("expected 401, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Fatalf("local auth envelope changed: %s", rec.Body.String())
	}
}

func TestKiroUpstreamErrorNotSentViaPublicMapper(t *testing.T) {
	ke := &KiroUpstreamError{StatusCode: 403, Endpoint: "Kiro", Body: `{"message":"` + upstreamSecretCanary + `"}`}
	pub := classifyGoError(ke, "req-k", "kiro", "claude", "m", "acct", "", "").Public()
	if strings.Contains(pub.Message, upstreamSecretCanary) {
		t.Fatal(pub.Message)
	}
	if pub.HTTPStatus == 401 {
		t.Fatal("kiro 403 became 401")
	}
	raw, _ := json.Marshal(pub)
	if strings.Contains(string(raw), "Kiro") {
		t.Fatalf("provider name in public json: %s", raw)
	}
}

func TestPortalEventHasNoRawDetail(t *testing.T) {
	s, err := apikey.Open(filepath.Join(t.TempDir(), "k.db"), []byte("pepper-32-bytes-long-for-tests!!!!"), apikey.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	rec, secret, err := s.Create(apikey.CreateInput{Name: "p", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutProviderErrorDetail(apikey.ProviderErrorDetail{
		RequestID: "req-portal", Detail: "account abc " + upstreamSecretCanary, PublicCode: apikey.ErrorProviderError,
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Commit(rec.Key.ID, apikey.CommitInput{
		RequestID: "req-portal", Outcome: apikey.OutcomeFailed, Endpoint: "openai",
		ErrorCode: apikey.ErrorProviderError, SanitizedError: providererr.MsgError, StatusCode: 502,
	})
	page, err := s.ListEvents(rec.Key.ID, apikey.EventQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(page)
	if strings.Contains(string(b), upstreamSecretCanary) || strings.Contains(string(b), "account abc") {
		t.Fatalf("portal list leaked: %s", b)
	}
	if len(page.Items) != 1 || page.Items[0].ErrorCode != apikey.ErrorProviderError {
		t.Fatalf("items=%+v", page.Items)
	}
	_ = secret
}

func TestWebSearchProviderErrorHidesKindAndCanary(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	err := &search.ProviderError{
		Kind:       search.ErrTimeout,
		StatusCode: 504,
		Err:        errors.New(upstreamSecretCanary),
	}
	h.sendClaudeErrorForWebSearch(rec, err)
	body := rec.Body.String()
	if strings.Contains(body, upstreamSecretCanary) {
		t.Fatalf("leaked canary: %s", body)
	}
	if strings.Contains(body, string(search.ErrTimeout)) || strings.Contains(body, "web_search provider") {
		t.Fatalf("leaked search kind: %s", body)
	}
	if rec.Code != http.StatusGatewayTimeout && rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
}
