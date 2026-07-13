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
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// TestPublishGatedAndSelectProfileShareSwitchLock is the regression guard for the
// shared per-account lock that closes the profile-switch/model-cache race (commit
// 2afe75d). It proves publishAccountModelsGated and SelectProfile serialize on the
// SAME per-account lock instance: while SelectProfile holds the lock (blocked in
// its candidate-model prefetch), a concurrent gated publish for the account cannot
// make progress. Once the switch commits and releases the lock, the gated publish
// runs but — having fetched against the OLD profile — is discarded, so the newer
// profile wins. If the two paths ever stop sharing one lock, the gated publish
// would run immediately and this test fails.
func TestPublishGatedAndSelectProfileShareSwitchLock(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", ProfileArn: "arn:old", AccessToken: "t", AuthMethod: "external_idp"})
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList("acct-1", []string{"old-model"})
	h := newTestHandler(p)
	h.modelCache.SetAccountModelInfo("acct-1", []ModelInfo{{ModelId: "old-model"}})

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "eu-central-1" {
			return []DiscoveredProfile{{ARN: "arn:new", Region: region}}, nil
		}
		return nil, fmt.Errorf("empty")
	})

	// The candidate-model prefetch blocks until released, so SelectProfile holds the
	// per-account switch lock for a controlled window.
	enteredFetch := make(chan struct{})
	releaseFetch := make(chan struct{})
	withStubModelListerCtx(t, func(_ context.Context, _ *config.Account) ([]ModelInfo, error) {
		close(enteredFetch)
		<-releaseFetch
		return []ModelInfo{{ModelId: "new-model"}}, nil
	})

	selDone := make(chan error, 1)
	go func() {
		_, err := h.SelectProfile(context.Background(), "acct-1", "arn:new", "eu-central-1")
		selDone <- err
	}()
	<-enteredFetch // SelectProfile now holds the per-account switch lock.

	// A gated publish for the OLD profile launched now must block on the SAME lock.
	published := make(chan bool, 1)
	go func() {
		published <- h.modelCache.PublishGated("acct-1", "arn:old", "us-east-1", []ModelInfo{{ModelId: "stale-old-model"}})
	}()

	select {
	case <-published:
		t.Fatal("publishAccountModelsGated ran while SelectProfile held the per-account lock — the two paths are not sharing one lock instance (race fix regressed)")
	case <-time.After(200 * time.Millisecond):
		// Still blocked: serialized on the shared lock, as required.
	}

	// Let SelectProfile commit the new profile and release the lock.
	close(releaseFetch)
	if err := <-selDone; err != nil {
		t.Fatalf("SelectProfile: %v", err)
	}

	// The gated publish now proceeds but fetched against the OLD profile → discarded.
	if got := <-published; got {
		t.Fatal("stale gated publish (old ARN) must be discarded after the switch won")
	}
	acc := p.GetByID("acct-1")
	if acc.ProfileArn != "arn:new" || acc.EffectiveApiRegion() != "eu-central-1" {
		t.Fatalf("newer profile must win, got %q/%q", acc.ProfileArn, acc.EffectiveApiRegion())
	}
	if ml := p.GetModelList("acct-1"); len(ml) != 1 || ml[0] != "new-model" {
		t.Fatalf("routing set must be the new profile's, got %v", ml)
	}
}

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
	h.modelCache.SetAccountModelInfo("acct-1", []ModelInfo{{ModelId: "old-only-model"}, {ModelId: "shared-model"}})
	h.modelCache.SetAccountModelInfo("acct-2", []ModelInfo{{ModelId: "shared-model"}})
	h.modelCache.RebuildAggregate()

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

	agg := map[string]bool{}
	for _, m := range h.modelCache.Snapshot() {
		agg[m.ModelId] = true
	}

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

// TestSelectProfileRejectsEmptyModelList proves Fix 4: a profile that returns
// zero models is not a usable target. Persisting it would report a false
// "refreshed" success AND leave the account with an empty routing set, which the
// pool reads as "cache not ready → optimistically allow every model" — silently
// mis-routing. The switch must be refused and the old profile kept intact.
func TestSelectProfileRejectsEmptyModelList(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", ProfileArn: "arn:old", AccessToken: "t", AuthMethod: "external_idp"})
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList("acct-1", []string{"old-model"})

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "eu-central-1" {
			return []DiscoveredProfile{{ARN: "arn:eu-target", Region: region}}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	// The new profile authorizes no models (empty, but NOT an error).
	withStubModelLister(t, func(*config.Account) ([]ModelInfo, error) {
		return []ModelInfo{}, nil
	})
	h := newTestHandler(p)

	res, err := h.SelectProfile(context.Background(), "acct-1", "arn:eu-target", "eu-central-1")
	if err == nil {
		t.Fatal("expected error when the selected profile returns zero models")
	}
	if res.ModelCacheRefreshed {
		t.Fatalf("must not report a refreshed cache for an empty model list, got %+v", res)
	}
	acc := p.GetByID("acct-1")
	if acc.ProfileArn != "arn:old" || acc.EffectiveApiRegion() != "us-east-1" {
		t.Fatalf("old profile must be intact, got %q/%q", acc.ProfileArn, acc.EffectiveApiRegion())
	}
	if ml := p.GetModelList("acct-1"); len(ml) != 1 || ml[0] != "old-model" {
		t.Fatalf("old model list must be intact (never emptied), got %v", ml)
	}
}

// TestSelectProfileHonorsCancelledContext proves Fix 5: if the admin request is
// cancelled before the switch commits, SelectProfile aborts and persists
// nothing, so a switch nobody is waiting for cannot land off a late response.
func TestSelectProfileHonorsCancelledContext(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", ProfileArn: "arn:old", AccessToken: "t", AuthMethod: "external_idp"})
	p := accountpool.GetPool()
	p.Reload()

	withStubProfileLister(t, func(_ context.Context, _ *config.Account, region string) ([]DiscoveredProfile, error) {
		if region == "eu-central-1" {
			return []DiscoveredProfile{{ARN: "arn:eu-target", Region: region}}, nil
		}
		return nil, fmt.Errorf("empty")
	})
	withStubModelLister(t, func(*config.Account) ([]ModelInfo, error) {
		return []ModelInfo{{ModelId: "new-model"}}, nil
	})
	h := newTestHandler(p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller went away before the switch could commit

	if _, err := h.SelectProfile(ctx, "acct-1", "arn:eu-target", "eu-central-1"); err == nil {
		t.Fatal("expected error for a cancelled context")
	}
	if got := p.GetByID("acct-1").ProfileArn; got != "arn:old" {
		t.Fatalf("cancelled switch must not persist, got %q", got)
	}
}

// TestPublishAccountModelsGatedDiscardsStaleRefresh proves Fix 2: a model
// refresh that started against the OLD profile must NOT clobber the model list
// after a concurrent SelectProfile has moved the account to a new profile. The
// gated publish re-checks the current ARN under the per-account switch lock and
// drops the stale result.
func TestPublishAccountModelsGatedDiscardsStaleRefresh(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "eu-central-1", ProfileArn: "arn:new", AccessToken: "t", AuthMethod: "external_idp"})
	p := accountpool.GetPool()
	p.Reload()
	// The account is already on the NEW profile with the new model list published.
	p.SetModelList("acct-1", []string{"new-model"})
	h := newTestHandler(p)
	h.modelCache.SetAccountModelInfo("acct-1", []ModelInfo{{ModelId: "new-model"}})

	// A background refresh that started earlier, against the OLD profile, now tries
	// to publish its stale result.
	published := h.modelCache.PublishGated("acct-1", "arn:old", "us-east-1", []ModelInfo{{ModelId: "stale-old-model"}})
	if published {
		t.Fatal("stale refresh (old ARN) must be discarded, not published")
	}
	if ml := p.GetModelList("acct-1"); len(ml) != 1 || ml[0] != "new-model" {
		t.Fatalf("model list must stay the new profile's, got %v", ml)
	}

	// A refresh that matches the CURRENT profile does publish.
	published = h.modelCache.PublishGated("acct-1", "arn:new", "eu-central-1", []ModelInfo{{ModelId: "new-model"}, {ModelId: "new-model-2"}})
	if !published {
		t.Fatal("a refresh matching the current profile must publish")
	}
	if ml := p.GetModelList("acct-1"); len(ml) != 2 {
		t.Fatalf("matching refresh should have republished 2 models, got %v", ml)
	}
}

// TestDropAccountModelsRemovesFromAggregate proves Fix 3: disabling/deleting an
// account eagerly removes its models from the global aggregate and the pool
// routing set, so /v1/models cannot keep serving a gone account's models and a
// later aggregate rebuild cannot resurrect them.
func TestDropAccountModelsRemovesFromAggregate(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddAccount(config.Account{ID: "acct-1", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", ProfileArn: "arn:1", AccessToken: "t", AuthMethod: "external_idp"})
	_ = config.AddAccount(config.Account{ID: "acct-2", Enabled: true, Region: "us-east-1", ApiRegion: "us-east-1", ProfileArn: "arn:2", AccessToken: "t", AuthMethod: "external_idp"})
	p := accountpool.GetPool()
	p.Reload()
	h := newTestHandler(p)
	h.modelCache.SetAccountModelInfo("acct-1", []ModelInfo{{ModelId: "only-1"}, {ModelId: "shared"}})
	h.modelCache.SetAccountModelInfo("acct-2", []ModelInfo{{ModelId: "shared"}})
	p.SetModelList("acct-1", []string{"only-1", "shared"})
	p.SetModelList("acct-2", []string{"shared"})
	h.modelCache.RebuildAggregate()

	h.modelCache.DropAccount("acct-1")

	agg := map[string]bool{}
	for _, m := range h.modelCache.Snapshot() {
		agg[m.ModelId] = true
	}
	if agg["only-1"] {
		t.Fatalf("dropped account's exclusive model must leave the aggregate, got %v", agg)
	}
	if !agg["shared"] {
		t.Fatalf("a model still offered by acct-2 must remain, got %v", agg)
	}
	if ml := p.GetModelList("acct-1"); len(ml) != 0 {
		t.Fatalf("dropped account's routing set must be cleared, got %v", ml)
	}

	// A rebuild after the drop must not resurrect the removed account's models.
	h.modelCache.RebuildAggregate()
	for _, m := range h.modelCache.Snapshot() {
		if m.ModelId == "only-1" {
			t.Fatal("rebuild resurrected a dropped account's model")
		}
	}
}

// TestPruneStaleAccountModelsDropsDisabled proves the backstop prune inside
// refreshModelsCache: metadata for accounts absent from the keep set is removed
// and the aggregate rebuilt, so a disabled account that never got an explicit
// drop still leaves /v1/models on the next full refresh.
func TestPruneStaleAccountModelsDropsDisabled(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := newTestHandler(p)
	h.modelCache.SetAccountModelInfo("live", []ModelInfo{{ModelId: "live-model"}})
	h.modelCache.SetAccountModelInfo("gone", []ModelInfo{{ModelId: "gone-model"}})
	p.SetModelList("gone", []string{"gone-model"})
	h.modelCache.RebuildAggregate()

	h.modelCache.PruneStale(map[string]bool{"live": true})

	agg := map[string]bool{}
	for _, m := range h.modelCache.Snapshot() {
		agg[m.ModelId] = true
	}
	if agg["gone-model"] {
		t.Fatalf("pruned account's model must be gone, got %v", agg)
	}
	if !agg["live-model"] {
		t.Fatalf("kept account's model must remain, got %v", agg)
	}
	if ml := p.GetModelList("gone"); len(ml) != 0 {
		t.Fatalf("pruned account's routing set must be cleared, got %v", ml)
	}
}
