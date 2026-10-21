package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kiro-go/config"
	"kiro-go/tokenopt"
)

func tokenOptAdmin(t *testing.T, h *Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/admin/api"+path, strings.NewReader(body))
	req.Header.Set("X-Admin-Password", config.GetPassword())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	return rec
}

func setupTokenOptAdmin(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	t.Cleanup(h.closeTokenOpt)
	return h
}

func adminState(t *testing.T, h *Handler, query string) tokenOptAdminResponse {
	t.Helper()
	rec := tokenOptAdmin(t, h, http.MethodGet, "/token-optimization"+query, "")
	if rec.Code != 200 {
		t.Fatalf("GET status %d: %s", rec.Code, rec.Body.String())
	}
	var out tokenOptAdminResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET body: %v\n%s", err, rec.Body.String())
	}
	return out
}

func TestAdminTokenOptDefaultsAreOffAndTruthful(t *testing.T) {
	h := setupTokenOptAdmin(t)
	st := adminState(t, h, "")

	s := st.Settings
	if s.Mode != "off" || s.InputCompression != "none" || s.Caveman != "off" || s.Ponytail != "off" {
		t.Fatalf("defaults = %+v", s)
	}
	if s.Headroom.TimeoutMs != config.DefaultHeadroomTimeout || s.Headroom.Mode != "cache" || s.Headroom.APIKeySet {
		t.Fatalf("headroom defaults = %+v", s.Headroom)
	}
	rt := st.Runtime
	if rt.Headroom.Active || rt.Headroom.State != "disabled" || rt.RTK.GatewayLevel || rt.Caveman.State != "inactive" || rt.Ponytail.State != "inactive" {
		t.Fatalf("runtime = %+v", rt)
	}
	if !rt.AssetsVerified || len(rt.Assets) != 3 {
		t.Fatalf("assets: verified=%v n=%d", rt.AssetsVerified, len(rt.Assets))
	}
	for _, p := range []string{"claude", "openai", "responses"} {
		if c := rt.Protocols[p]; !c.Headroom || !c.Behavior {
			t.Errorf("protocol %s caps = %+v", p, c)
		}
	}
	if st.Limits.TestedHeadroomVersion != tokenopt.TestedHeadroomVersion || st.Limits.MinTimeoutMs != config.MinHeadroomTimeout {
		t.Fatalf("limits = %+v", st.Limits)
	}
}

// Saving a setting is not the same as the integration working. The UI gets both.
func TestAdminTokenOptSavedIsNotTheSameAsRunning(t *testing.T) {
	h := setupTokenOptAdmin(t)
	body := `{"mode":"on","inputCompression":"headroom","headroom":{"baseUrl":"http://127.0.0.1:1"},"ponytail":"full"}`
	if rec := tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", body); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	st := adminState(t, h, "?probe=1") // the engine does not exist
	if st.Settings.Mode != "on" || st.Settings.InputCompression != "headroom" {
		t.Fatalf("saved settings = %+v", st.Settings)
	}
	if st.Runtime.Headroom.State != "disconnected" || st.Runtime.Headroom.LastCheck == nil || st.Runtime.Headroom.LastCheck.Error == "" {
		t.Fatalf("an unreachable engine must be reported as such: %+v", st.Runtime.Headroom)
	}
	if st.Runtime.Ponytail.State != "active" {
		t.Fatalf("ponytail needs no service and is active: %+v", st.Runtime.Ponytail)
	}
}

func TestAdminTokenOptPatchKeepsFieldsItDoesNotMention(t *testing.T) {
	h := setupTokenOptAdmin(t)
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"mode":"observe","caveman":"ultracave","headroom":{"timeoutMs":1800}}`)
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"ponytail":"lite"}`)
	s := adminState(t, h, "").Settings
	if s.Mode != "observe" || s.Caveman != "ultracave" || s.Ponytail != "lite" || s.Headroom.TimeoutMs != 1800 {
		t.Fatalf("a partial update clobbered other fields: %+v", s)
	}
}

func TestAdminTokenOptValidationErrorsNameTheField(t *testing.T) {
	h := setupTokenOptAdmin(t)
	cases := map[string]struct{ body, field string }{
		"bad mode":             {`{"mode":"turbo"}`, "mode"},
		"auto is unavailable":  {`{"inputCompression":"auto"}`, "inputCompression"},
		"unknown engine":       {`{"inputCompression":"zstd"}`, "inputCompression"},
		"megacave not offered": {`{"caveman":"megacave"}`, "caveman"},
		"bad ponytail":         {`{"ponytail":"max"}`, "ponytail"},
		"bad headroom mode":    {`{"headroom":{"mode":"ccr"}}`, "headroom.mode"},
		"timeout too small":    {`{"headroom":{"timeoutMs":5}}`, "headroom.timeoutMs"},
		"bad url":              {`{"headroom":{"baseUrl":"localhost:8787"}}`, "headroom.baseUrl"},
		"url with credentials": {`{"headroom":{"baseUrl":"http://u:p@127.0.0.1:8787"}}`, "headroom.baseUrl"},
		"live without a url":   {`{"mode":"on","inputCompression":"headroom"}`, "headroom.baseUrl"},
		"bad rtk url":          {`{"rtk":{"proxyUrl":"ftp://x"}}`, "rtk.proxyUrl"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var e map[string]string
			json.Unmarshal(rec.Body.Bytes(), &e)
			if e["field"] != c.field || e["error"] == "" {
				t.Fatalf("error = %v, want field %q with a message", e, c.field)
			}
		})
	}
	// Nothing above was stored.
	if raw := config.GetTokenOptimizationRaw(); raw != (config.TokenOptimizationConfig{}) {
		t.Fatalf("a rejected update was stored: %+v", raw)
	}
	if rec := tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{not json`); rec.Code != 400 {
		t.Fatalf("malformed JSON: %d", rec.Code)
	}
}

func TestAdminTokenOptNeverReturnsTheHeadroomToken(t *testing.T) {
	h := setupTokenOptAdmin(t)
	const secret = "s3cret-proxy-token-9876"
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"headroom":{"baseUrl":"http://127.0.0.1:8787","apiKey":"`+secret+`"}}`)

	rec := tokenOptAdmin(t, h, http.MethodGet, "/token-optimization", "")
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the token was returned by the API: %s", rec.Body.String())
	}
	s := adminState(t, h, "").Settings.Headroom
	if !s.APIKeySet || s.APIKeyMasked != "****9876" {
		t.Fatalf("view = %+v", s)
	}

	// Echoing the mask (or nothing) back keeps the stored token.
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"headroom":{"apiKey":"****9876"}}`)
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"headroom":{"apiKey":""}}`)
	if got := config.GetTokenOptimization().Headroom.APIKey; got != secret {
		t.Fatalf("token = %q after no-op saves", got)
	}
	// Only an explicit request removes it.
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"headroom":{"clearApiKey":true}}`)
	if got := config.GetTokenOptimization().Headroom.APIKey; got != "" {
		t.Fatalf("token not cleared: %q", got)
	}
}

// A very short, human-chosen token must not be shown either.
func TestAdminTokenOptMasksShortTokensCompletely(t *testing.T) {
	h := setupTokenOptAdmin(t)
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"headroom":{"baseUrl":"http://127.0.0.1:8787","apiKey":"abc"}}`)
	rec := tokenOptAdmin(t, h, http.MethodGet, "/token-optimization", "")
	if strings.Contains(rec.Body.String(), `"abc"`) || !strings.Contains(rec.Body.String(), `"****"`) {
		t.Fatalf("short token leaked or unmasked: %s", rec.Body.String())
	}
}

func TestAdminTokenOptTestEndpointVerifiesHeadroomAndPersistsOnlyTheSavedTarget(t *testing.T) {
	h := setupTokenOptAdmin(t)
	stub := newHeadroomStub(t)
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"mode":"on","inputCompression":"headroom","headroom":{"baseUrl":"`+stub.server.URL+`"}}`)

	// Testing unsaved values must not change what the status shows.
	other := newHeadroomStub(t)
	rec := tokenOptAdmin(t, h, http.MethodPost, "/token-optimization/test", `{"target":"headroom","headroom":{"baseUrl":"`+other.server.URL+`"}}`)
	var out struct {
		Report tokenopt.HealthReport `json:"report"`
		State  tokenOptAdminResponse `json:"state"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || !out.Report.Reachable || !out.Report.CompressOK || out.Report.Compat != "tested" {
		t.Fatalf("unsaved test: %d %+v", rec.Code, out.Report)
	}
	if out.State.Runtime.Headroom.LastCheck != nil {
		t.Fatalf("a probe of unsaved values became the stored status: %+v", out.State.Runtime.Headroom.LastCheck)
	}

	// Testing the saved target records it.
	rec = tokenOptAdmin(t, h, http.MethodPost, "/token-optimization/test", `{"target":"headroom"}`)
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.State.Runtime.Headroom.State != "connected" || out.State.Runtime.Headroom.LastCheck == nil {
		t.Fatalf("saved target not recorded: %+v", out.State.Runtime.Headroom)
	}
}

// The saved token is for the saved URL only.
func TestAdminTokenOptTestDoesNotSendTheSavedTokenToAnotherAddress(t *testing.T) {
	h := setupTokenOptAdmin(t)
	saved, typed := newHeadroomStub(t), newHeadroomStub(t)
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"headroom":{"baseUrl":"`+saved.server.URL+`","apiKey":"saved-secret-token"}}`)

	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization/test", `{"target":"headroom","headroom":{"baseUrl":"`+typed.server.URL+`"}}`)
	for _, tok := range typed.tokens {
		if tok != "" {
			t.Fatalf("the saved token was sent to an address the operator only typed: %q", tok)
		}
	}
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization/test", `{"target":"headroom"}`)
	if len(saved.tokens) == 0 || saved.tokens[len(saved.tokens)-1] != "saved-secret-token" {
		t.Fatalf("the saved URL should receive the saved token: %v", saved.tokens)
	}
}

func TestAdminTokenOptTestRejectsBadInput(t *testing.T) {
	h := setupTokenOptAdmin(t)
	for name, body := range map[string]string{
		"unknown target": `{"target":"zstd"}`,
		"no url yet":     `{"target":"headroom"}`,
		"bad url":        `{"target":"headroom","headroom":{"baseUrl":"gopher://x"}}`,
		"rtk no url":     `{"target":"rtk"}`,
		"bad json":       `{`,
	} {
		if rec := tokenOptAdmin(t, h, http.MethodPost, "/token-optimization/test", body); rec.Code != 400 {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

// RTK: the daemon is reachable AND a provider routes through it are two
// separate facts, and neither makes the gateway itself compress anything.
func TestAdminTokenOptRTKReportsWiringHonestly(t *testing.T) {
	h := setupTokenOptAdmin(t)
	rtk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte("ok"))
			return
		}
		http.NotFound(w, r)
	}))
	defer rtk.Close()

	provider := config.UpstreamProvider{ID: "p1", Name: "via-rtk", BaseURL: rtk.URL + "/v1", ApiKey: "k", Enabled: true}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{provider}, nil); err != nil {
		t.Fatal(err)
	}
	if rec := tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"mode":"on","inputCompression":"rtk","rtk":{"proxyUrl":"`+rtk.URL+`"}}`); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	st := adminState(t, h, "?probe=1")
	r := st.Runtime.RTK
	if !r.Selected || r.GatewayLevel || r.State != "connected" || len(r.RoutedProviders) != 1 || r.RoutedProviders[0] != "via-rtk" {
		t.Fatalf("rtk = %+v", r)
	}
	// Selecting RTK activates nothing in the gateway.
	if st.Runtime.Headroom.Active || st.Runtime.Headroom.Selected {
		t.Fatalf("headroom should not be affected: %+v", st.Runtime.Headroom)
	}

	// With no proxy configured RTK is client-side only, and the API says so.
	tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", `{"rtk":{"proxyUrl":""}}`)
	if r := adminState(t, h, "").Runtime.RTK; r.State != "client_side" || len(r.RoutedProviders) != 0 {
		t.Fatalf("rtk = %+v", r)
	}
}

// Concurrent admin saves of different fields must all land (no lost update).
func TestAdminTokenOptConcurrentSavesLoseNothing(t *testing.T) {
	h := setupTokenOptAdmin(t)
	bodies := []string{
		`{"mode":"observe"}`, `{"caveman":"caveman"}`, `{"ponytail":"ultra"}`,
		`{"headroom":{"timeoutMs":2222}}`, `{"headroom":{"mode":"token"}}`,
	}
	var wg sync.WaitGroup
	for _, b := range bodies {
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			if rec := tokenOptAdmin(t, h, http.MethodPost, "/token-optimization", b); rec.Code != 200 {
				t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
			}
		}(b)
	}
	wg.Wait()
	s := adminState(t, h, "").Settings
	if s.Mode != "observe" || s.Caveman != "caveman" || s.Ponytail != "ultra" || s.Headroom.TimeoutMs != 2222 || s.Headroom.Mode != "token" {
		t.Fatalf("a concurrent save was lost: %+v", s)
	}
}

// The admin route is behind the same password gate as every other admin API.
func TestAdminTokenOptRequiresTheAdminPassword(t *testing.T) {
	h := setupTokenOptAdmin(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/token-optimization", nil)
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
