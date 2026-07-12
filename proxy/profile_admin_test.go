package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func setupProfileAdmin(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", AccessToken: "t", AuthMethod: "external_idp"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := newTestHandler(p)
	return h
}

func TestApiDiscoverAccountProfilesReturnsNoSecrets(t *testing.T) {
	h := setupProfileAdmin(t)
	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "us-east-1" {
			return []DiscoveredProfile{{ARN: "arn:a", Region: region, DisplayName: "Prod"}}, nil
		}
		return nil, fmt.Errorf("empty")
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/api/accounts/acct-1/profiles", nil)
	h.apiDiscoverAccountProfiles(rec, r, "acct-1")

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "arn:a") || !strings.Contains(body, "Prod") {
		t.Fatalf("expected discovered profile in body, got %s", body)
	}
	// Must not leak token material.
	for _, secret := range []string{"accessToken", "refreshToken", "\"t\"", "\"r\""} {
		if strings.Contains(body, secret) {
			t.Errorf("profile discovery response leaked %q: %s", secret, body)
		}
	}
}

func TestApiSelectAccountProfileHappyPath(t *testing.T) {
	h := setupProfileAdmin(t)
	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "eu-central-1" {
			return []DiscoveredProfile{{ARN: "arn:eu", Region: region}}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	withStubModelLister(t, func(*config.Account) ([]ModelInfo, error) {
		return []ModelInfo{{ModelId: "m1"}}, nil
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/acct-1/profile",
		strings.NewReader(`{"arn":"arn:eu","region":"eu-central-1"}`))
	h.apiSelectAccountProfile(rec, r, "acct-1")

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Fatalf("expected success, got %v", resp)
	}
	if got := h.pool.GetByID("acct-1").ProfileArn; got != "arn:eu" {
		t.Fatalf("expected pinned arn:eu, got %q", got)
	}
}

func TestApiSelectAccountProfileRejectsBadBody(t *testing.T) {
	h := setupProfileAdmin(t)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/acct-1/profile",
		strings.NewReader(`{not json`))
	h.apiSelectAccountProfile(rec, r, "acct-1")
	if rec.Code != 400 {
		t.Fatalf("expected 400 for bad body, got %d", rec.Code)
	}
}
