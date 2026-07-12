package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

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

// GetPinnedProfile returns the account's currently active profile from its
// snapshot. This is a cheap pool read — the request hot path uses exactly this,
// never discovery.
func (h *Handler) GetPinnedProfile(accountID string) (PinnedProfile, bool) {
	acc := h.pool.GetByID(accountID)
	if acc == nil {
		return PinnedProfile{}, false
	}
	return PinnedProfile{ARN: acc.ProfileArn, Region: acc.EffectiveApiRegion()}, true
}

// SelectProfile switches an account to a specific discovered profile. It is an
// admin/setup operation, never on the request hot path. Ordering matters for
// safety:
//
//  1. validate inputs
//  2. ensure a fresh token via the TokenManager (no side refresh)
//  3. re-discover and verify the target ARN still exists in that region
//  4. persist the new profile ARN + region atomically
//  5. publish the new snapshot to the pool
//  6. invalidate ONLY this account's model cache (region-scoped model list)
//
// If any pre-commit step (1-3) fails, the account is left unchanged. If persistence
// fails, the account/config durability policy applies (atomic save; no partial
// write). The account is never left with a new ARN but a stale region/model cache.
func (h *Handler) SelectProfile(ctx context.Context, accountID, profileARN, region string) error {
	profileARN = strings.TrimSpace(profileARN)
	region = strings.TrimSpace(region)
	if accountID == "" || profileARN == "" || region == "" {
		return fmt.Errorf("accountID, profileARN and region are required")
	}

	acc := h.pool.GetByID(accountID)
	if acc == nil {
		return fmt.Errorf("account %s not found", accountID)
	}

	// Ensure a valid token via the central manager (does not itself pin/select).
	if h.tokenManager != nil {
		if fresh, err := h.tokenManager.EnsureFresh(accountID); err == nil && fresh != nil {
			acc = fresh
		} else if err != nil {
			return fmt.Errorf("token refresh failed: %w", err)
		}
	}

	// Verify the target ARN still exists at that region before committing.
	profiles, err := profileLister(ctx, acc, region)
	if err != nil {
		return fmt.Errorf("verify profile in %s: %w", region, err)
	}
	found := false
	for _, p := range profiles {
		if p.ARN == profileARN {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("profile %s not found in region %s", shortARN(profileARN), region)
	}

	// Persist atomically (config.Save is atomic). This updates ProfileArn + ApiRegion.
	if err := config.UpdateAccountProfileArnWithRegion(accountID, profileARN, region); err != nil {
		return fmt.Errorf("persist profile selection: %w", err)
	}

	// Publish the new snapshot to the pool (Reload rebuilds from config).
	h.pool.Reload()

	// Invalidate ONLY this account's model list (model entitlements are
	// region/profile-scoped). Do not flush any global model cache.
	h.pool.SetModelList(accountID, nil)

	logger.Infof("[ProfileDiscovery] account %s pinned to profile %s in %s",
		accountID, shortARN(profileARN), region)
	return nil
}
