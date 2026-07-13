package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"kiro-go/config"
	"kiro-go/logger"
)

// DiscoveredProfile is one CodeWhisperer/Kiro profile found during discovery. It
// carries no secrets — only the ARN, its region, and a display name.
type DiscoveredProfile struct {
	ARN         string `json:"arn"`
	Region      string `json:"region"`
	DisplayName string `json:"displayName"`
}

// ProfileDiscoveryResult is the outcome of probing an account across regions.
// Profiles is deduplicated by ARN and stable-sorted. RegionErrors records which
// regions failed (partial success is allowed: some regions can fail while others
// succeed). Err is set only when NO region yielded any profile.
type ProfileDiscoveryResult struct {
	Profiles     []DiscoveredProfile `json:"profiles"`
	RegionErrors map[string]string   `json:"regionErrors,omitempty"`
}

// profileLister is the seam for listing profiles in a region; overridable in tests.
// It returns the raw profiles for one region.
var profileLister = listProfilesInRegion

// DiscoverProfiles probes every candidate region for the account and returns all
// profiles found, deduplicated by ARN and stable-sorted (region, then ARN) so the
// UI ordering is stable. It honors ctx cancellation. It does NOT refresh tokens
// itself — the caller must ensure a fresh token via the TokenManager first.
//
// Partial success: if some regions fail but at least one yields profiles, the
// profiles are returned along with per-region errors. If all regions fail (or none
// yield a profile), the result has no profiles and a non-nil error.
func DiscoverProfiles(ctx context.Context, account *config.Account) (ProfileDiscoveryResult, error) {
	if account == nil {
		return ProfileDiscoveryResult{}, fmt.Errorf("nil account")
	}

	regions := profileProbeRegions(account)
	res := ProfileDiscoveryResult{RegionErrors: map[string]string{}}
	seen := map[string]bool{}

	for _, region := range regions {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		profiles, err := profileLister(ctx, account, region)
		if err != nil {
			res.RegionErrors[region] = err.Error()
			logger.Debugf("[ProfileDiscovery] region %s failed for %s: %v", region, accountEmailForLog(account), err)
			continue
		}
		for _, p := range profiles {
			arn := strings.TrimSpace(p.ARN)
			if arn == "" || seen[arn] {
				continue
			}
			seen[arn] = true
			if p.Region == "" {
				p.Region = region
			}
			res.Profiles = append(res.Profiles, p)
		}
	}

	// Stable sort: region asc, then ARN asc, so the picker list never reorders
	// between calls.
	sort.SliceStable(res.Profiles, func(i, j int) bool {
		if res.Profiles[i].Region != res.Profiles[j].Region {
			return res.Profiles[i].Region < res.Profiles[j].Region
		}
		return res.Profiles[i].ARN < res.Profiles[j].ARN
	})

	if len(res.Profiles) == 0 {
		if len(res.RegionErrors) > 0 {
			return res, fmt.Errorf("no profiles found; all %d region(s) failed", len(res.RegionErrors))
		}
		return res, fmt.Errorf("no profiles found for account")
	}
	if len(res.RegionErrors) == 0 {
		res.RegionErrors = nil
	}
	return res, nil
}

// listProfilesInRegion calls ListAvailableProfiles for one region and returns all
// profiles (not just the first). It reuses the account's REST client and headers.
func listProfilesInRegion(ctx context.Context, account *config.Account, region string) ([]DiscoveredProfile, error) {
	url := regionalizeURLForRegion(fmt.Sprintf("%s/ListAvailableProfiles", kiroRestAPIBase), region)
	// maxResults must match the value the proven-working listAvailableProfiles
	// uses: the CodeWhisperer API rejects larger values with HTTP 400
	// REQUEST_BODY_INVALID ("Improperly formed request").
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(`{"maxResults":10}`))
	if err != nil {
		return nil, err
	}
	setKiroHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")
	if account.AuthMethod == "external_idp" {
		req.Header.Set("TokenType", "EXTERNAL_IDP")
	}

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Profiles []struct {
			Arn         string `json:"arn"`
			ProfileName string `json:"profileName"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	out := make([]DiscoveredProfile, 0, len(result.Profiles))
	for _, p := range result.Profiles {
		arn := strings.TrimSpace(p.Arn)
		if arn == "" {
			continue
		}
		name := strings.TrimSpace(p.ProfileName)
		if name == "" {
			name = shortARN(arn)
		}
		out = append(out, DiscoveredProfile{ARN: arn, Region: region, DisplayName: name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty profile list")
	}
	return out, nil
}

// shortARN renders a compact display label from an ARN (last path segment), used
// when the upstream does not provide a profile name.
func shortARN(arn string) string {
	if i := strings.LastIndexAny(arn, "/:"); i >= 0 && i+1 < len(arn) {
		return arn[i+1:]
	}
	return arn
}

// PinnedProfile reports the account's currently pinned profile (ARN + region).
type PinnedProfile struct {
	ARN    string `json:"arn"`
	Region string `json:"region"`
}

// lookupAccountForAdmin resolves an account for an admin/setup operation. It
// prefers the live pool snapshot (fresh runtime stats) but falls back to the full
// config so disabled/banned accounts — which the pool excludes — are still
// addressable. Profile discovery/selection is an admin op, never on the hot path,
// so seeing a non-routable account here is intended.
func (h *Handler) lookupAccountForAdmin(accountID string) *config.Account {
	if acc := h.pool.GetByID(accountID); acc != nil {
		return acc
	}
	return config.GetAccountByID(accountID)
}

// GetPinnedProfile returns the account's currently active profile. It resolves
// via the pool first, then the full config, so a disabled/banned account's pinned
// profile is still reported. The request hot path reads the pool snapshot directly
// elsewhere and never calls this.
func (h *Handler) GetPinnedProfile(accountID string) (PinnedProfile, bool) {
	acc := h.lookupAccountForAdmin(accountID)
	if acc == nil {
		return PinnedProfile{}, false
	}
	return PinnedProfile{ARN: acc.ProfileArn, Region: acc.EffectiveApiRegion()}, true
}

// SelectProfileResult reports the outcome of a successful profile switch, so the
// admin API can tell the operator what actually happened (no false success).
type SelectProfileResult struct {
	ModelCacheRefreshed bool
	ModelCount          int
	Warnings            []string
}

// profileSwitchLock returns the per-account mutex used to serialize profile
// switches of the SAME account. Switches of different accounts get different
// locks and run in parallel. The tiny map guard (profileSwitchMu) is never held
// across I/O — only long enough to fetch/create the per-account lock.
func (h *Handler) profileSwitchLock(accountID string) *sync.Mutex {
	h.profileSwitchMu.Lock()
	defer h.profileSwitchMu.Unlock()
	if h.profileSwitchLocks == nil {
		h.profileSwitchLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := h.profileSwitchLocks[accountID]
	if !ok {
		mu = &sync.Mutex{}
		h.profileSwitchLocks[accountID] = mu
	}
	return mu
}

// SelectProfile switches an account to a specific discovered profile. It is an
// admin/setup operation, never on the request hot path. Ordering is
// prefetch-before-commit so a switch is atomic from the caller's point of view
// and never leaves a half-applied state:
//
//  1. validate inputs; resolve account (pool→config admin lookup)
//  2. ensure a fresh token via the TokenManager
//  3. re-discover and verify the target ARN still exists in that region
//  4. build a value-copy CANDIDATE snapshot (target ARN + region)
//  5. fetch the candidate profile's model list — OUTSIDE any cache lock
//     - on failure: do NOT persist; the old profile stays intact; return error
//  6. persist the new ARN + region atomically
//  7. publish the new snapshot to the pool
//  8. replace ONLY this account's routing model list with the candidate models
//  9. rebuild the global aggregate in-memory (drop models no account offers,
//     keep other accounts' models) — no network I/O under the lock
//
// The whole sequence is serialized per account so two concurrent switches of the
// same account cannot interleave; different accounts switch in parallel.
func (h *Handler) SelectProfile(ctx context.Context, accountID, profileARN, region string) (SelectProfileResult, error) {
	profileARN = strings.TrimSpace(profileARN)
	region = strings.TrimSpace(region)
	if accountID == "" || profileARN == "" || region == "" {
		return SelectProfileResult{}, fmt.Errorf("accountID, profileARN and region are required")
	}

	// Serialize switches of THIS account (different accounts stay parallel).
	lock := h.profileSwitchLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	acc := h.lookupAccountForAdmin(accountID)
	if acc == nil {
		return SelectProfileResult{}, fmt.Errorf("account %s not found", accountID)
	}

	// Ensure a valid token via the central manager (does not itself pin/select).
	if h.tokenManager != nil {
		if fresh, err := h.tokenManager.EnsureFresh(accountID); err == nil && fresh != nil {
			acc = fresh
		} else if err != nil {
			return SelectProfileResult{}, fmt.Errorf("token refresh failed: %w", err)
		}
	}

	// Verify the target ARN still exists at that region before committing.
	profiles, err := profileLister(ctx, acc, region)
	if err != nil {
		return SelectProfileResult{}, fmt.Errorf("verify profile in %s: %w", region, err)
	}
	found := false
	for _, p := range profiles {
		if p.ARN == profileARN {
			found = true
			break
		}
	}
	if !found {
		return SelectProfileResult{}, fmt.Errorf("profile %s not found in region %s", shortARN(profileARN), region)
	}

	// Build a value-copy candidate snapshot pointing at the target profile and
	// prefetch its model list BEFORE committing. If the new profile cannot serve
	// models, we refuse the switch and leave the old profile untouched. The fetch
	// is context-aware: if the caller (admin request) is cancelled mid-flight, the
	// HTTP call aborts and we never persist off a late response.
	candidate := *acc
	candidate.ProfileArn = profileARN
	candidate.ApiRegion = region
	models, err := h.modelCache.ListModels(ctx, &candidate)
	if err != nil {
		return SelectProfileResult{}, fmt.Errorf("fetch models for new profile: %w", err)
	}
	// A profile that serves zero models is not a usable switch target: persisting
	// it would report a false "refreshed" success, and an empty routing set is
	// read by the pool as "cache not ready → optimistically allow every model",
	// silently mis-routing. Refuse and keep the old profile.
	if len(models) == 0 {
		return SelectProfileResult{}, fmt.Errorf("selected profile %s returned no available models", shortARN(profileARN))
	}
	// The caller may have gone away while the fetch was in flight; do not commit a
	// switch nobody is waiting for.
	if err := ctx.Err(); err != nil {
		return SelectProfileResult{}, err
	}

	// Commit: persist ARN+region atomically.
	if err := config.UpdateAccountProfileArnWithRegion(accountID, profileARN, region); err != nil {
		return SelectProfileResult{}, fmt.Errorf("persist profile selection: %w", err)
	}

	// Publish the new account snapshot AND the new routing model list under a
	// single pool lock, so no reader can observe the new profile paired with the
	// old profile's model set (the cutover is atomic). Then rebuild the global
	// aggregate in-memory so stale old-profile models drop out while other
	// accounts' models stay — no network I/O under the cache lock.
	//
	// We ALREADY hold the per-account switch lock (acquired above and shared with
	// ModelCache.PublishGated), so PublishSwitch takes only the cache mutex and must
	// NOT re-acquire the switch lock — doing so would deadlock. This preserves the
	// documented lock order switchLock → cache mu across both paths.
	h.modelCache.PublishSwitch(accountID, models)

	logger.Infof("[ProfileDiscovery] account %s pinned to profile %s in %s (%d models)",
		accountID, shortARN(profileARN), region, len(models))
	return SelectProfileResult{ModelCacheRefreshed: true, ModelCount: len(models)}, nil
}
