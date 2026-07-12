package proxy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// withStubProfileLister swaps profileLister for the duration of a test.
func withStubProfileLister(t *testing.T, fn func(ctx context.Context, acc *config.Account, region string) ([]DiscoveredProfile, error)) {
	t.Helper()
	old := profileLister
	profileLister = fn
	t.Cleanup(func() { profileLister = old })
}

// withStubModelLister swaps modelLister for the duration of a test, so
// SelectProfile's candidate-model prefetch does not hit the network.
func withStubModelLister(t *testing.T, fn func(acc *config.Account) ([]ModelInfo, error)) {
	t.Helper()
	old := modelLister
	modelLister = fn
	t.Cleanup(func() { modelLister = old })
}

func discoveryTestAccount() *config.Account {
	return &config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", AuthMethod: "external_idp"}
}

// newTestHandler builds a Handler with the model-cache maps initialized and a
// no-op token manager, matching what NewHandler wires up in production.
func newTestHandler(p *accountpool.AccountPool) *Handler {
	h := &Handler{
		pool:               p,
		modelInfoByAccount: make(map[string][]ModelInfo),
		profileSwitchLocks: make(map[string]*sync.Mutex),
	}
	h.tokenManager = NewTokenManager(p, func(*config.Account) (string, string, int64, string, error) {
		return "t", "r", 0, "", nil
	}, func(string, string, string, int64) error { return nil })
	return h
}

func TestDiscoverProfilesSingleRegion(t *testing.T) {
	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "us-east-1" {
			return []DiscoveredProfile{{ARN: "arn:a", Region: region, DisplayName: "A"}}, nil
		}
		return nil, fmt.Errorf("empty profile list")
	})
	res, err := DiscoverProfiles(context.Background(), discoveryTestAccount())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Profiles) != 1 || res.Profiles[0].ARN != "arn:a" {
		t.Fatalf("expected 1 profile arn:a, got %+v", res.Profiles)
	}
}

func TestDiscoverProfilesMultiRegionDedupAndSort(t *testing.T) {
	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		switch region {
		case "us-east-1":
			return []DiscoveredProfile{
				{ARN: "arn:z", Region: region, DisplayName: "Z"},
				{ARN: "arn:a", Region: region, DisplayName: "A"},
			}, nil
		case "eu-central-1":
			return []DiscoveredProfile{
				{ARN: "arn:a", Region: region, DisplayName: "A-dup"}, // duplicate ARN across regions
				{ARN: "arn:m", Region: region, DisplayName: "M"},
			}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	res, err := DiscoverProfiles(context.Background(), discoveryTestAccount())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// arn:a deduped (first occurrence in us-east-1 wins); result sorted by region then ARN.
	arns := make([]string, 0)
	for _, p := range res.Profiles {
		arns = append(arns, p.Region+"/"+p.ARN)
	}
	// us-east-1 (arn:a, arn:z) then eu-central-1 (arn:m). arn:a's region stays us-east-1.
	want := []string{"us-east-1/arn:a", "us-east-1/arn:z", "eu-central-1/arn:m"}
	if len(arns) != len(want) {
		t.Fatalf("expected %v, got %v", want, arns)
	}
	// Verify no duplicate ARNs.
	seen := map[string]bool{}
	for _, p := range res.Profiles {
		if seen[p.ARN] {
			t.Fatalf("duplicate ARN %s in result", p.ARN)
		}
		seen[p.ARN] = true
	}
}

func TestDiscoverProfilesPartialSuccess(t *testing.T) {
	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "us-east-1" {
			return nil, fmt.Errorf("HTTP 500: boom")
		}
		return []DiscoveredProfile{{ARN: "arn:eu", Region: region, DisplayName: "EU"}}, nil
	})
	res, err := DiscoverProfiles(context.Background(), discoveryTestAccount())
	if err != nil {
		t.Fatalf("partial success should not error: %v", err)
	}
	if len(res.Profiles) != 1 || res.Profiles[0].ARN != "arn:eu" {
		t.Fatalf("expected the succeeding region's profile, got %+v", res.Profiles)
	}
	if res.RegionErrors["us-east-1"] == "" {
		t.Fatalf("expected a recorded region error for us-east-1")
	}
}

func TestDiscoverProfilesAllRegionsFail(t *testing.T) {
	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		return nil, fmt.Errorf("HTTP 403: denied")
	})
	res, err := DiscoverProfiles(context.Background(), discoveryTestAccount())
	if err == nil {
		t.Fatal("expected error when all regions fail")
	}
	if len(res.Profiles) != 0 {
		t.Fatalf("expected no profiles, got %+v", res.Profiles)
	}
	if len(res.RegionErrors) == 0 {
		t.Fatal("expected region errors to be recorded")
	}
}

func TestDiscoverProfilesContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	withStubProfileLister(t, func(_ context.Context, _ *config.Account, _ string) ([]DiscoveredProfile, error) {
		t.Fatal("lister should not be called after ctx cancel")
		return nil, nil
	})
	if _, err := DiscoverProfiles(ctx, discoveryTestAccount()); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSelectProfilePersistsAndReplacesModelCache(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", AccessToken: "t", AuthMethod: "external_idp"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList("acct-1", []string{"old-model"}) // seed an old-profile model list

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "eu-central-1" {
			return []DiscoveredProfile{{ARN: "arn:eu-target", Region: region, DisplayName: "EU"}}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	// The new profile serves a different model set.
	withStubModelLister(t, func(acc *config.Account) ([]ModelInfo, error) {
		if acc.ProfileArn != "arn:eu-target" || acc.ApiRegion != "eu-central-1" {
			t.Fatalf("candidate must carry target ARN/region, got %q/%q", acc.ProfileArn, acc.ApiRegion)
		}
		return []ModelInfo{{ModelId: "new-model-a"}, {ModelId: "new-model-b"}}, nil
	})

	h := newTestHandler(p)

	res, err := h.SelectProfile(context.Background(), "acct-1", "arn:eu-target", "eu-central-1")
	if err != nil {
		t.Fatalf("SelectProfile: %v", err)
	}
	if !res.ModelCacheRefreshed || res.ModelCount != 2 {
		t.Fatalf("expected refreshed cache with 2 models, got %+v", res)
	}

	// Persisted ARN + region.
	acc := p.GetByID("acct-1")
	if acc.ProfileArn != "arn:eu-target" {
		t.Fatalf("expected pinned ARN, got %q", acc.ProfileArn)
	}
	if acc.ApiRegion != "eu-central-1" {
		t.Fatalf("expected persisted region eu-central-1, got %q", acc.ApiRegion)
	}
	// Routing model list is REPLACED with the new profile's models (old-model gone).
	ml := p.GetModelList("acct-1")
	got := map[string]bool{}
	for _, m := range ml {
		got[m] = true
	}
	if got["old-model"] {
		t.Fatalf("old-profile model must be gone, got %v", ml)
	}
	if !got["new-model-a"] || !got["new-model-b"] {
		t.Fatalf("expected new profile models, got %v", ml)
	}
}

func TestSelectProfileRejectsVanishedProfile(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", AccessToken: "t"})
	p := accountpool.GetPool()
	p.Reload()
	before := p.GetByID("acct-1").ProfileArn

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, _ string) ([]DiscoveredProfile, error) {
		return []DiscoveredProfile{{ARN: "arn:other", Region: "us-east-1"}}, nil
	})
	withStubModelLister(t, func(*config.Account) ([]ModelInfo, error) {
		t.Fatal("model prefetch must not run when the target profile is absent")
		return nil, nil
	})
	h := newTestHandler(p)

	_, err := h.SelectProfile(context.Background(), "acct-1", "arn:gone", "us-east-1")
	if err == nil {
		t.Fatal("expected error when target profile is not present at region")
	}
	// Account unchanged.
	if got := p.GetByID("acct-1").ProfileArn; got != before {
		t.Fatalf("account must be unchanged on failed select, was %q now %q", before, got)
	}
}

// TestSelectProfileKeepsOldProfileWhenModelFetchFails proves prefetch-before-commit:
// if the new profile cannot serve models, the switch is refused and nothing changes.
func TestSelectProfileKeepsOldProfileWhenModelFetchFails(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", ProfileArn: "arn:old", AccessToken: "t"})
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList("acct-1", []string{"old-model"})

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "eu-central-1" {
			return []DiscoveredProfile{{ARN: "arn:eu-target", Region: region}}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	withStubModelLister(t, func(*config.Account) ([]ModelInfo, error) {
		return nil, fmt.Errorf("boom: new profile cannot list models")
	})
	h := newTestHandler(p)

	if _, err := h.SelectProfile(context.Background(), "acct-1", "arn:eu-target", "eu-central-1"); err == nil {
		t.Fatal("expected error when candidate model fetch fails")
	}
	acc := p.GetByID("acct-1")
	if acc.ProfileArn != "arn:old" || acc.EffectiveApiRegion() != "us-east-1" {
		t.Fatalf("old profile must be intact, got %q/%q", acc.ProfileArn, acc.EffectiveApiRegion())
	}
	if ml := p.GetModelList("acct-1"); len(ml) != 1 || ml[0] != "old-model" {
		t.Fatalf("old model list must be intact, got %v", ml)
	}
}

func TestDiscoverProfilesConcurrent(t *testing.T) {
	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		return []DiscoveredProfile{{ARN: "arn:" + region, Region: region}}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := DiscoverProfiles(context.Background(), discoveryTestAccount()); err != nil {
				t.Errorf("discover: %v", err)
			}
		}()
	}
	wg.Wait()
}
