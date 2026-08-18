package proxy

import (
	"encoding/json"
	"kiro-go/apikey"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testKeyHandler(t *testing.T) (*Handler, *apikey.Service) {
	t.Helper()
	mustInitConfig(t)
	on := true
	if err := config.UpdateSettingsPatch(nil, &on, ""); err != nil {
		t.Fatal(err)
	}
	pepper, err := config.GetOrCreateAPIKeyPepper()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apikey.Open(filepath.Join(t.TempDir(), "apikeys.db"), pepper, apikey.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return &Handler{keys: svc}, svc
}

func TestPortalCannotReadOtherKey(t *testing.T) {
	h, svc := testKeyHandler(t)
	a, sa, err := svc.Create(apikey.CreateInput{Name: "alice", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := svc.Create(apikey.CreateInput{Name: "bob", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Commit(b.Key.ID, apikey.CommitInput{Outcome: apikey.OutcomeSuccess, InputTokens: 42, Endpoint: "openai"})

	req := httptest.NewRequest(http.MethodPost, "/portal/api/session", strings.NewReader(`{"key":"`+sa+`"}`))
	rec := httptest.NewRecorder()
	h.handlePortal(rec, req)
	if rec.Code != 200 {
		t.Fatalf("session: %d %s", rec.Code, rec.Body.String())
	}
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 {
		t.Fatal("expected portal cookie")
	}

	sumReq := httptest.NewRequest(http.MethodGet, "/portal/api/summary", nil)
	sumReq.AddCookie(cookie[0])
	sumRec := httptest.NewRecorder()
	h.handlePortal(sumRec, sumReq)
	if sumRec.Code != 200 {
		t.Fatalf("summary: %d", sumRec.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(sumRec.Body.Bytes(), &body)
	if body["name"] != "alice" {
		t.Fatalf("expected alice summary, got %#v", body)
	}

	evReq := httptest.NewRequest(http.MethodGet, "/portal/api/events?range=30D", nil)
	evReq.AddCookie(cookie[0])
	evRec := httptest.NewRecorder()
	h.handlePortal(evRec, evReq)
	var evs struct {
		Items []apikey.PublicEvent `json:"items"`
	}
	_ = json.Unmarshal(evRec.Body.Bytes(), &evs)
	for _, ev := range evs.Items {
		if ev.InputTokens == 42 {
			t.Fatalf("alice session leaked bob event")
		}
	}
	_ = a
}

func TestAliceCannotUseBobCursorOrLastEventID(t *testing.T) {
	h, svc := testKeyHandler(t)
	a, sa, err := svc.Create(apikey.CreateInput{Name: "alice", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := svc.Create(apikey.CreateInput{Name: "bob", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Commit(b.Key.ID, apikey.CommitInput{RequestID: "bob-only", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", InputTokens: 99, StatusCode: 200})
	_ = svc.Commit(a.Key.ID, apikey.CommitInput{RequestID: "alice-only", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	bobPage, err := svc.ListEvents(b.Key.ID, apikey.EventQuery{Limit: 1})
	if err != nil || len(bobPage.Items) == 0 {
		t.Fatal(err)
	}
	bobHW, _ := svc.EventHighWater(b.Key.ID)

	req := httptest.NewRequest(http.MethodPost, "/portal/api/session", strings.NewReader(`{"key":"`+sa+`"}`))
	rec := httptest.NewRecorder()
	h.handlePortal(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("cookie")
	}

	evReq := httptest.NewRequest(http.MethodGet, "/portal/api/events?range=30D&cursor="+bobPage.NextCursor, nil)
	if bobPage.NextCursor == "" {
		cur := apikey.EncodeCursor(bobPage.Items[0].Timestamp.Unix(), bobPage.Items[0].EventID)
		evReq = httptest.NewRequest(http.MethodGet, "/portal/api/events?range=30D&cursor="+cur, nil)
	}
	evReq.AddCookie(cookies[0])
	evRec := httptest.NewRecorder()
	h.handlePortal(evRec, evReq)
	if evRec.Code != 200 {
		t.Fatalf("cursor %d %s", evRec.Code, evRec.Body.String())
	}
	if strings.Contains(evRec.Body.String(), "bob-only") || strings.Contains(evRec.Body.String(), "99") {
		t.Fatalf("alice cursor leaked bob: %s", evRec.Body.String())
	}

	srv := httptest.NewServer(http.HandlerFunc(h.handlePortal))
	t.Cleanup(srv.Close)
	stream, cancel := openPortalSSE(t, srv, cookies, "", fmtInt(bobHW))
	defer cancel()
	defer stream.Body.Close()
	frames := readSSE(stream.Body, func(fs []sseFrame) bool {
		for _, f := range fs {
			if strings.Contains(f.Data, "bob-only") {
				return true
			}
		}
		return len(fs) > 2
	}, 400*time.Millisecond)
	for _, f := range frames {
		if strings.Contains(f.Data, "bob-only") {
			t.Fatalf("Last-Event-ID leaked bob: %+v", f)
		}
	}
}

func fmtInt(n int64) string {
	return strconv.FormatInt(n, 10)
}

func TestPortalTokenExchangeHeaders(t *testing.T) {
	h, svc := testKeyHandler(t)
	rec, _, err := svc.Create(apikey.CreateInput{Name: "ex", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := svc.CreatePortalToken(rec.Key.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/usage/p/"+tok, nil)
	h.handleUsagePage(w, r)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("Referrer-Policy %q", w.Header().Get("Referrer-Policy"))
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/usage") {
		t.Fatalf("location %q", loc)
	}
	if strings.Contains(w.Header().Get("Location"), "pt-") {
		t.Fatal("redirect leaked portal token")
	}
}

func TestRedactPortalTokenPath(t *testing.T) {
	if redactHTTPPath("/usage/p/pt-secret") != "/usage/p/[redacted]" {
		t.Fatal(redactHTTPPath("/usage/p/pt-secret"))
	}
	if redactHTTPPath("/portal/api/me") != "/portal/api/me" {
		t.Fatal("non-token path changed")
	}
}

func TestPortalTokenCannotCallV1(t *testing.T) {
	h, svc := testKeyHandler(t)
	rec, _, err := svc.Create(apikey.CreateInput{Name: "p", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := svc.CreatePortalToken(rec.Key.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+tok)
	entry, err := h.authenticate(r)
	if err == nil {
		t.Fatalf("portal token authenticated as API key: %+v", entry)
	}
}

func TestAdminListNeverReturnsPlaintext(t *testing.T) {
	h, svc := testKeyHandler(t)
	_, secret, err := svc.Create(apikey.CreateInput{Name: "s", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.apiListApiKeys(rec, httptest.NewRequest(http.MethodGet, "/admin/api/api-keys", nil))
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("list leaked plaintext secret")
	}
	if !strings.Contains(rec.Body.String(), "keyMasked") {
		t.Fatalf("expected keyMasked: %s", rec.Body.String())
	}
}

func TestExpiredAndDisabledAuthMessages(t *testing.T) {
	h, svc := testKeyHandler(t)
	_, secret, err := svc.Create(apikey.CreateInput{Name: "off", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	r := newAuthTestRequest(t, "Authorization", "Bearer "+secret)
	_, err = h.authenticate(r)
	ae, _ := err.(*authError)
	if ae == nil || !strings.Contains(ae.message, "disabled") {
		t.Fatalf("disabled: %v", err)
	}
}
