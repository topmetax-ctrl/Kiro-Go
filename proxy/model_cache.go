package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
)

// modelListerFunc is the seam for fetching an account's available models. It is
// context-aware so a cancelled admin op aborts the in-flight fetch. Injected into
// ModelCache so tests can supply a fake without hitting the network; production
// uses the real Kiro ListAvailableModels call.
type modelListerFunc func(ctx context.Context, account *config.Account) ([]ModelInfo, error)

// ModelCache owns the model-routing cache concern extracted from the Handler
// god-object: the global aggregate served by /v1/models, the per-account model
// metadata used to rebuild that aggregate in-memory (no network) when one
// account's profile changes, and the locking that keeps both consistent.
//
// It deliberately does NOT own the per-account profile-switch lock. That lock is
// SHARED with Handler.SelectProfile (profile_discovery.go) to close a
// profile-switch/model-cache race: a background/manual model refresh that started
// against the old profile must not clobber a concurrent switch to a new profile.
// The lock is injected via switchLock and the documented lock order is
// switchLock → mu, matching SelectProfile, so the two paths cannot deadlock.
type ModelCache struct {
	pool *pool.AccountPool

	// listModels fetches one account's available models (injected seam).
	listModels modelListerFunc
	// ensureToken makes sure an account has a valid token before a fetch
	// (= Handler.ensureValidToken).
	ensureToken func(*config.Account) error
	// onFailure classifies/records a per-account failure (= Handler.handleAccountFailure).
	onFailure func(*config.Account, error)
	// lookupAdmin resolves an account for an admin/setup op, pool→config
	// (= Handler.lookupAccountForAdmin).
	lookupAdmin func(string) *config.Account
	// switchLock returns the SHARED per-account profile-switch mutex
	// (= Handler.profileSwitchLock). Owned by Handler; injected, never owned here.
	switchLock func(string) *sync.Mutex

	// cachedModels is the global /v1/models aggregate. Guarded by mu.
	cachedModels []ModelInfo
	// cacheTime is the unix time cachedModels was last rebuilt. Guarded by mu.
	cacheTime int64
	// modelInfoByAccount keeps the last-known model metadata per account so the
	// global aggregate can be rebuilt in-memory (no network) when one account's
	// profile changes — dropping models no account offers anymore while keeping
	// other accounts' models. Guarded by mu.
	modelInfoByAccount map[string][]ModelInfo
	mu                 sync.RWMutex
}

// NewModelCache builds a ModelCache over the pool with the given injected
// dependencies. Pass nil for listModels to use the production default
// (ListAvailableModelsContext). The other dependencies are required.
func NewModelCache(
	p *pool.AccountPool,
	listModels modelListerFunc,
	ensureToken func(*config.Account) error,
	onFailure func(*config.Account, error),
	lookupAdmin func(string) *config.Account,
	switchLock func(string) *sync.Mutex,
) *ModelCache {
	if listModels == nil {
		listModels = func(ctx context.Context, account *config.Account) ([]ModelInfo, error) {
			return ListAvailableModelsContext(ctx, account)
		}
	}
	return &ModelCache{
		pool:               p,
		listModels:         listModels,
		ensureToken:        ensureToken,
		onFailure:          onFailure,
		lookupAdmin:        lookupAdmin,
		switchLock:         switchLock,
		modelInfoByAccount: make(map[string][]ModelInfo),
	}
}

// ListModels fetches an account's available models through the injected seam.
// Exposed so Handler paths (SelectProfile's candidate prefetch, apiGetAccountModels)
// share the same context-aware seam tests can stub.
func (mc *ModelCache) ListModels(ctx context.Context, account *config.Account) ([]ModelInfo, error) {
	return mc.listModels(ctx, account)
}

// Snapshot returns the current global aggregate. The slice is replaced wholesale
// on every rebuild (never mutated in place), so callers may read it after the
// RLock is dropped without racing a concurrent rebuild.
func (mc *ModelCache) Snapshot() []ModelInfo {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.cachedModels
}

// RefreshAll 从 Kiro API 拉取所有已启用账号的模型列表并缓存。
func (mc *ModelCache) RefreshAll() {
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}

	ctx := context.Background()
	enabled := make(map[string]bool, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		enabled[account.ID] = true
		if err := mc.ensureToken(account); err != nil {
			logger.Warnf("[ModelsCache] Skip %s token refresh failed: %v", account.Email, err)
			mc.onFailure(account, err)
			continue
		}

		models, err := mc.listModels(ctx, account)
		if err != nil {
			logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", account.Email, err)
			mc.onFailure(account, err)
			continue
		}
		// Publish through the gated path: a profile switch that raced this sweep
		// must win, so the fetch (done against the account's profile at read time)
		// is only committed if that profile is still current.
		mc.PublishGated(account.ID, account.ProfileArn, account.EffectiveApiRegion(), models)
	}

	// Backstop prune: drop per-account metadata (and pool routing sets) for
	// accounts that are no longer enabled, so the global aggregate cannot keep
	// serving a disabled/deleted account's models.
	mc.PruneStale(enabled)
}

// PublishGated publishes a freshly-fetched model list to the pool routing set and
// the per-account metadata cache, but ONLY if the account still points at the
// profile the models were fetched for. It serializes against profile switches via
// the SHARED per-account switch lock and re-checks the current ARN/region under
// it, so a background or admin refresh that started before a concurrent
// SelectProfile cannot clobber the newer profile's model list with a stale result.
// Returns true when the models were published.
//
// Lock order is switchLock → mu, matching SelectProfile, so the two paths cannot
// deadlock.
func (mc *ModelCache) PublishGated(accountID, fetchedForARN, fetchedForRegion string, models []ModelInfo) bool {
	lock := mc.switchLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	cur := mc.lookupAdmin(accountID)
	if cur == nil {
		return false // account deleted while the fetch was in flight
	}
	if strings.TrimSpace(cur.ProfileArn) != strings.TrimSpace(fetchedForARN) ||
		cur.EffectiveApiRegion() != fetchedForRegion {
		return false // profile switched under us — discard the stale fetch
	}

	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	mc.pool.SetModelList(accountID, modelIDs)

	cp := make([]ModelInfo, len(models))
	copy(cp, models)
	mc.mu.Lock()
	if mc.modelInfoByAccount == nil {
		mc.modelInfoByAccount = make(map[string][]ModelInfo)
	}
	mc.modelInfoByAccount[accountID] = cp
	mc.rebuildAggregateLocked()
	mc.mu.Unlock()
	return true
}

// PublishSwitch publishes a profile switch's new model set for one account. It
// ASSUMES THE CALLER ALREADY HOLDS the per-account switch lock (SelectProfile does,
// for the whole switch) and therefore only takes mu here — it must NOT re-acquire
// the switch lock, which would deadlock. It atomically republishes the account
// snapshot and routing model list via the pool, then rebuilds the aggregate
// in-memory so stale old-profile models drop out while other accounts' models stay.
func (mc *ModelCache) PublishSwitch(accountID string, models []ModelInfo) {
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	// Publish the new account snapshot AND the new routing model list under a
	// single pool lock, so no reader can observe the new profile paired with the
	// old profile's model set (the cutover is atomic).
	mc.pool.PublishProfileSwitch(accountID, modelIDs)

	cp := make([]ModelInfo, len(models))
	copy(cp, models)
	mc.mu.Lock()
	if mc.modelInfoByAccount == nil {
		mc.modelInfoByAccount = make(map[string][]ModelInfo)
	}
	mc.modelInfoByAccount[accountID] = cp
	mc.rebuildAggregateLocked()
	mc.mu.Unlock()
}

// PruneStale drops per-account model metadata and pool routing sets for accounts
// not present in keep (disabled, deleted, or otherwise gone), then rebuilds the
// aggregate so /v1/models cannot resurrect a removed account's models. keep is the
// set of account IDs that should retain their models.
func (mc *ModelCache) PruneStale(keep map[string]bool) {
	mc.mu.Lock()
	var dropped []string
	for id := range mc.modelInfoByAccount {
		if !keep[id] {
			delete(mc.modelInfoByAccount, id)
			dropped = append(dropped, id)
		}
	}
	mc.rebuildAggregateLocked()
	mc.mu.Unlock()
	for _, id := range dropped {
		mc.pool.DeleteModelList(id)
	}
}

// DropAccount eagerly removes one account's model metadata and pool routing set
// (called when an account is disabled or deleted) and rebuilds the aggregate so
// its models leave /v1/models immediately, without waiting for the next full
// refresh. Safe to call for an account that has no cached models.
func (mc *ModelCache) DropAccount(accountID string) {
	mc.mu.Lock()
	_, had := mc.modelInfoByAccount[accountID]
	delete(mc.modelInfoByAccount, accountID)
	if had {
		mc.rebuildAggregateLocked()
	}
	mc.mu.Unlock()
	mc.pool.DeleteModelList(accountID)
}

// SetAccountModelInfo records the last-known model metadata for an account so the
// global aggregate can later be rebuilt in memory. Guarded by mu. It does NOT
// rebuild the aggregate — callers that need the aggregate updated call
// RebuildAggregate afterward.
func (mc *ModelCache) SetAccountModelInfo(accountID string, models []ModelInfo) {
	cp := make([]ModelInfo, len(models))
	copy(cp, models)
	mc.mu.Lock()
	if mc.modelInfoByAccount == nil {
		mc.modelInfoByAccount = make(map[string][]ModelInfo)
	}
	mc.modelInfoByAccount[accountID] = cp
	mc.mu.Unlock()
}

// RebuildAggregate recomputes the global aggregate from the per-account metadata,
// taking mu. Convenience for callers (and tests) that hold no lock.
func (mc *ModelCache) RebuildAggregate() {
	mc.mu.Lock()
	mc.rebuildAggregateLocked()
	mc.mu.Unlock()
}

// rebuildAggregateLocked recomputes cachedModels from the per-account metadata, so
// models that no remaining account offers drop out while every other account's
// models are preserved. Caller must hold mu. It performs no network I/O.
func (mc *ModelCache) rebuildAggregateLocked() {
	aggregated := make([]ModelInfo, 0)
	for _, models := range mc.modelInfoByAccount {
		aggregated = mergeUniqueModels(aggregated, models)
	}
	mc.cachedModels = aggregated
	mc.cacheTime = time.Now().Unix()
}

// FetchAndCache 为单个账号拉取并写入模型缓存。同时更新 pool 的路由缓存与全局聚合模型
// 列表。The publish is gated on the account still pointing at the profile the models
// were fetched for, so a manual or background refresh cannot clobber a concurrent
// profile switch.
func (mc *ModelCache) FetchAndCache(account *config.Account) error {
	if err := mc.ensureToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := mc.listModels(context.Background(), account)
	if err != nil {
		return err
	}
	if mc.PublishGated(account.ID, account.ProfileArn, account.EffectiveApiRegion(), models) {
		logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
	} else {
		logger.Infof("[ModelsCache] Discarded stale model refresh for account %s (profile changed)", account.Email)
	}
	return nil
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// 立即为指定账号拉取并更新模型路由缓存。
func (mc *ModelCache) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// 从 pool 取运行时最新 token（与 RefreshAll 逻辑一致）
	if latest := mc.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	if err := mc.FetchAndCache(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(mc.pool.GetModelList(id)),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// 直接复用 RefreshAll，为所有已启用账号刷新模型路由缓存。
func (mc *ModelCache) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	mc.RefreshAll()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": len(mc.Snapshot()),
		"failed":    0,
	})
}

// --- Pure model-shaping helpers (no receiver) ------------------------------

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	for _, m := range cached {
		supportsImage := modelSupportsImage(m.InputTypes)
		models = append(models, buildModelInfo(m.ModelId, "anthropic", supportsImage))
		// 自动生成 thinking 变体
		models = append(models, buildModelInfo(m.ModelId+thinkingSuffix, "anthropic", supportsImage))
	}
	return models
}

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
		buildModelInfo("claude-sonnet-4.6", "anthropic", true),
		buildModelInfo("claude-sonnet-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.6", "anthropic", true),
		buildModelInfo("claude-opus-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.7", "anthropic", true),
		buildModelInfo("claude-opus-4.7"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.5", "anthropic", true),
		buildModelInfo("claude-sonnet-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4", "anthropic", true),
		buildModelInfo("claude-sonnet-4"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-haiku-4.5", "anthropic", true),
		buildModelInfo("claude-haiku-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.5", "anthropic", true),
		buildModelInfo("claude-opus-4.5"+thinkingSuffix, "anthropic", true),
	}
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	return map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
			},
		},
	}
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}
