package proxy

import (
	"encoding/json"
	"kiro-go/apikey"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminCreateApiKeyBatch(t *testing.T) {
	h, _ := testKeyHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/batch",
		strings.NewReader(`{"count":3,"name":"team","enabled":true,"tokenLimit":1000}`))
	req.Header.Set("X-Admin-Password", config.GetPassword())
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool `json:"success"`
		Count   int  `json:"count"`
		Keys    []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Key  string `json:"key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Success || body.Count != 3 || len(body.Keys) != 3 {
		t.Fatalf("body %#v", body)
	}
	if body.Keys[0].Name != "team-01" || body.Keys[2].Name != "team-03" {
		t.Fatalf("names %q %q", body.Keys[0].Name, body.Keys[2].Name)
	}
	if body.Keys[0].Key == "" || !strings.HasPrefix(body.Keys[0].Key, "sk-") {
		t.Fatalf("missing secret %q", body.Keys[0].Key)
	}

	bad := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/batch", strings.NewReader(`{"count":0}`))
	bad.Header.Set("X-Admin-Password", config.GetPassword())
	badRec := httptest.NewRecorder()
	h.handleAdminAPI(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for count=0, got %d", badRec.Code)
	}
}

func TestPortalRememberPersistsCookie(t *testing.T) {
	h, svc := testKeyHandler(t)
	_, secret, err := svc.Create(apikey.CreateInput{Name: "remember", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	sess := httptest.NewRequest(http.MethodPost, "/portal/api/session",
		strings.NewReader(`{"key":"`+secret+`","remember":true}`))
	rec := httptest.NewRecorder()
	h.handlePortal(rec, sess)
	if rec.Code != 200 {
		t.Fatalf("session %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected cookie")
	}
	minAge := int((29 * 24 * time.Hour).Seconds())
	if cookies[0].MaxAge < minAge {
		t.Fatalf("remember MaxAge=%d", cookies[0].MaxAge)
	}
	exp, err := svc.SessionExpiry(cookies[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) < 29*24*time.Hour {
		t.Fatalf("server ttl %v", time.Until(exp))
	}

	plain := httptest.NewRequest(http.MethodPost, "/portal/api/session",
		strings.NewReader(`{"key":"`+secret+`"}`))
	plainRec := httptest.NewRecorder()
	h.handlePortal(plainRec, plain)
	if plainRec.Code != 200 {
		t.Fatalf("plain session %d", plainRec.Code)
	}
	plainCookies := plainRec.Result().Cookies()
	if len(plainCookies) == 0 || plainCookies[0].MaxAge != 0 {
		t.Fatalf("expected session cookie, got %#v", plainCookies)
	}
}

func TestOptionalPresentedKeyBindsStore(t *testing.T) {
	h, svc := testKeyHandler(t)
	off := false
	if err := config.UpdateSettingsPatch(nil, &off, ""); err != nil {
		t.Fatal(err)
	}
	rec, secret, err := svc.Create(apikey.CreateInput{Name: "opt", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := h.authenticate(newAuthTestRequest(t, "Authorization", "Bearer "+secret))
	if err != nil || entry == nil || entry.ID != rec.Key.ID {
		t.Fatalf("expected store key to bind while gate is off, got %+v err=%v", entry, err)
	}
	if entry, err := h.authenticate(newAuthTestRequest(t, "Authorization", "Bearer sk-unknown")); err != nil || entry != nil {
		t.Fatalf("unknown key must stay anonymous, got %+v err=%v", entry, err)
	}
}
