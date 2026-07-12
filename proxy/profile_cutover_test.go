package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// TestSelectProfileRebuildsAggregateDroppingStaleModels proves that after a
// switch the global /v1/models aggregate loses models only the old profile
// offered, while another account's models are preserved.
func TestSelectProfileRebuildsAggregateDroppingStaleModels(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", ProfileArn: "arn:old", AccessToken: "t", AuthMethod: "external_idp"})
	_ = config.AddAccount(config.Account{ID: "acct-2", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", ProfileArn: "arn:two", AccessToken: "t", AuthMethod: "external_idp"})
	p := accountpool.GetPool()
	p.Reload()

	h := newTestHandler(p)
	// Seed the aggregate: acct-1 currently offers old-only-model; acct-2 offers shared-model.
	h.setAccountModelInfo("acct-1", []ModelInfo{{ModelId: "old-only-model"}, {ModelId: "shared-model"}})
	h.setAccountModelInfo("acct-2", []ModelInfo{{ModelId: "shared-model"}})
	h.modelsCacheMu.Lock()
	h.rebuildAggregateLocked()
	h.modelsCacheMu.Unlock()

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "eu-central-1" {
			return []DiscoveredProfile{{ARN: "arn:eu-target", Region: region}}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	// After switching acct-1 to the EU profile, it only offers shared-model.
	withStubModelLister(t, func(*config.Account) ([]ModelInfo, error) {
		return []ModelInfo{{ModelId: "shared-model"}}, nil
	})

	if _, err := h.SelectProfile(context.Background(), "acct-1", "arn:eu-target", "eu-central-1"); err != nil {
		t.Fatalf("SelectProfile: %v", err)
	}

	h.modelsCacheMu.RLock()
	agg := map[string]bool{}
	for _, m := range h.cachedModels {
		agg[m.ModelId] = true
	}
	h.modelsCacheMu.RUnlock()

	if agg["old-only-model"] {
		t.Fatalf("old-only-model must drop from aggregate after switch, got %v", agg)
	}
	if !agg["shared-model"] {
		t.Fatalf("shared-model (still offered by acct-2 and new acct-1 profile) must remain, got %v", agg)
	}
}

// TestSelectProfileSameAccountSerialized runs two concurrent switches of the
// SAME account and asserts a deterministic final state (no interleaving) and no
// race. The per-account lock guarantees the two switches apply one after another.
func TestSelectProfileSameAccountSerialized(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", AccessToken: "t", AuthMethod: "external_idp"})
	p := accountpool.GetPool()
	p.Reload()

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		// Both targets exist in their respective regions.
		switch region {
		case "us-east-1":
			return []DiscoveredProfile{{ARN: "arn:us", Region: region}}, nil
		case "eu-central-1":
			return []DiscoveredProfile{{ARN: "arn:eu", Region: region}}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	withStubModelLister(t, func(acc *config.Account) ([]ModelInfo, error) {
		return []ModelInfo{{ModelId: "m-" + acc.ProfileArn}}, nil
	})
	h := newTestHandler(p)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = h.SelectProfile(context.Background(), "acct-1", "arn:us", "us-east-1") }()
	go func() {
		defer wg.Done()
		_, _ = h.SelectProfile(context.Background(), "acct-1", "arn:eu", "eu-central-1")
	}()
	wg.Wait()

	// Whichever won last, the account must be in exactly ONE consistent state:
	// ARN and region agree, and the routing model list matches that ARN. No
	// "new ARN + old region" or "new profile + stale model" mixing.
	acc := p.GetByID("acct-1")
	wantModel := "m-" + acc.ProfileArn
	ml := p.GetModelList("acct-1")
	if len(ml) != 1 || ml[0] != wantModel {
		t.Fatalf("model list must match pinned ARN %q, got %v", acc.ProfileArn, ml)
	}
	if acc.ProfileArn == "arn:us" && acc.EffectiveApiRegion() != "us-east-1" {
		t.Fatalf("arn:us must pair with us-east-1, got %q", acc.EffectiveApiRegion())
	}
	if acc.ProfileArn == "arn:eu" && acc.EffectiveApiRegion() != "eu-central-1" {
		t.Fatalf("arn:eu must pair with eu-central-1, got %q", acc.EffectiveApiRegion())
	}
}

// TestSelectProfileDifferentAccountsParallel confirms switches of DIFFERENT
// accounts are not serialized by a global lock (they can overlap) and both
// complete correctly under -race.
func TestSelectProfileDifferentAccountsParallel(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", AccessToken: "t", AuthMethod: "external_idp"})
	_ = config.AddAccount(config.Account{ID: "acct-2", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", AccessToken: "t", AuthMethod: "external_idp"})
	p := accountpool.GetPool()
	p.Reload()

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "eu-central-1" {
			return []DiscoveredProfile{{ARN: "arn:eu-1", Region: region}, {ARN: "arn:eu-2", Region: region}}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	withStubModelLister(t, func(acc *config.Account) ([]ModelInfo, error) {
		return []ModelInfo{{ModelId: "m-" + acc.ID}}, nil
	})
	h := newTestHandler(p)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = h.SelectProfile(context.Background(), "acct-1", "arn:eu-1", "eu-central-1")
	}()
	go func() {
		defer wg.Done()
		_, _ = h.SelectProfile(context.Background(), "acct-2", "arn:eu-2", "eu-central-1")
	}()
	wg.Wait()

	if got := p.GetByID("acct-1").ProfileArn; got != "arn:eu-1" {
		t.Fatalf("acct-1 expected arn:eu-1, got %q", got)
	}
	if got := p.GetByID("acct-2").ProfileArn; got != "arn:eu-2" {
		t.Fatalf("acct-2 expected arn:eu-2, got %q", got)
	}
}

// TestApiGetAccountsExposesCurrentProfile checks the additive read-only profile
// fields on /admin/api/accounts, that no secret leaks, and that an account with
// no pinned profile reports hasPinnedProfile:false without error.
func TestApiGetAccountsExposesCurrentProfile(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "with-profile", Enabled: true, Region: "us-east-1", ApiRegion: "eu-central-1", ProfileArn: "arn:aws:codewhisperer:eu-central-1:123:profile/ABC123", AccessToken: "secret-token", RefreshToken: "secret-refresh", AuthMethod: "external_idp"})
	_ = config.AddAccount(config.Account{ID: "no-profile", Enabled: true, Region: "us-east-1", AccessToken: "secret-token2"})
	p := accountpool.GetPool()
	p.Reload()
	h := newTestHandler(p)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	h.apiGetAccounts(rec, r)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	for _, secret := range []string{"secret-token", "secret-refresh", "accessToken", "refreshToken"} {
		if strings.Contains(body, secret) {
			t.Errorf("/accounts leaked %q: %s", secret, body)
		}
	}

	var accounts []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := map[string]map[string]interface{}{}
	for _, a := range accounts {
		byID[a["id"].(string)] = a
	}

	wp := byID["with-profile"]
	if wp["hasPinnedProfile"] != true {
		t.Errorf("with-profile should have hasPinnedProfile=true, got %v", wp["hasPinnedProfile"])
	}
	if wp["currentProfileArn"] != "arn:aws:codewhisperer:eu-central-1:123:profile/ABC123" {
		t.Errorf("unexpected currentProfileArn: %v", wp["currentProfileArn"])
	}
	if wp["currentProfileRegion"] != "eu-central-1" {
		t.Errorf("currentProfileRegion should be the profile/API region eu-central-1, got %v", wp["currentProfileRegion"])
	}
	if wp["currentProfileLabel"] != "ABC123" {
		t.Errorf("currentProfileLabel should be the shortARN ABC123, got %v", wp["currentProfileLabel"])
	}

	np := byID["no-profile"]
	if np["hasPinnedProfile"] != false {
		t.Errorf("no-profile should have hasPinnedProfile=false, got %v", np["hasPinnedProfile"])
	}
	if np["currentProfileArn"] != "" {
		t.Errorf("no-profile currentProfileArn should be empty, got %v", np["currentProfileArn"])
	}
}
