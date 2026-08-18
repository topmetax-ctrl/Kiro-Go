package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/apikey"
)

func portalCookie(t *testing.T, h *Handler, secret string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/portal/api/session", strings.NewReader(`{"key":"`+secret+`"}`))
	rec := httptest.NewRecorder()
	h.handlePortal(rec, req)
	if rec.Code != 200 {
		t.Fatalf("session %d %s", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("no cookie")
	}
	return rec.Result().Cookies()[0]
}

func TestPortalQueryContractAndIsolation(t *testing.T) {
	h, svc := testKeyHandler(t)
	a, sa, err := svc.Create(apikey.CreateInput{Name: "alice", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := svc.Create(apikey.CreateInput{Name: "bob", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Commit(a.Key.ID, apikey.CommitInput{
		RequestID: "a-ok", Outcome: apikey.OutcomeSuccess, Endpoint: "openai",
		ClientModel: "m-a", EffectiveModel: "m-a", InputTokens: 4, StatusCode: 200, Stream: true,
		TTFBMs: 15, TTFBKnown: true,
	})
	_ = svc.Commit(a.Key.ID, apikey.CommitInput{
		RequestID: "a-fail", Outcome: apikey.OutcomeFailed, Endpoint: "claude",
		ClientModel: "m-a", EffectiveModel: "m-a", StatusCode: 500, ErrorCode: apikey.ErrorProviderError,
	})
	_ = svc.Commit(b.Key.ID, apikey.CommitInput{
		RequestID: "b-secret", Outcome: apikey.OutcomeSuccess, Endpoint: "openai",
		ClientModel: "m-b", EffectiveModel: "m-b", InputTokens: 99, StatusCode: 200,
	})

	ck := portalCookie(t, h, sa)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.handlePortal(rec, req)
		return rec
	}

	rec := get("/portal/api/events?range=24H&keyId=" + b.Key.ID)
	if rec.Code != 200 {
		t.Fatalf("events %d %s", rec.Code, rec.Body.String())
	}
	var evBody struct {
		Items            []apikey.PublicEvent `json:"items"`
		Resolution       string               `json:"resolution"`
		DataRetention    apikey.RetentionMeta `json:"dataRetention"`
		RawAvailableFrom int64                `json:"rawAvailableFrom"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &evBody)
	if evBody.Resolution != apikey.ResolutionRaw || evBody.DataRetention.RawEventsDays == 0 {
		t.Fatalf("history metadata %+v", evBody)
	}
	for _, ev := range evBody.Items {
		if ev.RequestID == "b-secret" || ev.InputTokens == 99 || ev.ApiKeyID == b.Key.ID {
			t.Fatalf("keyId param leaked bob: %+v", ev)
		}
		if ev.ApiKeyID != "" {
			t.Fatalf("portal must strip apiKeyId: %+v", ev)
		}
	}

	rec = get("/portal/api/events?range=24H&status=failed&error_code=" + apikey.ErrorProviderError)
	_ = json.Unmarshal(rec.Body.Bytes(), &evBody)
	if len(evBody.Items) != 1 || evBody.Items[0].RequestID != "a-fail" {
		t.Fatalf("status+error filter %+v", evBody.Items)
	}

	rec = get("/portal/api/events?range=24H&model=m-a&endpoint=openai&streaming=true")
	_ = json.Unmarshal(rec.Body.Bytes(), &evBody)
	if len(evBody.Items) != 1 || evBody.Items[0].RequestID != "a-ok" {
		t.Fatalf("model/endpoint/stream %+v", evBody.Items)
	}
	if evBody.Items[0].TTFBMs == nil || *evBody.Items[0].TTFBMs != 15 {
		t.Fatalf("ttfb %+v", evBody.Items[0].TTFBMs)
	}

	rec = get("/portal/api/usage?range=1H")
	if rec.Code != 200 {
		t.Fatalf("usage %d %s", rec.Code, rec.Body.String())
	}
	var usage struct {
		Resolution string               `json:"resolution"`
		Source     string               `json:"source"`
		Metrics    []string             `json:"metrics"`
		Points     []apikey.SeriesPoint `json:"points"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &usage)
	if usage.Resolution != apikey.Resolution1m || usage.Source != apikey.SourceEvents {
		t.Fatalf("usage meta %+v", usage)
	}
	if len(usage.Metrics) == 0 {
		t.Fatal("missing metric allowlist")
	}

	from := time.Now().Add(-90 * 24 * time.Hour).Unix()
	to := time.Now().Unix()
	rec = get(fmt.Sprintf("/portal/api/usage?range=CUSTOM&from=%d&to=%d", from, to))
	if rec.Code != 200 {
		t.Fatalf("90d usage %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &usage)
	if usage.Source != apikey.SourceHourly {
		t.Fatalf("90d unfiltered should be hourly, got %s", usage.Source)
	}

	rec = get("/portal/api/usage?range=CUSTOM")
	if rec.Code != 400 {
		t.Fatalf("invalid custom %d %s", rec.Code, rec.Body.String())
	}
	rec = get("/portal/api/events?range=CUSTOM&from=100&to=50")
	if rec.Code != 400 {
		t.Fatalf("from>to %d", rec.Code)
	}
	rec = get("/portal/api/usage?metric=drop-table")
	if rec.Code != 400 {
		t.Fatalf("bad metric %d %s", rec.Code, rec.Body.String())
	}
}

func TestPortalCursorPages(t *testing.T) {
	h, svc := testKeyHandler(t)
	rec, sa, err := svc.Create(apikey.CreateInput{Name: "p", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		_ = svc.Commit(rec.Key.ID, apikey.CommitInput{
			RequestID: fmt.Sprintf("e-%d", i), Outcome: apikey.OutcomeSuccess,
			Endpoint: "openai", StatusCode: 200,
		})
	}
	ck := portalCookie(t, h, sa)
	seen := map[string]struct{}{}
	path := "/portal/api/events?range=24H&limit=3"
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(ck)
		w := httptest.NewRecorder()
		h.handlePortal(w, req)
		var body struct {
			Items      []apikey.PublicEvent `json:"items"`
			NextCursor string               `json:"nextCursor"`
			HasMore    bool                 `json:"hasMore"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		for _, ev := range body.Items {
			if _, dup := seen[ev.RequestID]; dup {
				t.Fatalf("duplicate page item %s", ev.RequestID)
			}
			seen[ev.RequestID] = struct{}{}
		}
		if !body.HasMore {
			break
		}
		path = "/portal/api/events?range=24H&limit=3&cursor=" + body.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("cursor walked %d events: %+v", len(seen), seen)
	}
}
