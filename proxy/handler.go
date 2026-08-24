package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/apikey"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/metrics"
	"kiro-go/pool"
	"kiro-go/providererr"
	"kiro-go/search"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120

const (
	microsoftProfileSelectionTTL          = 10 * time.Minute
	microsoftMaxPendingProfileSelections  = 64
	microsoftCanceledSessionTTL           = 10 * time.Minute
	microsoftMaxCanceledSessionTombstones = 128
	microsoftProfileDiscoveryTimeout      = 30 * time.Second
)

// looksLikeKiroAPIKey is a lightweight heuristic for plain-text imports.
// Official keys currently use the ksk_ prefix; future formats can still be
// imported via explicit authMethod/kiroApiKey fields.
func looksLikeKiroAPIKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	// Convenience form: ksk_xxx|region
	if idx := strings.IndexByte(value, '|'); idx > 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return strings.HasPrefix(value, "ksk_")
}

// RequestLog stores details about a single API request (success or failure).
type RequestLog struct {
	Time      int64   `json:"time"`      // Unix timestamp
	Endpoint  string  `json:"endpoint"`  // claude/openai/responses
	Model     string  `json:"model"`     // Requested model
	AccountID string  `json:"accountId"` // Account used
	Status    string  `json:"status"`    // "success" or "error"
	Error     string  `json:"error"`     // Error message (empty on success)
	ErrorType string  `json:"errorType"` // Error category (empty on success)
	Tokens    int     `json:"tokens"`    // Total tokens (input+output, 0 on failure)
	Credits   float64 `json:"credits"`   // Credits consumed (0 on failure)
	Duration  int64   `json:"duration"`  // Request duration in ms
}

const requestLogsMaxSize = 500

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests    int64
	successRequests  int64
	failedRequests   int64
	totalTokens      int64
	totalCredits     float64 // float64 需要用锁保护
	creditsMu        sync.RWMutex
	startTime        int64
	stopRefresh      chan struct{}
	stopStatsSaver   chan struct{}
	stopKeyRetention chan struct{}
	shutdownOnce     sync.Once
	// keys is the durable API-key store. Nil in unit tests that construct
	// &Handler{} without NewHandler — those keep the config.json fallback.
	keys *apikey.Service
	// modelCache owns the model-routing cache concern (the /v1/models aggregate,
	// per-account model metadata, and their locking). Extracted from this
	// god-object; see proxy/model_cache.go. Upstream's cachedModels/modelsCacheMu/
	// modelsCacheTime are the pre-extraction form of this and are deliberately
	// NOT carried over — two competing caches would diverge.
	modelCache   *ModelCache
	promptCache  *promptCacheTracker
	tokenManager *TokenManager
	// profileSwitchLocks serializes profile switches per account so two concurrent
	// switches of the SAME account cannot interleave persist/publish. Switches of
	// DIFFERENT accounts still run in parallel (no global lock on the hot path).
	profileSwitchMu    sync.Mutex
	profileSwitchLocks map[string]*sync.Mutex
	// conversationRunner orchestrates multi-round Kiro calls with server-side
	// web_search execution. Injected so tests can supply fakes.
	conversationRunner ConversationRunner
	// memory is the long-term memory provider. It is a noopMemoryProvider when
	// the feature is disabled (never nil), and is rebuilt on config change via
	// rebuildMemoryProvider — mirroring the account pool's reload-on-change model.
	// Guarded by memoryMu so the admin update path can swap it while requests read it.
	memory             MemoryProvider
	memoryMu           sync.RWMutex
	tokenRefreshMu     sync.Mutex
	credentialImportMu sync.Mutex
	// 请求日志 (环形缓冲区，包含成功和失败)
	requestLogs   []RequestLog
	requestLogsMu sync.RWMutex

	microsoftSelections   map[string]*microsoftProfileSelection
	microsoftSelectionsMu sync.Mutex
	microsoftFlowMu       sync.Mutex
	microsoftCanceled     map[string]time.Time
	microsoftDiscoveries  map[string]*microsoftProfileDiscovery
}

type microsoftProfileSelection struct {
	SessionID string
	Account   config.Account
	Profiles  []KiroProfile
	ExpiresAt time.Time
	timer     *time.Timer
	mu        sync.Mutex
	canceled  atomic.Bool
}

type microsoftProfileDiscovery struct {
	cancel context.CancelFunc
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}
	if msg := validateClaudeOutputConfig(req.OutputConfig); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:                 pool.GetPool(),
		totalRequests:        int64(totalReq),
		successRequests:      int64(successReq),
		failedRequests:       int64(failedReq),
		totalTokens:          int64(totalTokens),
		totalCredits:         totalCredits,
		startTime:            time.Now().Unix(),
		stopRefresh:          make(chan struct{}),
		stopStatsSaver:       make(chan struct{}),
		stopKeyRetention:     make(chan struct{}),
		promptCache:          newPromptCacheTracker(defaultPromptCacheTTL),
		conversationRunner:   NewKiroConversationRunner(),
		profileSwitchLocks:   make(map[string]*sync.Mutex),
		memory:               newMemoryProviderFromConfig(),
		microsoftSelections:  make(map[string]*microsoftProfileSelection),
		microsoftCanceled:    make(map[string]time.Time),
		microsoftDiscoveries: make(map[string]*microsoftProfileDiscovery),
	}
	h.tokenManager = NewTokenManager(h.pool, nil, nil)
	// The model-routing cache borrows Handler's per-account profile-switch lock
	// (h.profileSwitchLock) — the SAME lock SelectProfile takes — so a gated model
	// refresh and a profile switch of one account serialize, closing the
	// profile-switch/model-cache race (commit 2afe75d). The lock stays owned by
	// Handler; ModelCache only borrows it via the injected accessor.
	h.modelCache = NewModelCache(
		h.pool,
		nil, // production default: ListAvailableModelsContext
		h.ensureValidToken,
		h.handleAccountFailure,
		h.lookupAccountForAdmin,
		h.profileSwitchLock,
	)
	// 恢复转发指标聚合数据 (事件日志与时序为内存态，不持久化)
	if err := metrics.Load(forwardMetricsPath()); err != nil {
		logger.Warnf("[Metrics] failed to load forward stats: %v", err)
	}
	// 启动后台刷新
	go h.backgroundRefresh()
	// 启动后台统计保存 (每30秒保存一次)
	go h.backgroundStatsSaver()
	h.attachKeyStore()
	// 清理过期的 stored responses（>30 天）
	go purgeExpiredResponses(responsesDefaultTTL)
	return h
}

func (h *Handler) attachKeyStore() {
	if !config.Initialized() {
		return
	}
	pepper, err := config.GetOrCreateAPIKeyPepper()
	if err != nil {
		logger.Fatalf("[ApiKey] pepper: %v", err)
	}
	path := filepath.Join(config.GetConfigDir(), "apikeys.db")
	retain := time.Duration(config.GetUsageRetentionDays()) * 24 * time.Hour
	svc, err := apikey.Open(path, pepper, apikey.Options{Retention: retain})
	if err != nil {
		logger.Fatalf("[ApiKey] open store: %v", err)
	}
	legacy := config.ListApiKeys()
	entries := make([]apikey.LegacyKey, len(legacy))
	for i, e := range legacy {
		entries[i] = apikey.LegacyKey{
			ID: e.ID, Name: e.Name, Key: e.Key, Enabled: e.Enabled, Migrated: e.Migrated,
			CreatedAt: e.CreatedAt, LastUsedAt: e.LastUsedAt, TokenLimit: e.TokenLimit,
			CreditLimit: e.CreditLimit, TokensUsed: e.TokensUsed, CreditsUsed: e.CreditsUsed,
			RequestsCount: e.RequestsCount,
		}
	}
	res, err := svc.ImportLegacy(entries)
	if err != nil {
		_ = svc.Close()
		logger.Fatalf("[ApiKey] legacy import: %v", err)
	}
	logger.Infof("[ApiKey] store ready; imported %d, already present %d, skipped %d", res.Imported, res.AlreadyPresent, res.Skipped)
	h.keys = svc
	go h.backgroundKeyRetention()
}

func (h *Handler) backgroundKeyRetention() {
	if h.keys == nil {
		return
	}
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			start := time.Now()
			ev, hr, err := h.keys.Cleanup()
			if err != nil {
				logger.Warnf("[ApiKey] retention cleanup failed: %v", err)
				continue
			}
			if ev > 0 || hr > 0 {
				logger.Infof("[ApiKey] retention removed %d events, %d hourly buckets in %s", ev, hr, time.Since(start))
			}
		case <-h.stopKeyRetention:
			return
		}
	}
}

// getMemory returns the current memory provider under a read lock. It is never
// nil (a noopMemoryProvider stands in when the feature is disabled), so callers
// can use the result without a nil check.
func (h *Handler) getMemory() MemoryProvider {
	h.memoryMu.RLock()
	defer h.memoryMu.RUnlock()
	return h.memory
}

// rebuildMemoryProvider swaps in a provider freshly built from the current
// config. Called after the admin memory config changes, mirroring how
// apiUpdateSettings calls h.pool.Reload() so the change takes effect at runtime
// without a restart.
func (h *Handler) rebuildMemoryProvider() {
	next := newMemoryProviderFromConfig()
	h.memoryMu.Lock()
	h.memory = next
	h.memoryMu.Unlock()
}

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Minute) // 每 30 分钟刷新一次
	defer ticker.Stop()

	// 启动时延迟 10 秒后执行一次
	time.Sleep(10 * time.Second)
	h.modelCache.RefreshAll()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.modelCache.RefreshAll()
			h.refreshAllAccounts()
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts 刷新所有账户信息
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	for i := range accounts {
		account := &accounts[i]
		if !account.Enabled {
			continue
		}
		if accountBearerToken(account) == "" {
			continue
		}

		// API Key accounts skip OAuth refresh; still sync usage/subscription.
		if !config.IsAPIKeyAccount(account) {
			// 检查 token 是否需要刷新。Route through the TokenManager so background
			// and foreground refreshes share the same per-account coordination,
			// validation, atomic persist, and pool publish.
			if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
				fresh, err := h.tokenManager.EnsureFresh(account.ID)
				if err != nil {
					logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
					h.handleAccountFailure(account, err)
					continue
				}
				if fresh != nil {
					account.AccessToken = fresh.AccessToken
					account.RefreshToken = fresh.RefreshToken
					account.ExpiresAt = fresh.ExpiresAt
					account.ProfileArn = fresh.ProfileArn
				}
			}
		}

		// 刷新账户信息
		info, err := RefreshAccountInfo(account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
			continue
		}

		config.UpdateAccountInfo(account.ID, *info)
		logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
	}
	h.pool.Reload()
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	r = withRequestIDContext(withClientIPContext(r, clientIP(r)), requestIDFromRequest(r))
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendClaudeError(wrapLeaseWriter(w, r.Context()), ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	r = withRequestIDContext(withClientIPContext(r, clientIP(r)), requestIDFromRequest(r))
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendOpenAIError(wrapLeaseWriter(w, r.Context()), ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// clientIP resolves the request's client IP. Forwarded-IP headers are only
// consulted when TrustProxy is enabled (otherwise a directly-exposed server
// would trust attacker-supplied headers).
//
// Header preference matters for correctness behind Cloudflare: a client can
// prepend its own X-Forwarded-For, and Cloudflare appends the real IP *after*
// it, so the left-most XFF token is spoofable. CF-Connecting-IP is set (and
// overwritten) by Cloudflare to the true client IP, so it is preferred when
// present. XFF/X-Real-IP remain the fallback for non-Cloudflare proxies.
func clientIP(r *http.Request) string {
	if config.GetTrustProxy() {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
			return cf
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
				return ip
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// adminIPAllowed reports whether the given client IP may reach /admin*. An empty
// allowlist allows everyone (back-compat). Loopback is always allowed so a typo'd
// allowlist can never lock the operator out of localhost.
func adminIPAllowed(ipStr string) bool {
	list := config.GetAdminAllowlist()
	if len(list) == 0 {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, e := range list {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if _, c, err := net.ParseCIDR(e); err == nil && c.Contains(ip) {
				return true
			}
		} else if p := net.ParseIP(e); p != nil && p.Equal(ip) {
			return true
		}
	}
	return false
}

// Ingress body-size caps by endpoint class. Applied centrally in ServeHTTP via
// http.MaxBytesReader so every downstream body read (io.ReadAll or
// json.Decoder) is bounded and returns 413 on overflow, without wrapping each of
// the ~30 read sites individually. Inference/messages payloads can be large
// (long conversations, base64 images); admin/config/import payloads are small.
const (
	maxInferenceBodyBytes = 32 << 20 // 32 MiB: /v1/messages, /chat/completions, /responses
	maxAdminBodyBytes     = 4 << 20  // 4 MiB: admin/config/import/token-count/other
)

// bodyLimitForPath returns the max request body size for a path's endpoint class.
func bodyLimitForPath(path string) int64 {
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses":
		return maxInferenceBodyBytes
	default:
		// Everything else that accepts a body (admin APIs, import, config,
		// count_tokens) is small by nature.
		return maxAdminBodyBytes
	}
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Bound the request body before any handler reads it. MaxBytesReader makes an
	// over-limit read fail (surfaced as 400/413 by the handlers' error paths) and
	// prevents an unbounded body from exhausting memory.
	if r.Body != nil && r.Method != "GET" && r.Method != "OPTIONS" {
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimitForPath(path))
	}

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, redactHTTPPath(path), r.RemoteAddr)

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		h.serveInference(w, r, "claude", h.authenticateForClaude, h.handleClaudeMessages)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		h.serveInference(w, r, "openai", h.authenticateForOpenAI, h.handleOpenAIChat)
	case path == "/v1/responses" || path == "/responses":
		h.serveInference(w, r, "responses", h.authenticateForOpenAI, h.handleOpenAIResponses)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点 (IP 白名单在密码校验之前生效)
	case path == "/admin" || path == "/admin/" || strings.HasPrefix(path, "/admin/"):
		if !adminIPAllowed(clientIP(r)) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		switch {
		case path == "/admin" || path == "/admin/":
			h.serveAdminPage(w, r)
		case strings.HasPrefix(path, "/admin/api/"):
			h.handleAdminAPI(w, r)
		default:
			h.serveStaticFile(w, r)
		}

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	case strings.HasPrefix(path, "/portal/"):
		h.handlePortal(w, r)
	case path == "/usage" || path == "/usage/" || strings.HasPrefix(path, "/usage/"):
		h.handleUsagePage(w, r)
	case strings.HasPrefix(path, "/locales/"):
		rel := strings.TrimPrefix(path, "/locales/")
		if rel == "" || strings.Contains(rel, "..") {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "web/locales/"+rel)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	}
	// Additive, backward-compatible prompt-cache observability. Counts only — no
	// prompt text, fingerprints, or token secrets.
	if h.promptCache != nil {
		m, entries, capacity := h.promptCache.Metrics()
		resp["promptCache"] = map[string]interface{}{
			"hits":           m.Hits,
			"misses":         m.Misses,
			"creations":      m.Creations,
			"evictions":      m.Evictions,
			"expiredEvicted": m.ExpiredEvict,
			"currentEntries": entries,
			"capacity":       capacity,
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

// handleModels lists the public model catalog. The operator picks the source
// in Settings: forwarding routes only, the Kiro account catalog, or both.
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	var models []map[string]interface{}
	switch config.GetPublicModelCatalog() {
	case config.PublicModelCatalogKiro:
		models = h.kiroCatalogModels()
	case config.PublicModelCatalogBoth:
		models = mergeModelListings(forwardingCatalogModels(), h.kiroCatalogModels())
	default:
		models = forwardingCatalogModels()
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

func forwardingCatalogModels() []map[string]interface{} {
	names := config.AdvertisedRouteModels()
	models := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		models = append(models, buildModelInfo(name, "kiro-proxy", true))
	}
	return models
}

func (h *Handler) kiroCatalogModels() []map[string]interface{} {
	var cached []ModelInfo
	if h.modelCache != nil {
		cached = h.modelCache.Snapshot()
		if len(cached) == 0 {
			h.modelCache.RefreshAll()
			cached = h.modelCache.Snapshot()
		}
	}
	thinkingCfg := config.GetThinkingConfig()
	models := buildAnthropicModelsResponse(cached, thinkingCfg.Suffix, thinkingCfg.AdvertiseEffortModels)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingCfg.Suffix, thinkingCfg.AdvertiseEffortModels)
	}
	return append(models,
		buildModelInfo("auto", "kiro-proxy", true),
		buildModelInfo("gpt-4o", "kiro-proxy", true),
		buildModelInfo("gpt-4", "kiro-proxy", true),
	)
}

func mergeModelListings(first, second []map[string]interface{}) []map[string]interface{} {
	seen := make(map[string]bool, len(first)+len(second))
	out := make([]map[string]interface{}, 0, len(first)+len(second))
	for _, list := range [][]map[string]interface{}{first, second} {
		for _, m := range list {
			id, _ := m["id"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, m)
		}
	}
	return out
}

// rejectUnconfiguredModel answers 404 when the public catalog is locked to
// forwarding routes and the client asked for a name that is not one of them.
func (h *Handler) rejectUnconfiguredModel(w http.ResponseWriter, model string, isClaudeRoute bool) bool {
	if config.GetPublicModelCatalog() != config.PublicModelCatalogForwarding {
		return false
	}
	if route, _ := config.ResolveRoute(model); route != nil {
		return false
	}
	msg := "model not found: " + strings.TrimSpace(model)
	if isClaudeRoute {
		h.sendClaudeError(w, http.StatusNotFound, "not_found_error", msg)
	} else {
		h.sendOpenAIError(w, http.StatusNotFound, "invalid_request_error", msg)
	}
	return true
}

// apiDiscoverAccountProfiles GET /admin/api/accounts/{id}/profiles
// Probes all candidate regions and returns the discovered profiles (deduped,
// sorted) plus any per-region errors. Never returns tokens or secrets.
func (h *Handler) apiDiscoverAccountProfiles(w http.ResponseWriter, r *http.Request, id string) {
	// Admin/setup op: resolve from the full config, not just the routable pool,
	// so profile discovery works on disabled/banned accounts too (the picker is
	// often used precisely to recover such an account).
	account := h.lookupAccountForAdmin(id)
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// Ensure a valid token via the central manager before probing.
	if h.tokenManager != nil {
		if fresh, err := h.tokenManager.EnsureFresh(id); err == nil && fresh != nil {
			account = fresh
		}
	}
	res, err := DiscoverProfiles(r.Context(), account)
	if err != nil && len(res.Profiles) == 0 {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":        err.Error(),
			"regionErrors": res.RegionErrors,
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"profiles":     res.Profiles,
		"regionErrors": res.RegionErrors,
	})
}

// apiGetAccountPinnedProfile GET /admin/api/accounts/{id}/profile
func (h *Handler) apiGetAccountPinnedProfile(w http.ResponseWriter, r *http.Request, id string) {
	pinned, ok := h.GetPinnedProfile(id)
	if !ok {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"pinned": pinned})
}

// apiSelectAccountProfile POST /admin/api/accounts/{id}/profile
// Body: {"arn": "...", "region": "..."}
func (h *Handler) apiSelectAccountProfile(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		ARN    string `json:"arn"`
		Region string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}
	result, err := h.SelectProfile(r.Context(), id, req.ARN, req.Region)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	pinned, _ := h.GetPinnedProfile(id)
	warnings := result.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":             true,
		"pinned":              pinned,
		"modelCacheRefreshed": result.ModelCacheRefreshed,
		"modelCount":          result.ModelCount,
		"warnings":            warnings,
	})
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}
	if msg := validateClaudeOutputConfig(req.OutputConfig); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking, nameEffort := resolveClaudeThinkingModeAndEffort(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	applyModelNameEffort(&req, nameEffort)
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取请求
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}
	clientModel := req.Model
	noteAPIKeyMeta(r.Context(), "claude", clientModel, "", req.Stream)

	// The last genuine user question, captured BEFORE any memory injection mutates
	// req.Messages. Used as the retrieval query (inject) and the user side of a
	// captured turn (capture). Empty when the turn carries no new question.
	userText := lastClaudeUserText(req.Messages)

	// Memory inject (read): retrieve relevant memories for this caller and prepend a
	// bounded context block to the last user message, so both the forward path (reads
	// body) and the Kiro path (reads req) see the enriched request. Fail-open: a
	// backend error/outage returns no memories and leaves the request unchanged. It
	// deliberately runs BEFORE tryForwardUpstream and re-marshals body so forwarded
	// requests are enriched too. It does NOT touch the translator.
	if config.MemoryInjectEnabled() && userText != "" {
		principal := memoryScopeForRequest(r.Context())
		mems, _ := h.getMemory().Search(r.Context(), SearchQuery{Scope: MemoryScope{Principal: principal}, Query: userText})
		if block := buildMemoryContextBlock(mems, config.GetMemoryConfig().MaxInjectTokens); block != "" {
			if injectMemoryIntoRequest(&req, block) {
				if b, err := json.Marshal(&req); err == nil {
					body = b
				}
			}
		}
	}

	// Forward to an external upstream when the (raw, un-normalized) client model
	// matches an enabled route. Passthrough bypasses the Kiro pool entirely.
	if h.tryForwardUpstream(r, w, body, req.Model, req.Stream, "/messages", true, userText) {
		// Capture (write) for the forward path is handled inside tryForwardUpstream
		// (non-stream only), since only it can tee the upstream response body.
		return
	}
	if h.rejectUnconfiguredModel(w, req.Model, true) {
		return
	}

	// The forward path declined. If it declined because the route's chain ended at
	// the Kiro-pool sentinel, tag the request so the pool's metrics are attributed
	// to that route. Must happen HERE — before req.Model is rewritten below — since
	// the route was resolved against the raw model.
	r = withPoolRouteContext(r, req.Model)

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking, nameEffort := resolveClaudeThinkingModeAndEffort(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	applyModelNameEffort(&req, nameEffort)
	noteAPIKeyMeta(r.Context(), "claude", clientModel, req.Model, req.Stream)
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	apiKeyID := apiKeyIDFromContext(r.Context())

	// Pure native web_search: relay via Kiro MCP (generateAssistantResponse does not run it).
	if hasWebSearchTool(&req) {
		h.handleWebSearchRequest(r.Context(), w, &req, estimatedInputTokens, apiKeyID)
		return
	}

	// Mixed tools including native web_search: agentic loop digests web_search internally
	// and returns client tool_use blocks as-is.
	if hasWebSearchAmongTools(&req) {
		logger.Infof("[WebSearch] Mixed tools with native web_search, entering agentic loop")
		h.runWebSearchLoop(r.Context(), w, &req, thinking, estimatedInputTokens, apiKeyID)
		return
	}

	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)

	// Resolve the web_search server-tool policy from the request. When a
	// web_search tool is present, the proxy executes the search itself via the
	// conversation runner instead of handing an unresolved tool_use to the client.
	policy, hasWebSearch, policyErr := extractWebSearchPolicy(req.Tools)
	if policyErr != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", policyErr.Error())
		return
	}
	// Fail fast when web_search is requested and the operator turned the feature
	// on but no provider is usable (no SearXNG base URL and no Tavily key):
	// silently passing the tool through would stall, since the client does not
	// execute this server tool.
	if hasWebSearch && config.WebSearchToggledOn() && !policy.Enabled {
		h.sendClaudeError(w, 500, "api_error", "web_search is enabled but no search provider is configured (set a SearXNG base URL or a Tavily API key)")
		return
	}
	// Engage the runner only when a web_search tool is present AND the feature is
	// fully enabled (toggle + key). Otherwise behavior is unchanged.
	useRunner := hasWebSearch && policy.Enabled

	// Stream or non-stream
	if req.Stream {
		h.handleClaudeStream(r.Context(), w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, useRunner, policy, userText)
	} else {
		h.handleClaudeNonStream(r.Context(), w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, useRunner, policy, userText)
	}
}

// handleClaudeStream Claude 流式响应
//
// When useRunner is set (a web_search tool is present and the feature is fully
// enabled), the ConversationRunner drives the multi-round search loop and only
// the final round's buffered events are streamed to the client. Otherwise the
// stream path behaves exactly as before (a single CallKiroAPIContext round).
func (h *Handler) handleClaudeStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, useRunner bool, policy WebSearchPolicy, userText string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	acc := newAPIKeyUsageAcc(estimatedInputTokens)
	defer noteLeaseUsageOnExit(ctx, acc)

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	reqStart := time.Now()
	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	// guard replaces the hand-maintained messageStarted flag: the executor's one
	// Committed() check enforces "no retry after the first client-visible byte".
	// message_start is Claude's first byte, so ensureMessageStart commits the guard.
	guard := &streamGuard{}
	ex := newChatExecutor(h.pool, h.ensureValidToken, h.handleAccountFailure, model)

	attempt := func(ctx context.Context, account *config.Account) attemptOutcome {
		acc.markProviderStarted()
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)
		messageStartUsage := cacheUsage

		ensureMessageStart := func() {
			if guard.Committed() {
				return
			}
			h.sendSSE(w, flusher, "message_start", map[string]interface{}{
				"type": "message_start",
				"message": map[string]interface{}{
					"id":            msgID,
					"type":          "message",
					"role":          "assistant",
					"content":       []interface{}{},
					"model":         model,
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage":         buildClaudeUsageMap(startInputTokens, 0, messageStartUsage, cacheProfile != nil),
				},
			})
			guard.Commit()
		}

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var toolUses []KiroToolUse
		var upstreamStopReason string
		var nextContentIndex int
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		activeBlockIndex := -1
		activeBlockType := ""

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				return
			}

			if !thinking {
				return
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		// emitToolUse renders one tool_use content block. Shared by the live
		// callback and the runner's final-round replay so both paths emit an
		// identical SSE shape.
		emitToolUse := func(tu KiroToolUse) {
			processClaudeText("", false, true)
			rawContentBuilder.WriteString(tu.Name)
			if b, err := json.Marshal(tu.Input); err == nil {
				rawContentBuilder.Write(b)
			}

			toolUses = append(toolUses, tu)
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    tu.ToolUseID,
					"name":  tu.Name,
					"input": map[string]interface{}{},
				},
			})

			inputJSON, _ := json.Marshal(tu.Input)
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": string(inputJSON),
				},
			})

			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
		}

		// emitWebSearchNativeBlocks streams the Anthropic-native server_tool_use +
		// web_search_tool_result pair for each search the proxy ran, so a client like
		// Claude Code renders "Web Search(query)" and counts it. Emitted before the
		// final round's text (the searches happened before the answer). The proxy
		// cannot mint real encrypted_content; an empty placeholder is sent. Gated by
		// the caller (webSearch.emitNativeToolBlocks) so it can be turned off.
		emitWebSearchNativeBlocks := func(searches []WebSearchInvocation) {
			for _, s := range searches {
				ensureMessageStart()
				closeActiveBlock()

				stuIdx := nextContentIndex
				nextContentIndex++
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": stuIdx,
					"content_block": map[string]interface{}{
						"type":  "server_tool_use",
						"id":    s.ToolUseID,
						"name":  "web_search",
						"input": map[string]interface{}{},
					},
				})
				inputJSON, _ := json.Marshal(map[string]interface{}{"query": s.Query})
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": stuIdx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": string(inputJSON),
					},
				})
				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": stuIdx,
				})

				items := make([]map[string]interface{}, 0, len(s.Sources))
				for _, src := range s.Sources {
					items = append(items, map[string]interface{}{
						"type":              "web_search_result",
						"title":             src.Title,
						"url":               src.URL,
						"encrypted_content": "",
					})
				}
				resIdx := nextContentIndex
				nextContentIndex++
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": resIdx,
					"content_block": map[string]interface{}{
						"type":        "web_search_tool_result",
						"tool_use_id": s.ToolUseID,
						"content":     items,
					},
				})
				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": resIdx,
				})
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				acc.observeText(text, isThinking)
				if isThinking {
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: emitToolUse,
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
				acc.setUpstream(inTok, outTok)
			},
			OnCredits: func(c float64) {
				credits = c
				acc.setCredits(c)
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
		}

		// usedRunner marks the web_search path so token aggregation below does not
		// let the final round's context-occupancy override the summed input total.
		usedRunner := false
		var runSources []SearchSource
		var runSearches []WebSearchInvocation

		if useRunner {
			usedRunner = true
			run, err := h.conversationRunner.Run(ctx, account, payload, policy)
			if err != nil {
				if classifyRunError(err) {
					// Kiro/account error: identical to the CallKiroAPIContext failure
					// path. Report it to the executor, which retries onto another
					// account when nothing has been flushed yet, or routes to
					// onCommitted (the "no retry after first byte" surface) once the
					// stream has started.
					return attemptAccountFailed(err)
				}
				// Search/config/mixed/cancel: NOT an account failure. Surface it here
				// (terminal) so the account is neither blamed nor retried, then tell
				// the executor the attempt is fully handled.
				if ctx.Err() != nil {
					return attemptHandled()
				}
				if !guard.Committed() {
					h.sendClaudeErrorForWebSearch(w, err)
					return attemptHandled()
				}
				h.recordFailure()
				pub := classifyGoError(err, requestIDFromContext(ctx), "search", "claude", model, "", "", "").Public()
				var mixed *MixedToolUseError
				msg := pub.MessageOrDefault()
				typ := providererr.EnvelopeType(pub.Code, true)
				if errors.As(err, &mixed) {
					msg = mixed.Error()
					typ = "invalid_request_error"
				}
				h.sendSSE(w, flusher, "error", map[string]interface{}{
					"type":       "error",
					"error":      map[string]string{"type": typ, "message": msg},
					"request_id": pub.RequestID,
				})
				return attemptHandled()
			}

			// Emit the synthetic Anthropic-native web_search blocks first: the
			// searches ran before the model composed the final answer, so they
			// precede its text in the stream. Gated by the kill-switch; empty when
			// off or when no search ran.
			if config.WebSearchEmitNativeToolBlocks() {
				runSearches = run.Searches
				emitWebSearchNativeBlocks(runSearches)
			}

			// Replay only the final round's ordered events through the same
			// renderer the live path uses. Intermediate rounds (searches) are never
			// streamed to the client.
			for _, ev := range run.FinalRound.Events {
				switch ev.Kind {
				case RoundEventText:
					rawContentBuilder.WriteString(ev.Text)
					processClaudeText(ev.Text, false, false)
				case RoundEventThinking:
					rawThinkingBuilder.WriteString(ev.Text)
					processClaudeText(ev.Text, true, false)
				case RoundEventToolUse:
					if ev.ToolUse != nil {
						emitToolUse(*ev.ToolUse)
					}
				}
			}

			inputTokens = run.TotalInputTokens
			outputTokens = run.TotalOutputTokens
			credits = run.TotalCredits
			acc.setUpstream(inputTokens, outputTokens)
			acc.setCredits(credits)
			if run.FinalContextPct > 0 {
				realInputTokens = int(run.FinalContextPct * float64(getContextWindowSize(model)) / 100.0)
			}
			runSources = run.Sources
		} else {
			measure := func() (int, int, string, bool) {
				return rawContentBuilder.Len(), len(toolUses), upstreamStopReason, rawThinkingBuilder.Len() > 0
			}

			// A same-account retry only happens while nothing has been flushed, so
			// SSE block indices are still at their initial values and need no
			// rollback. What must be cleared is every accumulator plus the thinking
			// tag parser state: processClaudeText buffers up to 50 runes before
			// flushing, so a short truncated attempt can leave a partial tag behind
			// that would otherwise be prefixed onto the retry's first chunk.
			reset := func() {
				rawContentBuilder.Reset()
				rawThinkingBuilder.Reset()
				toolUses = nil
				inputTokens = 0
				outputTokens = 0
				credits = 0
				realInputTokens = 0
				upstreamStopReason = ""
				textBuffer = ""
				inThinkingBlock = false
				dropTagThinking = false
				thinkingSource = thinkingSourceUnknown
				thinkingStarted = false
				eventThinkingOpen = false
				acc.resetObserved()
			}

			// The guard is the fork's equivalent of upstream's messageStarted flag:
			// a same-account retry is only safe while nothing has been flushed.
			err := runKiroWithIntegrityRetry(ctx, account, payload, callback, measure, reset,
				func() bool { return !guard.Committed() })
			if err != nil {
				if ctx.Err() != nil {
					return attemptHandled()
				}
				if isStreamIntegrityError(err) {
					// The account authenticated and answered; the stream ended
					// truncated. Rotate onto another account without cooling this
					// healthy one down.
					return attemptRotateWithoutBlame(err)
				}
				return attemptAccountFailed(err)
			}
		}

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}

		// Append the deterministic Sources list (web_search path) as a final text
		// delta before closing the block.
		if usedRunner && len(runSources) > 0 && config.WebSearchAppendSources() {
			if src := formatSourcesList(runSources); src != "" {
				rawContentBuilder.WriteString(src)
				sendText(src, 0)
			}
		}

		closeActiveBlock()

		// upstreamInput is the value the stream (or runner aggregate) actually
		// reported, captured before any override so accounted usage never uses
		// context occupancy. legacyInput reproduces the historical client number.
		upstreamInput := inputTokens
		var legacyInput int
		if usedRunner {
			// Aggregate totals already summed across rounds; do not overwrite with
			// the final round's context occupancy (spec: client-visible usage is the
			// whole logical request). Fall back to the estimate only if unset.
			legacyInput = upstreamInput
			if legacyInput <= 0 {
				if realInputTokens > 0 {
					legacyInput = realInputTokens
				} else {
					legacyInput = estimatedInputTokens
				}
			}
		} else if realInputTokens > 0 {
			legacyInput = realInputTokens
		} else if upstreamInput > 0 {
			legacyInput = upstreamInput
		} else {
			legacyInput = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		estimatedOutput := estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)

		// Split internal accounting (upstream-accurate) from the client-visible
		// number (legacy by default, accurate when the operator opts in).
		accountedInput, clientInput := usageSplit(upstreamInput, estimatedInputTokens, legacyInput)
		accountedOutput, clientOutput := usageSplit(outputTokens, estimatedOutput, estimatedOutput)

		h.recordAccountedSuccess(ctx, apiKeyID, accountedInput, accountedOutput, credits, upstreamInput, outputTokens)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, accountedInput+accountedOutput, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		// Pool metrics must use the accounted values, not the raw upstream vars:
		// Kiro reports no token counts, so inputTokens/outputTokens are 0 on this
		// path and the dashboard would show every pool request as 0 in / 0 out.
		h.recordSuccessLogSplit(ctx, "claude", model, account.ID, accountedInput, accountedOutput, credits, time.Since(reqStart).Milliseconds())

		// Capture (write): store this turn's Q&A into memory (async, fail-open,
		// redaction enforced in the provider). No-op unless capture is enabled
		// (non-explicit write mode). outputContent is the clean answer text.
		h.captureTurnAsync(apiKeyID, userText, outputContent)

		// Upstream's metadataEvent stopReason, mapped to Anthropic's vocabulary.
		// Falls back to end_turn when the upstream sent none, and tool_use always
		// wins when the turn produced tool calls.
		stopReason := mapClaudeStopReason(upstreamStopReason, len(toolUses))

		ensureMessageStart()
		usageMap := buildClaudeUsageMap(clientInput, clientOutput, cacheUsage, cacheProfile != nil)
		// Report the server-side searches the proxy ran so the client counts them
		// instead of "Did 0 searches". Gated by the same kill-switch as the blocks.
		if usedRunner && len(runSearches) > 0 {
			usageMap["server_tool_use"] = map[string]interface{}{
				"web_search_requests": len(runSearches),
			}
		}
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": usageMap,
		})

		h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return attemptHandled()
	}

	// Claude's mid-stream (post-commit) failure surface: blame the account (the
	// executor does not on the committed branch, matching the original loop which
	// called handleAccountFailure on both the pre- and post-commit paths), record
	// the request failure, and emit an `error` SSE event. This differs from OpenAI
	// (silent) and Responses (response.failed) — the asymmetry is intentional and
	// lives here in the per-protocol renderer.
	onCommitted := func(account *config.Account, err error) {
		h.handleAccountFailure(account, err)
		pub := h.recordUpstreamFailure(ctx, "claude", model, account.ID, "", "", err)
		h.recordFailureWithDetails(ctx, "claude", model, account.ID, err)
		h.sendSSE(w, flusher, "error", map[string]interface{}{
			"type": "error",
			"error": map[string]string{
				"type":    providererr.EnvelopeType(pub.Code, true),
				"message": pub.MessageOrDefault(),
			},
			"request_id": pub.RequestID,
		})
	}

	onExhausted := func(noAccounts bool, lastErr error) {
		if noAccounts {
			h.sendClaudeError(w, 503, "api_error", "No available accounts")
			return
		}
		pub := h.recordUpstreamFailure(ctx, "claude", model, "", "", "", lastErr)
		h.recordFailureWithDetails(ctx, "claude", model, "", lastErr)
		h.sendPublicClaudeError(w, pub)
	}

	ex.Run(ctx, guard, attempt, onCommitted, onExhausted)
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// Shutdown stops the handler's background workers and flushes dirty state. It is
// safe to call once during graceful shutdown; the stop channels are closed so the
// backgroundRefresh / backgroundStatsSaver loops exit (the stats saver persists a
// final snapshot on its way out). Any token whose persistence was deferred
// (PersistenceDegraded) gets a final flush attempt so a rotated credential is not
// lost across restart.
func (h *Handler) Shutdown() {
	// Closing the channels signals the background loops to return. Guard against a
	// double close in case Shutdown is invoked more than once.
	h.shutdownOnce.Do(func() {
		if h.tokenManager != nil {
			h.tokenManager.FlushPending()
		}
		close(h.stopRefresh)
		close(h.stopStatsSaver)
		if h.stopKeyRetention != nil {
			close(h.stopKeyRetention)
		}
		if h.keys != nil {
			_ = h.keys.Close()
		}
		// Persist a final stats snapshot synchronously (the stats saver also does
		// this on exit, but do it here too in case that goroutine already returned).
		h.saveStats()
	})
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	config.UpdateStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
	)
	if err := metrics.Save(forwardMetricsPath()); err != nil {
		logger.Warnf("[Metrics] failed to save forward stats: %v", err)
	}
}

// forwardMetricsPath returns the file holding persisted forward-metric
// aggregates, kept alongside the config file.
func forwardMetricsPath() string {
	return config.GetConfigDir() + "/forward_stats.json"
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
//
// On a leased inference request this only Notes success; ServeHTTP's settle
// performs the single Commit. Direct callers (tests, no lease) Commit immediately.
func (h *Handler) recordSuccessForApiKey(ctx context.Context, apiKeyID string, inputTokens, outputTokens int, credits float64) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		apiKeyID = apiKeyIDFromContext(ctx)
	}
	if apiKeyID == "" {
		return
	}
	in := apikey.CommitInput{
		RequestID:    requestIDFromContext(ctx),
		Outcome:      apikey.OutcomeSuccess,
		InputTokens:  int64(inputTokens),
		OutputTokens: int64(outputTokens),
		Credits:      credits,
		StatusCode:   http.StatusOK,
	}
	if noteAPIKeyOutcome(ctx, in) {
		return
	}
	if h.keys != nil {
		if err := h.keys.Commit(apiKeyID, in); err != nil {
			logger.Warnf("[ApiKey] failed to commit usage for key %s: %v", apiKeyID, err)
		}
		return
	}
	if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
}

func (h *Handler) recordAccountedSuccess(ctx context.Context, apiKeyID string, accountedIn, accountedOut int, credits float64, upstreamIn, upstreamOut int) {
	src, est := usageProvenance(upstreamIn, upstreamOut, accountedIn, accountedOut, credits)
	noteAPIKeyUsage(ctx, int64(accountedIn), int64(accountedOut), credits, src, est)
	h.recordSuccessForApiKey(ctx, apiKeyID, accountedIn, accountedOut, credits)
}

func (h *Handler) commitAPIKeyOutcome(ctx context.Context, outcome, endpoint, model, errCode, errMsg string, status int, inTok, outTok int, credits float64, latencyMs, ttfbMs int64, ttfbKnown bool, stream bool) {
	id := apiKeyIDFromContext(ctx)
	if id == "" || h.keys == nil {
		return
	}
	in := apikey.CommitInput{
		RequestID:      requestIDFromContext(ctx),
		Outcome:        outcome,
		InputTokens:    int64(inTok),
		OutputTokens:   int64(outTok),
		Credits:        credits,
		Endpoint:       endpoint,
		ClientModel:    model,
		EffectiveModel: model,
		StatusCode:     status,
		LatencyMs:      latencyMs,
		TTFBMs:         ttfbMs,
		TTFBKnown:      ttfbKnown,
		Stream:         stream,
		ErrorCode:      classifyStoredError(status, errCode, errMsg),
		SanitizedError: errMsg,
	}
	if noteAPIKeyOutcome(ctx, in) {
		return
	}
	if err := h.keys.Commit(id, in); err != nil {
		logger.Warnf("[ApiKey] commit %s for key %s: %v", outcome, id, err)
	}
}

func classifyStoredError(status int, errCode, errMsg string) string {
	if apikey.KnownErrorCode(errCode) {
		return errCode
	}
	return apikey.ClassifyPublicError(status, errCode, errMsg)
}

// recordFailure bumps the global failure counters. Kept as its own method because
// the fork's per-protocol committed-failure and exhaustion renderers call it
// directly when there is no endpoint/model/account context worth logging.
func (h *Handler) recordFailure() {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
}

// recordFailureWithDetails records a failure and stores it in the request logs.
//
// ctx carries the route this request is being served for, when the pool is
// answering on behalf of a forwarding route (see pool_route_context.go). It may
// be nil on paths with no request scope; attribution is then simply skipped.
func (h *Handler) recordFailureWithDetails(ctx context.Context, endpoint, model, accountID string, err error) {
	h.recordFailure()

	if err == nil {
		return
	}

	in := classifyGoError(err, requestIDFromContext(ctx), endpoint, endpoint, model, accountID, "", "")
	h.persistProviderError(in)
	errMsg := in.AdminSummary()
	errType := classifyError(err.Error())

	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		Status:    "error",
		Error:     errMsg,
		ErrorType: errType,
	}

	h.appendRequestLog(entry)
	recordKiroMetric(kiroMetric{
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		ErrorMsg:  errMsg,
		ErrorType: errType,
		RouteID:   poolRouteIDFromContext(ctx),
		ClientIP:  clientIPFromContext(ctx),
		ApiKeyID:  apiKeyIDFromContext(ctx),
		RequestID: requestIDFromContext(ctx),
	})
	pub := in.Public()
	h.commitAPIKeyOutcome(ctx, apikey.OutcomeFailed, endpoint, model, pub.Code, pub.Message, pub.HTTPStatus, 0, 0, 0, 0, 0, false, false)
}

// recordSuccessLogSplit records a successful request in the request logs and in
// the metrics store. RequestLog keeps only the combined input+output total; the
// split is preserved for the per-provider token breakdown in the dashboard,
// which would otherwise render every Kiro response as "out 0".
//
// ctx carries the pool-route attribution, as in recordFailureWithDetails.
func (h *Handler) recordSuccessLogSplit(ctx context.Context, endpoint, model, accountID string, inputTokens, outputTokens int, credits float64, durationMs int64) {
	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		Status:    "success",
		Tokens:    inputTokens + outputTokens,
		Credits:   credits,
		Duration:  durationMs,
	}

	h.appendRequestLog(entry)
	recordKiroMetric(kiroMetric{
		Endpoint:     endpoint,
		Model:        model,
		AccountID:    accountID,
		Ok:           true,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Credits:      credits,
		DurationMs:   durationMs,
		RouteID:      poolRouteIDFromContext(ctx),
		ClientIP:     clientIPFromContext(ctx),
		ApiKeyID:     apiKeyIDFromContext(ctx),
		RequestID:    requestIDFromContext(ctx),
	})
}

func (h *Handler) appendRequestLog(entry RequestLog) {
	h.requestLogsMu.Lock()
	if h.requestLogs == nil {
		h.requestLogs = make([]RequestLog, 0, requestLogsMaxSize)
	}
	if len(h.requestLogs) >= requestLogsMaxSize {
		h.requestLogs = h.requestLogs[1:]
	}
	h.requestLogs = append(h.requestLogs, entry)
	h.requestLogsMu.Unlock()
}

// classifyError categorizes an error message into a type for display.
func classifyError(msg string) string {
	switch {
	case isQuotaErrorMessage(msg):
		return "quota"
	case isOverageErrorMessage(msg):
		return "overage"
	case isSuspensionErrorMessage(msg):
		return "suspended"
	case isAuthErrorMessage(msg):
		return "auth"
	case isProfileUnavailableErrorMessage(msg):
		return "profile"
	default:
		return "unknown"
	}
}

// getRequestLogs returns a copy of request logs (newest first).
func (h *Handler) getRequestLogs() []RequestLog {
	h.requestLogsMu.RLock()
	defer h.requestLogsMu.RUnlock()
	if len(h.requestLogs) == 0 {
		return []RequestLog{}
	}
	result := make([]RequestLog, len(h.requestLogs))
	for i, e := range h.requestLogs {
		result[len(h.requestLogs)-1-i] = e
	}
	return result
}

// usageSplit separates the token count used for INTERNAL accounting from the one
// reported to the CLIENT, so a single request can charge accurate usage against
// the pool/key while still emitting the historical (legacy) client number.
//
//   - accounted: what the internal sinks (per-key TokensUsed, per-account
//     TotalTokens, global counters) record. Upstream-reported first, estimator
//     fallback. It NEVER uses context-window occupancy as a token count.
//   - client: what the response's usage map reports. In legacy mode (default) it
//     is the tail's historical value, passed in verbatim as legacyClient — so the
//     client sees byte-identical numbers to before this change. In accurate mode
//     it switches to the accounted value.
//
// upstream is the upstream-reported count (0 when the stream carried none);
// estimated is the local estimator fallback; legacyClient is the exact value the
// tail reported before Phase A (which, for most tails, is context occupancy).
func usageSplit(upstream, estimated, legacyClient int) (accounted, client int) {
	accounted = upstream
	if accounted <= 0 {
		accounted = estimated
	}
	client = legacyClient
	if config.GetUsageReportingMode() == config.UsageReportingAccurate {
		client = accounted
	}
	return accounted, client
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, useRunner bool, policy WebSearchPolicy, userText string) {
	// Non-stream: fully buffered, so the guard is never committed and a late
	// upstream error can still retry (invariant #7). No committed-failure branch.
	reqStart := time.Now()
	acc := newAPIKeyUsageAcc(estimatedInputTokens)
	defer noteLeaseUsageOnExit(ctx, acc)
	guard := &streamGuard{}
	ex := newChatExecutor(h.pool, h.ensureValidToken, h.handleAccountFailure, model)

	attempt := func(ctx context.Context, account *config.Account) attemptOutcome {
		acc.markProviderStarted()
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var sources []SearchSource
		var searches []WebSearchInvocation
		var upstreamStopReason string

		if useRunner {
			// Server-side web_search path: the runner drives as many Kiro rounds as
			// the model needs, executing searches in between, and returns the final
			// round plus aggregate usage.
			run, err := h.conversationRunner.Run(ctx, account, payload, policy)
			if err != nil {
				if classifyRunError(err) {
					// A Kiro/account error: fail the account and retry from the
					// original payload on a different account.
					return attemptAccountFailed(err)
				}
				// Search/config/mixed/cancel error: do not fail the account. A
				// cancelled context renders nothing (client is gone); otherwise map
				// the error to a Claude-compatible response. Both are terminal — the
				// executor must not retry or fall through to the exhaustion tail.
				if ctx.Err() != nil {
					return attemptHandled()
				}
				h.sendClaudeErrorForWebSearch(w, err)
				return attemptHandled()
			}
			fr := run.FinalRound
			content = fr.VisibleContent
			thinkingContent = fr.ThinkingContent
			toolUses = fr.ToolUses
			inputTokens = run.TotalInputTokens
			outputTokens = run.TotalOutputTokens
			credits = run.TotalCredits
			acc.setUpstream(inputTokens, outputTokens)
			acc.setCredits(credits)
			if fr.ContextUsagePct > 0 {
				realInputTokens = int(fr.ContextUsagePct * float64(getContextWindowSize(model)) / 100.0)
			}
			sources = run.Sources
			if config.WebSearchEmitNativeToolBlocks() {
				searches = run.Searches
			}
		} else {
			callback := &KiroStreamCallback{
				OnText: func(text string, isThinking bool) {
					if isThinking {
						thinkingContent += text
					} else {
						content += text
					}
				},
				OnToolUse: func(tu KiroToolUse) {
					toolUses = append(toolUses, tu)
				},
				OnComplete: func(inTok, outTok int) {
					inputTokens = inTok
					outputTokens = outTok
				},
				OnCredits: func(c float64) {
					credits = c
				},
				OnContextUsage: func(pct float64) {
					realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
				},
				OnStopReason: func(reason string) {
					upstreamStopReason = reason
				},
			}
			callback = observeKiroCallback(acc, callback)

			measure := func() (int, int, string, bool) {
				return len(content), len(toolUses), upstreamStopReason, thinkingContent != ""
			}

			reset := func() {
				content = ""
				thinkingContent = ""
				toolUses = nil
				inputTokens = 0
				outputTokens = 0
				credits = 0
				realInputTokens = 0
				upstreamStopReason = ""
				acc.resetObserved()
			}

			// Fully buffered: nothing reaches the client until the response is
			// encoded, so a retry can never duplicate output — canRetry is nil.
			err := runKiroWithIntegrityRetry(ctx, account, payload, callback, measure, reset, nil)
			if err != nil {
				if ctx.Err() != nil {
					return attemptHandled()
				}
				if isStreamIntegrityError(err) {
					return attemptRotateWithoutBlame(err)
				}
				return attemptAccountFailed(err)
			}
		}

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		// upstreamInput is the value the stream (or runner aggregate) reported,
		// captured before any override so accounted usage never uses context
		// occupancy. legacyInput reproduces the historical client number (which,
		// on this tail, lets context occupancy override even the runner total).
		upstreamInput := inputTokens
		var legacyInput int
		if realInputTokens > 0 {
			legacyInput = realInputTokens
		} else if upstreamInput > 0 {
			legacyInput = upstreamInput
		} else {
			legacyInput = estimatedInputTokens
		}
		estimatedOutput := estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		accountedInput, clientInput := usageSplit(upstreamInput, estimatedInputTokens, legacyInput)
		accountedOutput, clientOutput := usageSplit(outputTokens, estimatedOutput, estimatedOutput)

		// Append a deterministic Sources list when the runner gathered any and the
		// feature is configured to do so.
		if useRunner && len(sources) > 0 && config.WebSearchAppendSources() {
			finalContent += formatSourcesList(sources)
		}

		h.recordAccountedSuccess(ctx, apiKeyID, accountedInput, accountedOutput, credits, upstreamInput, outputTokens)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, accountedInput+accountedOutput, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		// Pool metrics must use the accounted values, not the raw upstream vars:
		// Kiro reports no token counts, so inputTokens/outputTokens are 0 on this
		// path and the dashboard would show every pool request as 0 in / 0 out.
		h.recordSuccessLogSplit(ctx, "claude", model, account.ID, accountedInput, accountedOutput, credits, time.Since(reqStart).Milliseconds())

		// Capture (write) this turn into memory when auto-capture is on. Async +
		// fail-open + provider-enforced redaction; never blocks or breaks the response.
		h.captureTurnAsync(memoryScopeForRequest(ctx), userText, finalContent)

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, clientInput, clientOutput, model, searches, upstreamStopReason)
		resp.Usage.InputTokens = billedClaudeInputTokens(clientInput, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if len(searches) > 0 {
			resp.Usage.ServerToolUse = &ClaudeServerToolUsage{WebSearchRequests: len(searches)}
		}
		if cacheProfile != nil {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return attemptHandled()
	}

	onExhausted := func(noAccounts bool, lastErr error) {
		if noAccounts {
			h.sendClaudeError(w, 503, "api_error", "No available accounts")
			return
		}
		pub := h.recordUpstreamFailure(ctx, "claude", model, "", "", "", lastErr)
		h.recordFailureWithDetails(ctx, "claude", model, "", lastErr)
		h.sendPublicClaudeError(w, pub)
	}

	ex.Run(ctx, guard, attempt, nil, onExhausted)
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	if ctx := leaseContextFromWriter(w); ctx != nil {
		noteAPIKeyHTTPStatus(ctx, status, errType, message)
	}
	rid := requestIDFromWriter(w)
	setRequestIDHeader(w, rid)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body := map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	}
	if rid != "" {
		body["request_id"] = rid
	}
	json.NewEncoder(w).Encode(body)
}

// sendClaudeErrorForWebSearch maps a non-account web_search runner error to a
// Claude-compatible error response. Provider auth/config failures are 500-class
// api_errors; a mixed server/client tool turn is a 400 invalid_request the
// client can act on; anything else falls back to a generic 502.
func (h *Handler) sendClaudeErrorForWebSearch(w http.ResponseWriter, err error) {
	h.recordFailure()
	var cfgErr *search.ConfigError
	var provErr *search.ProviderError
	var mixed *MixedToolUseError
	switch {
	case errors.As(err, &mixed):
		h.sendClaudeError(w, 400, "invalid_request_error", mixed.Error())
	case errors.As(err, &cfgErr):
		h.sendClaudeError(w, 500, "api_error", cfgErr.Error())
	case errors.As(err, &provErr):
		// Auth against the search provider is a server misconfiguration; do not
		// surface provider kind or raw error text to the client.
		if provErr.Kind == search.ErrAuth {
			h.sendClaudeError(w, 500, "api_error", "web_search provider authentication failed")
		} else {
			pub := classifyGoError(err, requestIDFromWriter(w), "search", "claude", "", "", "", "").Public()
			h.sendPublicClaudeError(w, pub)
		}
	default:
		pub := classifyGoError(err, requestIDFromWriter(w), "search", "claude", "", "", "", "").Public()
		h.sendPublicClaudeError(w, pub)
	}
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}
	clientModel := req.Model
	noteAPIKeyMeta(r.Context(), "openai", clientModel, "", req.Stream)

	// Forward to an external upstream when the (raw, un-normalized) client model
	// matches an enabled route. Passthrough bypasses the Kiro pool entirely.
	if h.tryForwardUpstream(r, w, body, req.Model, req.Stream, "/chat/completions", false, "") {
		return
	}
	if h.rejectUnconfiguredModel(w, req.Model, false) {
		return
	}

	// Attribute pool traffic to the route that fell through to it — before the
	// model rewrite below, which strips the suffix the route was matched on.
	r = withPoolRouteContext(r, req.Model)

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking, nameEffort := ParseModelThinkingAndEffort(req.Model, thinkingCfg.Suffix)
	req.Model = actualModel
	applyOpenAIModelNameEffort(&req, nameEffort)
	noteAPIKeyMeta(r.Context(), "openai", clientModel, req.Model, req.Stream)
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := OpenAIToKiro(&req, thinking)

	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.Stream {
		h.handleOpenAIStream(r.Context(), w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	} else {
		h.handleOpenAINonStream(r.Context(), w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	}
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	acc := newAPIKeyUsageAcc(estimatedInputTokens)
	defer noteLeaseUsageOnExit(ctx, acc)

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	reqStart := time.Now()
	guard := &streamGuard{}
	ex := newChatExecutor(h.pool, h.ensureValidToken, h.handleAccountFailure, model)

	attempt := func(ctx context.Context, account *config.Account) attemptOutcome {
		acc.markProviderStarted()
		var upstreamStopReason string
		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			guard.Commit()
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendChunk(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendChunk(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendChunk(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
				guard.Commit()
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}
		callback = observeKiroCallback(acc, callback)

		measure := func() (int, int, string, bool) {
			return rawContentBuilder.Len(), len(toolCalls), upstreamStopReason, rawReasoningBuilder.Len() > 0
		}

		// Retries only happen before anything is flushed, so chunk indices stay
		// valid. The thinking tag parser state must be cleared too: processText
		// holds back up to 50 runes, so a short truncated attempt would
		// otherwise prepend its leftovers to the retry's first chunk.
		reset := func() {
			rawContentBuilder.Reset()
			rawReasoningBuilder.Reset()
			toolCalls = nil
			toolCallIndex = 0
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			upstreamStopReason = ""
			textBuffer = ""
			inThinkingBlock = false
			dropTagThinking = false
			thinkingSource = thinkingSourceUnknown
			thinkingStarted = false
			eventThinkingOpen = false
			acc.resetObserved()
		}

		err := runKiroWithIntegrityRetry(ctx, account, payload, callback, measure, reset,
			func() bool { return !guard.Committed() })
		if err != nil {
			if ctx.Err() != nil {
				return attemptHandled()
			}
			// Integrity failures are upstream hiccups, not account faults: rotate
			// without cooling down a healthy account.
			if isStreamIntegrityError(err) {
				return attemptRotateWithoutBlame(err)
			}
			return attemptAccountFailed(err)
		}

		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		upstreamInput := inputTokens
		var legacyInput int
		if realInputTokens > 0 {
			legacyInput = realInputTokens
		} else if upstreamInput > 0 {
			legacyInput = upstreamInput
		} else {
			legacyInput = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		estimatedOutput := estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			estimatedOutput += estimateApproxTokens(tc.Function.Name)
			estimatedOutput += estimateApproxTokens(tc.Function.Arguments)
		}

		accountedInput, clientInput := usageSplit(upstreamInput, estimatedInputTokens, legacyInput)
		accountedOutput, clientOutput := usageSplit(outputTokens, estimatedOutput, estimatedOutput)

		h.recordAccountedSuccess(ctx, apiKeyID, accountedInput, accountedOutput, credits, upstreamInput, outputTokens)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, accountedInput+accountedOutput, credits)
		// See the claude tails: accounted values, not the always-0 upstream vars.
		h.recordSuccessLogSplit(ctx, "openai", model, account.ID, accountedInput, accountedOutput, credits, time.Since(reqStart).Milliseconds())
		finishReason := mapOpenAIFinishReason(upstreamStopReason, len(toolCalls))

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": map[string]int{
				"prompt_tokens":     clientInput,
				"completion_tokens": clientOutput,
				"total_tokens":      clientInput + clientOutput,
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return attemptHandled()
	}

	// OpenAI's mid-stream (post-commit) failure surface is deliberately SILENT: no
	// error object, no [DONE] — just handleAccountFailure + recordFailure + return.
	// This asymmetry (Claude emits an error SSE, Responses emits response.failed) is
	// intentional and is preserved by making it this protocol's committed-failure
	// renderer. handleAccountFailure runs here because the executor does not blame
	// the account on the committed branch (the original loop called it on both the
	// pre- and post-commit paths) — except for stream-integrity failures, where
	// the account answered fine and the upstream cut the stream.
	//
	// The stream is NOT left silent: a client that receives partial content and
	// then nothing cannot distinguish a truncated turn from a slow one, so an
	// error chunk is emitted (and deliberately no [DONE], which would claim the
	// turn completed).
	onCommitted := func(account *config.Account, err error) {
		if !isStreamIntegrityError(err) {
			h.handleAccountFailure(account, err)
		}
		pub := h.recordUpstreamFailure(ctx, "openai", model, account.ID, "", "", err)
		h.recordFailureWithDetails(ctx, "openai", model, account.ID, err)
		data, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message":    pub.MessageOrDefault(),
				"type":       providererr.EnvelopeType(pub.Code, false),
				"code":       pub.Code,
				"request_id": pub.RequestID,
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	onExhausted := func(noAccounts bool, lastErr error) {
		if noAccounts {
			h.sendOpenAIError(w, 503, "server_error", "No available accounts")
			return
		}
		pub := h.recordUpstreamFailure(ctx, "openai", model, "", "", "", lastErr)
		h.recordFailureWithDetails(ctx, "openai", model, "", lastErr)
		h.sendPublicOpenAIError(w, pub)
	}

	ex.Run(ctx, guard, attempt, onCommitted, onExhausted)
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	// Non-stream: fully buffered, so the guard is never committed and a late
	// upstream error can still retry (invariant #7). No committed-failure branch.
	reqStart := time.Now()
	acc := newAPIKeyUsageAcc(estimatedInputTokens)
	defer noteLeaseUsageOnExit(ctx, acc)
	guard := &streamGuard{}
	ex := newChatExecutor(h.pool, h.ensureValidToken, h.handleAccountFailure, model)

	attempt := func(ctx context.Context, account *config.Account) attemptOutcome {
		acc.markProviderStarted()
		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamStopReason string

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
		}
		callback = observeKiroCallback(acc, callback)

		measure := func() (int, int, string, bool) {
			return len(content), len(toolUses), upstreamStopReason, reasoningContent != ""
		}

		reset := func() {
			content = ""
			reasoningContent = ""
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			upstreamStopReason = ""
			acc.resetObserved()
		}

		// Fully buffered: nothing reaches the client until the response is
		// encoded, so a retry can never duplicate output.
		err := runKiroWithIntegrityRetry(ctx, account, payload, callback, measure, reset, nil)
		if err != nil {
			if ctx.Err() != nil {
				return attemptHandled()
			}
			// Integrity failures are upstream hiccups, not account faults.
			if isStreamIntegrityError(err) {
				return attemptRotateWithoutBlame(err)
			}
			return attemptAccountFailed(err)
		}

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}

		upstreamInput := inputTokens
		var legacyInput int
		if realInputTokens > 0 {
			legacyInput = realInputTokens
		} else if upstreamInput > 0 {
			legacyInput = upstreamInput
		} else {
			legacyInput = estimatedInputTokens
		}
		estimatedOutput := estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		accountedInput, clientInput := usageSplit(upstreamInput, estimatedInputTokens, legacyInput)
		accountedOutput, clientOutput := usageSplit(outputTokens, estimatedOutput, estimatedOutput)

		h.recordAccountedSuccess(ctx, apiKeyID, accountedInput, accountedOutput, credits, upstreamInput, outputTokens)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, accountedInput+accountedOutput, credits)
		// See the claude tails: accounted values, not the always-0 upstream vars.
		h.recordSuccessLogSplit(ctx, "openai", model, account.ID, accountedInput, accountedOutput, credits, time.Since(reqStart).Milliseconds())

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, clientInput, clientOutput, model, thinkingFormat, upstreamStopReason)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return attemptHandled()
	}

	onExhausted := func(noAccounts bool, lastErr error) {
		if noAccounts {
			h.sendOpenAIError(w, 503, "server_error", "No available accounts")
			return
		}
		pub := h.recordUpstreamFailure(ctx, "openai", model, "", "", "", lastErr)
		h.recordFailureWithDetails(ctx, "openai", model, "", lastErr)
		h.sendPublicOpenAIError(w, pub)
	}

	ex.Run(ctx, guard, attempt, nil, onExhausted)
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	if ctx := leaseContextFromWriter(w); ctx != nil {
		noteAPIKeyHTTPStatus(ctx, status, errType, message)
	}
	rid := requestIDFromWriter(w)
	setRequestIDHeader(w, rid)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	errObj := map[string]interface{}{
		"type":    errType,
		"message": message,
	}
	if rid != "" {
		errObj["request_id"] = rid
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": errObj,
	})
}

// refreshAccountToken serializes the complete refresh-token rotation lifecycle:
// load the latest persisted credential, refresh it, persist any rotation, and
// only then publish it to the runtime pool. A single lock is intentionally used
// across accounts because refreshes are rare and this keeps every refresh entry
// point consistent.
//
// The request hot path does NOT go through here — ensureValidToken routes to the
// TokenManager, which coalesces per account instead of taking a global lock.
// This remains the entry point for the admin/forced refreshes (force=true), where
// the caller wants the full load-refresh-persist-publish cycle synchronously.
func (h *Handler) refreshAccountToken(account *config.Account, force bool) (bool, error) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return false, fmt.Errorf("account is required for token refresh")
	}

	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()

	var latest *config.Account
	accounts := config.GetAccounts()
	for i := range accounts {
		if accounts[i].ID == account.ID {
			latest = &accounts[i]
			break
		}
	}
	if latest == nil {
		return false, fmt.Errorf("account %s no longer exists", account.ID)
	}
	working := *latest

	// API Key credentials never expire and cannot be OAuth-refreshed.
	if config.IsAPIKeyAccount(&working) {
		token := strings.TrimSpace(working.KiroApiKey)
		if token == "" {
			token = strings.TrimSpace(working.AccessToken)
		}
		if token == "" {
			return false, fmt.Errorf("account %s has no kiroApiKey", working.ID)
		}
		h.pool.UpdateCredentialState(
			account,
			working.ID,
			token,
			"",
			0,
			"",
		)
		return false, nil
	}

	if !force && (working.ExpiresAt == 0 || time.Now().Unix() < working.ExpiresAt-tokenRefreshSkewSeconds) {
		h.pool.UpdateCredentialState(
			account,
			working.ID,
			working.AccessToken,
			working.RefreshToken,
			working.ExpiresAt,
			working.ProfileArn,
		)
		return false, nil
	}
	if strings.TrimSpace(working.RefreshToken) == "" {
		return false, fmt.Errorf("account %s has no refresh token", working.ID)
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(&working)
	if err != nil {
		return false, err
	}
	if refreshToken == "" {
		refreshToken = working.RefreshToken
	}

	if err := config.UpdateAccountCredentialState(
		working.ID,
		accessToken,
		refreshToken,
		expiresAt,
		profileArn,
	); err != nil {
		return false, fmt.Errorf("persist refreshed token for account %s: %w", working.ID, err)
	}

	// Do not expose a rotated credential through the pool until persistence has
	// succeeded. This ordering prevents a later refresh from reading stale state.
	h.pool.UpdateCredentialState(
		account,
		working.ID,
		accessToken,
		refreshToken,
		expiresAt,
		profileArn,
	)
	return true, nil
}

// ensureValidToken 确保 token 有效。
//
// Coordination, IdP call, validation, atomic persist, and pool publish are all
// owned by the TokenManager: concurrent requests for the same account coalesce
// onto a single refresh (one IdP call), while different accounts refresh in
// parallel (no global lock). On success the freshest snapshot is copied back into
// the caller's account struct so the in-flight request uses the new credentials.
func (h *Handler) ensureValidToken(account *config.Account) error {
	// API Key credentials never expire and cannot be OAuth-refreshed; they only
	// need to be present.
	if config.IsAPIKeyAccount(account) {
		if accountBearerToken(account) == "" {
			return fmt.Errorf("account %s has no kiroApiKey", account.ID)
		}
		return nil
	}
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	fresh, err := h.tokenManager.EnsureFresh(account.ID)
	if err != nil {
		return err
	}
	if fresh != nil {
		account.AccessToken = fresh.AccessToken
		account.RefreshToken = fresh.RefreshToken
		account.ExpiresAt = fresh.ExpiresAt
		account.ProfileArn = fresh.ProfileArn
	}
	return nil
}

// ==================== 管理 API ====================

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// 验证密码
	password := r.Header.Get("X-Admin-Password")
	if password == "" {
		cookie, _ := r.Cookie("admin_password")
		if cookie != nil {
			password = cookie.Value
		}
	}

	if subtle.ConstantTimeCompare([]byte(password), []byte(config.GetPassword())) != 1 {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh 必须在通用 /refresh 前匹配，否则会被误拦截
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.modelCache.apiRefreshAllAccountsModels(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.modelCache.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/profiles") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/profiles")
		h.apiDiscoverAccountProfiles(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/profile") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/profile")
		h.apiGetAccountPinnedProfile(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/profile") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/profile")
		h.apiSelectAccountProfile(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiGetAccountOverage(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/microsoft-sso/start" && r.Method == "POST":
		h.apiStartMicrosoftSSO(w, r)
	case path == "/auth/microsoft-sso/complete" && r.Method == "POST":
		h.apiCompleteMicrosoftSSO(w, r)
	case path == "/auth/microsoft-sso/select-profile" && r.Method == "POST":
		h.apiSelectMicrosoftSSOProfile(w, r)
	case path == "/auth/microsoft-sso/cancel" && r.Method == "POST":
		h.apiCancelMicrosoftSSO(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/auth/local-cache/scan" && r.Method == "GET":
		h.apiScanLocalCache(w, r)
	case path == "/auth/local-cache/import" && r.Method == "POST":
		h.apiImportLocalCache(w, r)
	case path == "/auth/apikeys-batch" && r.Method == "POST":
		h.apiImportApiKeys(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiStartKiroSso(w, r)
	case path == "/auth/kiro-sso/poll" && r.Method == "POST":
		h.apiPollKiroSso(w, r)
	case path == "/auth/kiro-sso/cancel" && r.Method == "POST":
		h.apiCancelKiroSso(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/logs" && r.Method == "GET":
		h.apiGetLogs(w, r)
	case path == "/logs" && r.Method == "DELETE":
		h.apiClearLogs(w, r)
	case path == "/logs/history" && r.Method == "GET":
		h.apiGetConsoleHistory(w, r)
	case path == "/logs/stream" && r.Method == "GET":
		h.apiStreamLogs(w, r)
	case path == "/logs/level" && r.Method == "GET":
		h.apiGetLogLevel(w, r)
	case path == "/logs/level" && r.Method == "POST":
		h.apiSetLogLevel(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/proxy/import" && r.Method == "POST":
		h.apiImportProxies(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/memory/config" && r.Method == "GET":
		h.apiGetMemoryConfig(w, r)
	case path == "/memory/config" && r.Method == "POST":
		h.apiUpdateMemoryConfig(w, r)
	case path == "/memory/search" && r.Method == "GET":
		h.apiMemorySearch(w, r)
	case path == "/memory" && r.Method == "POST":
		h.apiMemoryAdd(w, r)
	case path == "/memory" && r.Method == "DELETE":
		h.apiMemoryDelete(w, r)
	case path == "/upstreams/export" && r.Method == "GET":
		h.apiExportUpstreams(w, r)
	case path == "/upstreams/import" && r.Method == "POST":
		h.apiImportUpstreams(w, r)
	case strings.HasPrefix(path, "/upstreams/") && h.handleUpstreamConnectionAPI(w, r, path):
		return
	case path == "/upstreams" && r.Method == "GET":
		h.apiGetUpstreams(w, r)
	case path == "/upstreams" && r.Method == "POST":
		h.apiUpdateUpstreams(w, r)
	case path == "/upstream-models" && r.Method == "POST":
		h.apiUpstreamModels(w, r)
	case path == "/upstream-test" && r.Method == "POST":
		h.apiUpstreamTest(w, r)
	case path == "/forward-stats" && r.Method == "GET":
		h.apiGetForwardStats(w, r)
	case path == "/top-ips" && r.Method == "GET":
		h.apiGetTopIPs(w, r)
	case path == "/provider-stats" && r.Method == "GET":
		h.apiGetProviderDetail(w, r)
	case path == "/forward-history" && r.Method == "GET":
		h.apiGetForwardHistory(w, r)
	case path == "/forward-events/export" && r.Method == "GET":
		h.apiExportForwardEvents(w, r)
	case path == "/forward-stats/reset-provider" && r.Method == "POST":
		h.apiResetProviderStats(w, r)
	case path == "/forward-stats/reset" && r.Method == "POST":
		h.apiResetForwardStats(w, r)
	case path == "/forward-events" && r.Method == "GET":
		h.apiGetForwardEvents(w, r)
	case path == "/provider-errors" && r.Method == "GET":
		h.apiGetProviderError(w, r)
	case strings.HasPrefix(path, "/provider-errors/") && r.Method == "GET":
		h.apiGetProviderError(w, r)
	case path == "/forward-events/stream" && r.Method == "GET":
		h.apiStreamForwardEvents(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/net-interfaces" && r.Method == "GET":
		h.apiGetNetInterfaces(w, r)
	case path == "/public-ip" && r.Method == "GET":
		h.apiGetPublicIP(w, r)
	case path == "/security" && r.Method == "GET":
		h.apiGetSecurity(w, r)
	case path == "/security" && r.Method == "POST":
		h.apiUpdateSecurity(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/inject/status" && r.Method == "GET":
		h.apiInjectStatus(w, r)
	case path == "/inject" && r.Method == "POST":
		h.apiInjectAccount(w, r)
	case path == "/api-keys" && r.Method == "GET":
		h.apiListApiKeys(w, r)
	case path == "/api-keys" && r.Method == "POST":
		h.apiCreateApiKey(w, r)
	case path == "/api-keys/batch" && r.Method == "POST":
		h.apiCreateApiKeyBatch(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/rotate") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/rotate")
		h.apiRotateApiKey(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/portal-token") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/portal-token")
		h.apiCreatePortalToken(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/portal-token") && r.Method == "DELETE":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/portal-token")
		h.apiDeletePortalToken(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "GET":
		h.apiGetApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "PUT":
		h.apiUpdateApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "DELETE":
		h.apiDeleteApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]

		result[i] = map[string]interface{}{
			"id":                a.ID,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"provider":          a.Provider,
			"region":            a.Region,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          accountBearerToken(&a) != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
			// Current pinned Kiro/CodeWhisperer profile (read-only, from the
			// persisted snapshot). No network call, no secrets — the ARN and its
			// region are not credentials. Lets the card show the active profile
			// without a per-card discovery call.
			"currentProfileArn":    a.ProfileArn,
			"currentProfileRegion": a.EffectiveApiRegion(),
			"currentProfileLabel":  shortARN(a.ProfileArn),
			"hasPinnedProfile":     strings.TrimSpace(a.ProfileArn) != "",
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}

	// Handle API-key credential creation
	if account.KiroApiKey != "" || strings.EqualFold(account.AuthMethod, "api_key") || strings.EqualFold(account.AuthMethod, "apikey") {
		// Reject empty API key
		if account.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
			return
		}
		// Normalize authMethod and set expiry
		account.AuthMethod = "api_key"
		account.ExpiresAt = 0
		account.AccessToken = account.KiroApiKey // Set accessToken to kiroApiKey for pool compatibility
		// Don't call RefreshToken for API-key accounts
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token（或是 API-key 账号），立即拉取并缓存模型列表
	if account.Enabled && (account.AccessToken != "" || account.IsApiKeyCredential()) {
		go func(acc config.Account) {
			if err := h.modelCache.FetchAndCache(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		}(account)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	// Drop the deleted account's cached models so /v1/models stops advertising them
	// immediately (and a later aggregate rebuild cannot resurrect them).
	h.modelCache.DropAccount(id)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.modelCache.FetchAndCache(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", acc.Email, err)
			}
		}(*existing)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage 拉取并返回单个账号的上游 Overages 状态。
// 同步把结果写回 config.json 缓存，确保 UI 与持久化一致。
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
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

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

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

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				// 记录本次从禁用→启用、且有 token 的账号
				if enabled && !a.Enabled && a.AccessToken != "" {
					toRefreshModels = append(toRefreshModels, a)
				}
				a.Enabled = enabled
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			go func(a config.Account) {
				a.Enabled = true
				if err := h.modelCache.FetchAndCache(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			}(acc)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// 刷新 token
			if account.RefreshToken != "" {
				if _, err := h.refreshAccountToken(account, true); err != nil {
					logger.Warnf("[BatchRefresh] Token refresh failed for %s: %v", account.Email, err)
					failCount++
					continue
				}
			}
			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	sessionID, authorizeURL, expiresIn, err := auth.StartMicrosoftSSOLogin()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeURL,
		"expiresIn":    expiresIn,
		"stage":        "kiro",
	})
}

func (h *Handler) apiCompleteMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackURL string `json:"callbackUrl"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.CallbackURL) == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId and callbackUrl are required"})
		return
	}

	progress, err := auth.ContinueMicrosoftSSOLogin(req.SessionID, req.CallbackURL)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if h.microsoftSessionCanceled(req.SessionID) {
		auth.CancelMicrosoftSSOLogin(req.SessionID)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if progress.AuthorizationURL != "" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":      true,
			"stage":        "microsoft",
			"authorizeUrl": progress.AuthorizationURL,
		})
		return
	}
	if progress.Result == nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft SSO returned no credential"})
		return
	}

	result := progress.Result
	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         result.Email,
		UserId:        result.UserID,
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    auth.MicrosoftSSOAuthMethod,
		Provider:      auth.MicrosoftSSOProvider,
		Region:        "us-east-1",
		ExpiresAt:     result.ExpiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
	}

	discoveryContext, discovery, ok := h.beginMicrosoftProfileDiscovery(r.Context(), req.SessionID)
	if !ok {
		clearMicrosoftAccountCredential(&account)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	profiles, profileErr := DiscoverKiroProfilesContext(discoveryContext, &account)
	discoveryErr := discoveryContext.Err()
	h.endMicrosoftProfileDiscovery(req.SessionID, discovery)

	h.microsoftFlowMu.Lock()
	if h.microsoftSessionCanceledLocked(req.SessionID, time.Now()) {
		h.microsoftFlowMu.Unlock()
		clearMicrosoftAccountCredential(&account)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if discoveryErr != nil {
		h.microsoftFlowMu.Unlock()
		clearMicrosoftAccountCredential(&account)
		w.WriteHeader(http.StatusGatewayTimeout)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile discovery was canceled or timed out"})
		return
	}
	if len(profiles) > 1 {
		selectionID, expiredSelections, err := h.storeMicrosoftProfileSelection(req.SessionID, account, profiles)
		h.microsoftFlowMu.Unlock()
		discardDetachedMicrosoftProfileSelections(expiredSelections)
		if err != nil {
			clearMicrosoftAccountCredential(&account)
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":                  true,
			"stage":                    "profile",
			"requiresProfileSelection": true,
			"selectionId":              selectionID,
			"profiles":                 profiles,
		})
		return
	}
	if len(profiles) == 1 {
		account.ProfileArn = profiles[0].ARN
	}
	if err := config.AddAccount(account); err != nil {
		h.microsoftFlowMu.Unlock()
		h.writeAddAccountError(w, err)
		return
	}
	delete(h.microsoftCanceled, strings.TrimSpace(req.SessionID))
	h.microsoftFlowMu.Unlock()
	h.pool.Reload()

	response := map[string]interface{}{
		"success": true,
		"stage":   "complete",
		"account": map[string]interface{}{"id": account.ID, "email": account.Email},
	}
	if profileErr != nil {
		response["warning"] = "The account was added, but its Kiro profile could not be resolved yet"
	}
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) apiSelectMicrosoftSSOProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SelectionID string `json:"selectionId"`
		ProfileARN  string `json:"profileArn"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	selectionID := strings.TrimSpace(req.SelectionID)
	selection := h.getMicrosoftProfileSelection(selectionID)
	if selection == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile selection not found or expired"})
		return
	}

	selection.mu.Lock()
	if selection.canceled.Load() || !time.Now().Before(selection.ExpiresAt) {
		selection.mu.Unlock()
		h.removeMicrosoftProfileSelection(selectionID, selection)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile selection not found or expired"})
		return
	}

	profileARN := strings.TrimSpace(req.ProfileARN)
	allowed := false
	for _, profile := range selection.Profiles {
		if profile.ARN == profileARN {
			allowed = true
			break
		}
	}
	if !allowed {
		selection.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Selected Kiro profile was not offered for this login"})
		return
	}

	account := selection.Account
	account.ProfileArn = profileARN
	h.microsoftFlowMu.Lock()
	now := time.Now()
	if selection.canceled.Load() ||
		!now.Before(selection.ExpiresAt) ||
		h.microsoftSessionCanceledLocked(selection.SessionID, now) {
		h.microsoftFlowMu.Unlock()
		h.detachMicrosoftProfileSelection(selectionID, selection)
		selection.canceled.Store(true)
		selection.Account = config.Account{}
		selection.Profiles = nil
		selection.mu.Unlock()
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if err := config.AddAccount(account); err != nil {
		h.microsoftFlowMu.Unlock()
		selection.mu.Unlock()
		h.writeAddAccountError(w, err)
		return
	}
	h.detachMicrosoftProfileSelection(selectionID, selection)
	selection.canceled.Store(true)
	selection.Account = config.Account{}
	selection.Profiles = nil
	delete(h.microsoftCanceled, selection.SessionID)
	h.microsoftFlowMu.Unlock()
	selection.mu.Unlock()
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"stage":   "complete",
		"account": map[string]interface{}{"id": account.ID, "email": account.Email},
	})
}

func (h *Handler) apiCancelMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		SelectionID string `json:"selectionId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	sessionID := strings.TrimSpace(req.SessionID)
	selectionID := strings.TrimSpace(req.SelectionID)
	if sessionID == "" && selectionID != "" {
		if selection := h.getMicrosoftProfileSelection(selectionID); selection != nil {
			sessionID = selection.SessionID
		}
	}
	if sessionID != "" {
		h.markMicrosoftSessionCanceled(sessionID)
	}
	auth.CancelMicrosoftSSOLogin(sessionID)
	if selectionID != "" {
		h.removeMicrosoftProfileSelection(selectionID, nil)
	}
	if sessionID != "" {
		h.removeMicrosoftProfileSelectionsForSession(sessionID)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) storeMicrosoftProfileSelection(
	sessionID string,
	account config.Account,
	profiles []KiroProfile,
) (string, []*microsoftProfileSelection, error) {
	now := time.Now()
	expiresAt := now.Add(microsoftProfileSelectionTTL)
	tokenExpiry := time.Unix(account.ExpiresAt, 0)
	if account.ExpiresAt > 0 && tokenExpiry.Before(expiresAt) {
		expiresAt = tokenExpiry
	}
	if !expiresAt.After(now) {
		return "", nil, fmt.Errorf("Microsoft credential expired before profile selection")
	}
	selectionID := uuid.NewString()
	selection := &microsoftProfileSelection{
		SessionID: strings.TrimSpace(sessionID),
		Account:   account,
		Profiles:  append([]KiroProfile(nil), profiles...),
		ExpiresAt: expiresAt,
	}
	var expired []*microsoftProfileSelection

	h.microsoftSelectionsMu.Lock()
	if h.microsoftSelections == nil {
		h.microsoftSelections = make(map[string]*microsoftProfileSelection)
	}
	for id, current := range h.microsoftSelections {
		if !now.Before(current.ExpiresAt) {
			delete(h.microsoftSelections, id)
			current.canceled.Store(true)
			if current.timer != nil {
				current.timer.Stop()
				current.timer = nil
			}
			expired = append(expired, current)
		}
	}
	if len(h.microsoftSelections) >= microsoftMaxPendingProfileSelections {
		h.microsoftSelectionsMu.Unlock()
		return "", expired, fmt.Errorf("too many pending Microsoft profile selections; cancel one and try again")
	}
	h.microsoftSelections[selectionID] = selection
	selection.timer = time.AfterFunc(time.Until(expiresAt), func() {
		h.removeMicrosoftProfileSelection(selectionID, selection)
	})
	h.microsoftSelectionsMu.Unlock()
	return selectionID, expired, nil
}

func (h *Handler) getMicrosoftProfileSelection(selectionID string) *microsoftProfileSelection {
	if selectionID == "" {
		return nil
	}
	h.microsoftSelectionsMu.Lock()
	selection := h.microsoftSelections[selectionID]
	if selection != nil && !time.Now().Before(selection.ExpiresAt) {
		delete(h.microsoftSelections, selectionID)
		selection.canceled.Store(true)
		if selection.timer != nil {
			selection.timer.Stop()
			selection.timer = nil
		}
		h.microsoftSelectionsMu.Unlock()
		discardDetachedMicrosoftProfileSelection(selection)
		return nil
	}
	h.microsoftSelectionsMu.Unlock()
	return selection
}

func (h *Handler) detachMicrosoftProfileSelection(
	selectionID string,
	expected *microsoftProfileSelection,
) *microsoftProfileSelection {
	selectionID = strings.TrimSpace(selectionID)
	if selectionID == "" {
		return nil
	}
	h.microsoftSelectionsMu.Lock()
	current := h.microsoftSelections[selectionID]
	if current != nil && (expected == nil || current == expected) {
		delete(h.microsoftSelections, selectionID)
		current.canceled.Store(true)
		if current.timer != nil {
			current.timer.Stop()
			current.timer = nil
		}
	} else {
		current = nil
	}
	h.microsoftSelectionsMu.Unlock()
	return current
}

func (h *Handler) removeMicrosoftProfileSelection(selectionID string, expected *microsoftProfileSelection) {
	if selection := h.detachMicrosoftProfileSelection(selectionID, expected); selection != nil {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func (h *Handler) removeMicrosoftProfileSelectionsForSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	var removed []*microsoftProfileSelection
	h.microsoftSelectionsMu.Lock()
	for selectionID, selection := range h.microsoftSelections {
		if selection.SessionID == sessionID {
			delete(h.microsoftSelections, selectionID)
			selection.canceled.Store(true)
			if selection.timer != nil {
				selection.timer.Stop()
				selection.timer = nil
			}
			removed = append(removed, selection)
		}
	}
	h.microsoftSelectionsMu.Unlock()
	for _, selection := range removed {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func discardDetachedMicrosoftProfileSelection(selection *microsoftProfileSelection) {
	selection.mu.Lock()
	selection.Account = config.Account{}
	selection.Profiles = nil
	selection.mu.Unlock()
}

func discardDetachedMicrosoftProfileSelections(selections []*microsoftProfileSelection) {
	for _, selection := range selections {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func clearMicrosoftAccountCredential(account *config.Account) {
	account.AccessToken = ""
	account.RefreshToken = ""
	account.ClientSecret = ""
}

func (h *Handler) markMicrosoftSessionCanceled(sessionID string) {
	now := time.Now()
	h.microsoftFlowMu.Lock()
	if h.microsoftCanceled == nil {
		h.microsoftCanceled = make(map[string]time.Time)
	}
	h.cleanupMicrosoftCanceledLocked(now)
	if len(h.microsoftCanceled) >= microsoftMaxCanceledSessionTombstones {
		var oldestID string
		var oldestExpiry time.Time
		for id, expiry := range h.microsoftCanceled {
			if oldestID == "" || expiry.Before(oldestExpiry) {
				oldestID = id
				oldestExpiry = expiry
			}
		}
		delete(h.microsoftCanceled, oldestID)
	}
	h.microsoftCanceled[sessionID] = now.Add(microsoftCanceledSessionTTL)
	if discovery := h.microsoftDiscoveries[sessionID]; discovery != nil {
		discovery.cancel()
	}
	h.microsoftFlowMu.Unlock()
}

func (h *Handler) beginMicrosoftProfileDiscovery(
	parent context.Context,
	sessionID string,
) (context.Context, *microsoftProfileDiscovery, bool) {
	sessionID = strings.TrimSpace(sessionID)
	h.microsoftFlowMu.Lock()
	defer h.microsoftFlowMu.Unlock()
	if h.microsoftSessionCanceledLocked(sessionID, time.Now()) {
		return nil, nil, false
	}
	if h.microsoftDiscoveries == nil {
		h.microsoftDiscoveries = make(map[string]*microsoftProfileDiscovery)
	}
	if h.microsoftDiscoveries[sessionID] != nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(parent, microsoftProfileDiscoveryTimeout)
	discovery := &microsoftProfileDiscovery{cancel: cancel}
	h.microsoftDiscoveries[sessionID] = discovery
	return ctx, discovery, true
}

func (h *Handler) endMicrosoftProfileDiscovery(sessionID string, expected *microsoftProfileDiscovery) {
	expected.cancel()
	h.microsoftFlowMu.Lock()
	if h.microsoftDiscoveries[strings.TrimSpace(sessionID)] == expected {
		delete(h.microsoftDiscoveries, strings.TrimSpace(sessionID))
	}
	h.microsoftFlowMu.Unlock()
}

func (h *Handler) microsoftSessionCanceled(sessionID string) bool {
	h.microsoftFlowMu.Lock()
	defer h.microsoftFlowMu.Unlock()
	return h.microsoftSessionCanceledLocked(sessionID, time.Now())
}

func (h *Handler) microsoftSessionCanceledLocked(sessionID string, now time.Time) bool {
	h.cleanupMicrosoftCanceledLocked(now)
	expiry, exists := h.microsoftCanceled[strings.TrimSpace(sessionID)]
	return exists && now.Before(expiry)
}

func (h *Handler) cleanupMicrosoftCanceledLocked(now time.Time) {
	for sessionID, expiry := range h.microsoftCanceled {
		if !now.Before(expiry) {
			delete(h.microsoftCanceled, sessionID)
		}
	}
}

func (h *Handler) writeMicrosoftSSOCanceled(w http.ResponseWriter) {
	w.WriteHeader(http.StatusConflict)
	json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft SSO login was canceled"})
}

func (h *Handler) writeAddAccountError(w http.ResponseWriter, err error, rotatedRefreshToken ...string) {
	if errors.Is(err, config.ErrDuplicateAccountID) ||
		errors.Is(err, config.ErrDuplicateRefreshToken) ||
		errors.Is(err, config.ErrDuplicateAPIKey) {
		w.WriteHeader(http.StatusConflict)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
	payload := map[string]string{"error": err.Error()}
	if len(rotatedRefreshToken) > 0 {
		if rotated := strings.TrimSpace(rotatedRefreshToken[0]); rotated != "" {
			// Microsoft may have already invalidated the original refresh token.
			// Surface the rotated value so operators can retry import without a
			// full interactive re-login.
			payload["rotatedRefreshToken"] = rotated
			payload["hint"] = "The identity provider rotated the refresh token before persistence failed; retry import with rotatedRefreshToken"
		}
	}
	json.NewEncoder(w).Encode(payload)
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoginHint string `json:"loginHint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	sessionID, authorizeURL, loopbackPort, err := auth.StartKiroSsoLogin(req.LoginHint)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeURL,
		"loopbackPort": loopbackPort,
	})
}

func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.SessionID == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId is required"})
		return
	}

	status, result, err := auth.PollKiroSsoLogin(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "pending",
		})
		return
	}

	// Flow completed — tạo account
	if result.AccessToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No access token received"})
		return
	}

	// Lấy user info từ login hint (có thể cập nhật sau qua RefreshAccountInfo)
	email := result.UserEmail
	if email == "" {
		email = result.LoginHint
	}

	account := config.Account{
		ID:               auth.GenerateAccountID(),
		Email:            email,
		AccessToken:      result.AccessToken,
		RefreshToken:     result.RefreshToken,
		AuthMethod:       "external_idp",
		Provider:         "MicrosoftEntra",
		IssuerURL:        result.IssuerURL,
		IdPClientID:      result.IdPClientID,
		Scopes:           result.Scopes,
		LoginHint:        result.LoginHint,
		IdPTokenEndpoint: result.IdPTokenEndpoint,
		Region:           "us-east-1",
		ExpiresAt:        time.Now().Unix() + int64(result.ExpiresIn),
		Enabled:          true,
		MachineId:        config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()

	// Resolve profile ARN + fetch account info in background.
	// external_idp accounts: resolve profileArn via ListAvailableProfiles (with
	// TokenType: EXTERNAL_IDP header) first — this is the correct health-check.
	// GetUsageLimits is optional metadata; failure does NOT ban the account.
	go func() {
		// Step 1: Resolve profile ARN (required for runtime generate endpoint)
		if _, err := ResolveProfileArn(&account); err != nil {
			logger.Warnf("[apiPollKiroSso] Profile ARN resolve failed for external_idp account %s: %v", account.Email, err)
		}
		// Step 2: Usage/subscription info (best-effort, non-fatal)
		if _, err := RefreshAccountInfo(&account); err != nil {
			logger.Warnf("[apiPollKiroSso] Account info refresh failed for external_idp account %s: %v", account.Email, err)
		}
		h.modelCache.FetchAndCache(&account)
	}()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "completed",
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	auth.CancelKiroSsoLogin(req.SessionID)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	var req credentialImportPayload
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	req.RefreshToken = strings.TrimSpace(req.RefreshToken)
	req.KiroApiKey = strings.TrimSpace(req.KiroApiKey)
	req.AccessToken = strings.TrimSpace(req.AccessToken)
	methodHint := strings.ToLower(strings.TrimSpace(req.AuthMethod))
	isAPIKeyImport := req.KiroApiKey != "" ||
		methodHint == "api_key" || methodHint == "apikey" ||
		(req.RefreshToken == "" && looksLikeKiroAPIKey(req.AccessToken))
	if isAPIKeyImport && req.KiroApiKey == "" {
		// Allow plain-text / AccessToken-only API key imports.
		req.KiroApiKey = req.AccessToken
	}
	if !isAPIKeyImport && req.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken or kiroApiKey is required"})
		return
	}
	if len(req.RefreshToken) > 512<<10 || len(req.AccessToken) > 512<<10 || len(req.KiroApiKey) > 512<<10 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "credential token is too long"})
		return
	}
	originalRefreshToken := req.RefreshToken
	h.credentialImportMu.Lock()
	defer h.credentialImportMu.Unlock()
	accountID := strings.TrimSpace(req.ID)
	if accountID != "" {
		if _, err := uuid.Parse(accountID); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "id must be a UUID"})
			return
		}
		if config.AccountIDExists(accountID) {
			h.writeAddAccountError(w, config.ErrDuplicateAccountID)
			return
		}
	}

	// API Key import path: no OAuth refresh, no profileArn.
	if isAPIKeyImport {
		if accountID == "" {
			accountID = auth.GenerateAccountID()
		}
		account := config.Account{
			ID:         accountID,
			Email:      strings.TrimSpace(req.Email),
			UserId:     strings.TrimSpace(req.UserID),
			Nickname:   strings.TrimSpace(req.Nickname),
			KiroApiKey: req.KiroApiKey,
			AuthMethod: "api_key",
			Provider:   strings.TrimSpace(req.Provider),
			Region:     strings.TrimSpace(req.Region),
			AuthRegion: strings.TrimSpace(req.AuthRegion),
			ApiRegion:  strings.TrimSpace(req.ApiRegion),
			Enabled:    true,
		}
		// MachineId is deliberately left empty: NormalizeAPIKeyAccount derives a
		// deterministic one from the key, so re-importing the same key yields the
		// same machine identity.
		if err := config.NormalizeAPIKeyAccount(&account); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if config.AccountAPIKeyExists(account.KiroApiKey) {
			h.writeAddAccountError(w, config.ErrDuplicateAPIKey)
			return
		}
		// Best-effort: fetch account info to populate email/usage.
		if info, infoErr := RefreshAccountInfo(&account); infoErr == nil && info != nil {
			if account.Email == "" {
				account.Email = info.Email
			}
			if account.UserId == "" {
				account.UserId = info.UserId
			}
			account.SubscriptionType = info.SubscriptionType
			account.SubscriptionTitle = info.SubscriptionTitle
			account.DaysRemaining = info.DaysRemaining
			account.UsageCurrent = info.UsageCurrent
			account.UsageLimit = info.UsageLimit
			account.UsagePercent = info.UsagePercent
			account.NextResetDate = info.NextResetDate
			account.LastRefresh = info.LastRefresh
		}
		if err := config.AddAccount(account); err != nil {
			h.writeAddAccountError(w, err)
			return
		}
		h.pool.Reload()
		if account.Enabled && account.AccessToken != "" {
			go func(acc config.Account) {
				if err := h.modelCache.FetchAndCache(&acc); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for new API key account %s: %v", acc.Email, err)
				}
			}(account)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"account": map[string]interface{}{
				"id":    account.ID,
				"email": account.Email,
			},
		})
		return
	}

	if config.AccountCredentialExists(originalRefreshToken) {
		h.writeAddAccountError(w, config.ErrDuplicateRefreshToken)
		return
	}

	// 设置默认值
	req.Region = strings.TrimSpace(req.Region)
	if req.Region == "" {
		req.Region = "us-east-1"
	}

	account, status, err := h.importOAuthCredential(req)
	if err != nil {
		var persistErr *credentialPersistError
		if errors.As(err, &persistErr) {
			h.writeAddAccountError(w, persistErr.err, persistErr.rotatedRefreshToken)
			return
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// credentialImportPayload is the request body shared by the credential import
// endpoint and the local-cache importer.
type credentialImportPayload struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	UserID        string `json:"userId"`
	ProfileARN    string `json:"profileArn"`
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken"`
	ClientID      string `json:"clientId"`
	ClientSecret  string `json:"clientSecret"`
	AuthMethod    string `json:"authMethod"`
	Provider      string `json:"provider"`
	Region        string `json:"region"`
	AuthRegion    string `json:"authRegion"`
	ApiRegion     string `json:"apiRegion"`
	KiroApiKey    string `json:"kiroApiKey"`
	Nickname      string `json:"nickname"`
	TokenEndpoint string `json:"tokenEndpoint"`
	IssuerURL     string `json:"issuerUrl"`
	IdPClientID   string `json:"idpClientId"`
	Scopes        string `json:"scopes"`
	LoginHint     string `json:"loginHint"`
}

// credentialPersistError wraps a persistence failure that happened AFTER the
// identity provider rotated the refresh token, so the caller can return the
// rotated value instead of stranding the operator with a dead credential.
type credentialPersistError struct {
	err                 error
	rotatedRefreshToken string
}

func (e *credentialPersistError) Error() string { return e.err.Error() }
func (e *credentialPersistError) Unwrap() error { return e.err }

// importOAuthCredential performs the refresh-token based credential import
// (idc / social / external_idp). It returns the created account, or an HTTP
// status code plus error on failure. Shared by the HTTP handler and the
// local-cache auto-import so both paths behave identically.
func (h *Handler) importOAuthCredential(req credentialImportPayload) (*config.Account, int, error) {
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	if req.RefreshToken == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("refreshToken is required")
	}
	// The token the caller handed us, before any IdP rotation below. Used for the
	// credential fingerprint so re-imports of the same credential collide.
	originalRefreshToken := req.RefreshToken
	// 标准化 authMethod. An absent authMethod is deliberately NOT defaulted here:
	// the classifier below needs to see it empty to consider the implicit
	// external-IdP shape (a bare Microsoft blob carries only clientId +
	// accessToken), and its final default arm covers the plain cases.
	method := strings.ToLower(strings.TrimSpace(req.AuthMethod))
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	derivedTokenEndpoint, derivedIssuer, derivedScopes := auth.DeriveExternalIdpEndpoints(
		req.UserID, req.ClientID, req.AccessToken,
	)
	// Explicit AWS/social methods win over incidental tokenEndpoint/issuerUrl
	// fields so mixed JSON templates cannot force the external-IdP path.
	explicitMicrosoft := method == "external_idp" || method == "external-idp" ||
		method == "externalidp" ||
		method == "external" || method == "microsoft" || method == "m365" || method == "office365" ||
		method == "azure" || method == "azuread" || method == "azure-ad" || method == "azure_ad" ||
		method == "entra" || method == "entra-id" ||
		provider == "external" || provider == "microsoft" || provider == "m365" || provider == "office365" ||
		provider == "azure" || provider == "azuread" || provider == "azure-ad" || provider == "azure_ad" ||
		provider == "entra" || provider == "entra-id"
	implicitExternal := method == "" && provider == "" &&
		(derivedTokenEndpoint != "" ||
			strings.TrimSpace(req.TokenEndpoint) != "" ||
			strings.TrimSpace(req.IssuerURL) != "")
	switch {
	case method == "api_key" || method == "apikey":
		req.AuthMethod = "api_key"
	case method == "idc" || method == "builderid" || method == "enterprise":
		req.AuthMethod = "idc"
	case method == "social" || method == "google" || method == "github":
		req.AuthMethod = "social"
	case explicitMicrosoft || implicitExternal:
		req.AuthMethod = auth.MicrosoftSSOAuthMethod
	case req.ClientID != "" && req.ClientSecret != "":
		req.AuthMethod = "idc"
	default:
		req.AuthMethod = "social"
	}

	// Backend-side external_idp detection: an older cached frontend (or a JSON
	// export missing the authMethod field) can mislabel a Microsoft Entra
	// account as "social", which then refreshes against the wrong endpoint and
	// fails with 401 "Bad credentials". Trust the IdP signals over the label:
	// presence of issuerUrl / idpClientId / a Microsoft-style provider means
	// this is an External IdP account regardless of what authMethod claims.
	//
	// Only "social" is rescued this way. An EXPLICIT idc/api_key from the caller
	// is authoritative — an IdC tenant may legitimately carry a tokenEndpoint or
	// issuerUrl, and forcing it onto the Microsoft path would fail its import
	// with a client_id UUID error.
	if req.AuthMethod == "social" && method != "social" {
		if req.IssuerURL != "" || req.IdPClientID != "" ||
			strings.Contains(strings.ToLower(req.Provider), "entra") ||
			strings.Contains(strings.ToLower(req.Provider), "microsoft") {
			req.AuthMethod = auth.MicrosoftSSOAuthMethod
		}
	}

	// 用 refreshToken 刷新获取新的 accessToken。导入必须以一次成功的刷新为前提：
	// 本地缓存里的 accessToken 不携带可信的过期时间，盲猜短 TTL 会让账号在选号时
	// 永远被跳过，导致后台/按需刷新都无法触发（详见 ensureValidToken 与 Pick 的过期判定）。
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.TokenEndpoint = strings.TrimSpace(req.TokenEndpoint)
	req.IssuerURL = strings.TrimRight(strings.TrimSpace(req.IssuerURL), "/")
	req.Scopes = strings.TrimSpace(req.Scopes)

	if req.AuthMethod == auth.MicrosoftSSOAuthMethod {
		if req.IssuerURL == "" {
			req.IssuerURL = derivedIssuer
		}
		if req.IssuerURL == "" && req.TokenEndpoint != "" {
			normalizedTokenEndpoint, tokenIssuer, tokenScopes := auth.ExternalIdpConfigurationFromTokenEndpoint(
				req.TokenEndpoint, req.ClientID,
			)
			if normalizedTokenEndpoint != "" {
				req.TokenEndpoint = normalizedTokenEndpoint
				req.IssuerURL = tokenIssuer
				if req.Scopes == "" {
					req.Scopes = tokenScopes
				}
			}
		}
		if req.IssuerURL != "" {
			builtTokenEndpoint, normalizedIssuer, builtScopes := auth.ExternalIdpConfigurationFromIssuer(req.IssuerURL, req.ClientID)
			if req.TokenEndpoint == "" {
				req.TokenEndpoint = builtTokenEndpoint
			}
			if normalizedIssuer != "" {
				req.IssuerURL = normalizedIssuer
			}
			if req.Scopes == "" {
				req.Scopes = builtScopes
			}
		}
		if req.TokenEndpoint == "" {
			req.TokenEndpoint = derivedTokenEndpoint
		}
		if req.Scopes == "" {
			req.Scopes = derivedScopes
		}
		normalizedScopes, err := auth.NormalizeExternalIdpScopes(req.Scopes, req.ClientID)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		req.Scopes = normalizedScopes
		if err := auth.ValidateExternalIdpConfiguration(req.ClientID, req.TokenEndpoint, req.IssuerURL, req.Scopes); err != nil {
			return nil, http.StatusBadRequest, err
		}
		req.Provider = auth.MicrosoftSSOProvider
		req.ClientSecret = ""
	}

	profileARN := strings.TrimSpace(req.ProfileARN)
	if profileARN != "" {
		canonicalARN, _, ok := parseKiroProfileArn(profileARN)
		if !ok {
			return nil, http.StatusBadRequest, fmt.Errorf("profileArn is invalid")
		}
		profileARN = canonicalARN
	}

	tempAccount := &config.Account{
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		ClientSecret:  req.ClientSecret,
		AuthMethod:    req.AuthMethod,
		Region:        req.Region,
		TokenEndpoint: req.TokenEndpoint,
		IssuerURL:     req.IssuerURL,
		IdPClientID:   req.IdPClientID,
		Scopes:        req.Scopes,
	}
	accessToken, newRefreshToken, expiresAt, newProfileArn, err := auth.RefreshToken(tempAccount)
	if err != nil {
		// Refresh may fail if token already consumed elsewhere (token rotation).
		// If import payload includes an accessToken, use it directly and let
		// background refresh handle renewal later.
		if req.AccessToken != "" {
			logger.Warnf("[ImportCredentials] Token refresh failed for %s (using provided accessToken): %v", req.AuthMethod, err)
			accessToken = req.AccessToken
			newRefreshToken = req.RefreshToken
			expiresAt = 0 // imported token may be stale; let pool refresh immediately
			newProfileArn = ""
		} else {
			return nil, http.StatusBadRequest, fmt.Errorf("Token refresh failed: %s", err.Error())
		}
	}
	if newRefreshToken != "" {
		req.RefreshToken = newRefreshToken
	}
	rotatedRefreshToken := ""
	if req.RefreshToken != "" && req.RefreshToken != originalRefreshToken {
		rotatedRefreshToken = req.RefreshToken
	}

	// 获取用户信息
	email := strings.TrimSpace(req.Email)
	userID := strings.TrimSpace(req.UserID)
	if req.AuthMethod == auth.MicrosoftSSOAuthMethod {
		tokenEmail, tokenUserID := auth.ExternalIdpTokenIdentity(accessToken)
		if tokenEmail != "" {
			email = tokenEmail
		}
		if tokenUserID != "" {
			userID = tokenUserID
		}
	} else if tokenEmail, _, _ := auth.GetUserInfo(accessToken); tokenEmail != "" {
		email = tokenEmail
	}
	// external_idp: identity lookups may fail for Microsoft-issued tokens; fall
	// back to the loginHint the importer supplied.
	if email == "" && req.LoginHint != "" {
		email = req.LoginHint
	}

	accountID := strings.TrimSpace(req.ID)
	if accountID == "" {
		accountID = auth.GenerateAccountID()
	}
	if profileARN == "" {
		profileARN = newProfileArn
	}
	// Interactive Microsoft login only accepts profiles discovered for the
	// refreshed token. Import keeps the same trust boundary so a client cannot
	// pin an arbitrary data-plane ARN onto a working credential.
	if req.AuthMethod == auth.MicrosoftSSOAuthMethod && profileARN != "" {
		probeAccount := *tempAccount
		probeAccount.AccessToken = accessToken
		probeAccount.RefreshToken = req.RefreshToken
		probeAccount.ExpiresAt = expiresAt
		offeredProfiles, discoverErr := DiscoverKiroProfiles(&probeAccount)
		if discoverErr != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("Unable to verify profileArn against Kiro profiles: %w", discoverErr)
		}
		offered := false
		for _, profile := range offeredProfiles {
			if profile.ARN == profileARN {
				offered = true
				break
			}
		}
		if !offered {
			return nil, http.StatusBadRequest, fmt.Errorf("profileArn was not offered for this credential")
		}
	}
	// 创建账号
	account := config.Account{
		ID:                      accountID,
		Email:                   email,
		UserId:                  userID,
		Nickname:                strings.TrimSpace(req.Nickname),
		AccessToken:             accessToken,
		RefreshToken:            req.RefreshToken,
		RefreshTokenFingerprint: config.RefreshTokenFingerprint(originalRefreshToken),
		ClientID:                req.ClientID,
		ClientSecret:            req.ClientSecret,
		AuthMethod:              req.AuthMethod,
		Provider:                req.Provider,
		Region:                  req.Region,
		AuthRegion:              req.AuthRegion,
		ApiRegion:               req.ApiRegion,
		ExpiresAt:               expiresAt,
		Enabled:                 true,
		MachineId:               config.GenerateMachineId(),
		ProfileArn:              profileARN,
		TokenEndpoint:           req.TokenEndpoint,
		IssuerURL:               req.IssuerURL,
		IdPClientID:             req.IdPClientID,
		Scopes:                  req.Scopes,
		LoginHint:               req.LoginHint,
	}

	if err := config.AddAccount(account); err != nil {
		// The IdP may have already invalidated the refresh token the caller sent.
		// Carry the rotated one out so the HTTP layer can hand it back and the
		// operator can retry import without a full interactive re-login.
		if rotatedRefreshToken != "" {
			return nil, http.StatusInternalServerError, &credentialPersistError{err: err, rotatedRefreshToken: rotatedRefreshToken}
		}
		return nil, http.StatusInternalServerError, err
	}

	// external_idp: resolve profileArn after import (ListAvailableProfiles with TokenType header)
	if req.AuthMethod == "external_idp" {
		go func(acc config.Account) {
			if _, err := ResolveProfileArn(&acc); err != nil {
				logger.Warnf("[ImportCredentials] Profile ARN resolve failed for external_idp account %s: %v", acc.Email, err)
			}
		}(account)
	}

	h.pool.Reload()
	return &account, http.StatusOK, nil
}

// apiScanLocalCache handles GET /admin/api/auth/local-cache/scan.
//
// It scans the local AWS SSO cache (~/.aws/sso/cache) for Kiro IDE credentials
// and returns a non-secret summary of each discovered identity so the UI can
// present them for one-click import. Secrets are never included in the response;
// each entry carries a fingerprint used to reference it in the import call.
//
// Returns available=false (not an error) when no local cache exists, so the UI
// can hide the feature on containerized/remote deployments.
func (h *Handler) apiScanLocalCache(w http.ResponseWriter, r *http.Request) {
	dir, _ := auth.LocalCacheDir()
	creds, err := auth.ScanLocalKiroCredentials()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	type item struct {
		Fingerprint string `json:"fingerprint"`
		SourceFile  string `json:"sourceFile"`
		AuthMethod  string `json:"authMethod"`
		Provider    string `json:"provider"`
		Region      string `json:"region"`
		LoginHint   string `json:"loginHint,omitempty"`
		HasClient   bool   `json:"hasClient"`
		Importable  bool   `json:"importable"`
		Reason      string `json:"reason,omitempty"`
	}
	items := make([]item, 0, len(creds))
	for _, c := range creds {
		it := item{
			Fingerprint: c.Fingerprint,
			SourceFile:  c.SourceFile,
			AuthMethod:  c.AuthMethod,
			Provider:    c.Provider,
			Region:      c.Region,
			LoginHint:   c.LoginHint,
			HasClient:   c.HasClient,
			Importable:  true,
		}
		// IdC tokens need a client registration to refresh; flag if missing.
		if c.AuthMethod == "idc" && !c.HasClient {
			it.Importable = false
			it.Reason = "missing clientId/clientSecret (client registration file not found)"
		}
		items = append(items, it)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"available": len(creds) > 0,
		"cacheDir":  dir,
		"count":     len(items),
		"accounts":  items,
	})
}

// apiImportLocalCache handles POST /admin/api/auth/local-cache/import.
//
// Body: {"fingerprints": ["tok-abcd1234", ...]} — the fingerprints returned by
// the scan endpoint. When omitted or empty, every importable credential found
// in the cache is imported. Each credential is run through the same OAuth import
// path as manual credential import, so region probing and profile resolution
// apply automatically.
func (h *Handler) apiImportLocalCache(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fingerprints []string `json:"fingerprints"`
	}
	// Body is optional; ignore decode errors and treat as "import all".
	_ = json.NewDecoder(r.Body).Decode(&req)

	creds, err := auth.ScanLocalKiroCredentials()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(creds) == 0 {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "no local Kiro credentials found"})
		return
	}

	want := make(map[string]bool, len(req.Fingerprints))
	for _, f := range req.Fingerprints {
		want[f] = true
	}

	type result struct {
		Fingerprint string `json:"fingerprint"`
		SourceFile  string `json:"sourceFile"`
		Success     bool   `json:"success"`
		AccountID   string `json:"accountId,omitempty"`
		Email       string `json:"email,omitempty"`
		Error       string `json:"error,omitempty"`
	}
	var results []result
	imported := 0
	for _, c := range creds {
		if len(want) > 0 && !want[c.Fingerprint] {
			continue
		}
		res := result{Fingerprint: c.Fingerprint, SourceFile: c.SourceFile}

		if c.AuthMethod == "idc" && !c.HasClient {
			res.Error = "missing clientId/clientSecret"
			results = append(results, res)
			continue
		}

		account, _, importErr := h.importOAuthCredential(credentialImportPayload{
			AccessToken:  c.AccessToken,
			RefreshToken: c.RefreshToken,
			ClientID:     c.ClientID,
			ClientSecret: c.ClientSecret,
			AuthMethod:   c.AuthMethod,
			Provider:     c.Provider,
			Region:       c.Region,
			IssuerURL:    c.IssuerURL,
			IdPClientID:  c.IdPClientID,
			Scopes:       c.Scopes,
			LoginHint:    c.LoginHint,
		})
		if importErr != nil {
			res.Error = importErr.Error()
			results = append(results, res)
			continue
		}
		res.Success = true
		res.AccountID = account.ID
		res.Email = account.Email
		imported++
		results = append(results, res)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  imported > 0,
		"imported": imported,
		"total":    len(results),
		"results":  results,
	})
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   h.totalRequests,
		"successRequests": h.successRequests,
		"failedRequests":  h.failedRequests,
		"totalTokens":     h.totalTokens,
		"totalCredits":    h.totalCredits,
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":               config.GetApiKey(),
		"requireApiKey":        config.IsApiKeyRequired(),
		"port":                 config.GetPort(),
		"host":                 config.GetHost(),
		"allowOverUsage":       config.GetAllowOverUsage(),
		"maxPayloadBytes":      config.GetMaxPayloadBytes(),
		"publicModelCatalog":   config.GetPublicModelCatalogRaw(),
		"resolvedModelCatalog": config.GetPublicModelCatalog(),
	})
}

// apiGetSecurity returns the security-hardening config plus a live evaluation of
// weak-config warnings and the caller's resolved IP (for the "your IP" hint).
func (h *Handler) apiGetSecurity(w http.ResponseWriter, r *http.Request) {
	cert, key := config.GetTLSFiles()
	al := config.GetAdminAllowlist()
	if al == nil {
		al = []string{} // serialize as [] not null
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"adminAllowlist": al,
		"trustProxy":     config.GetTrustProxy(),
		"tlsCertFile":    cert,
		"tlsKeyFile":     key,
		"tlsEnabled":     config.IsTLSEnabled(),
		"host":           config.GetHost(),
		"clientIP":       clientIP(r),
		"warnings":       config.EvaluateSecurityWarnings(),
	})
}

// apiUpdateSecurity applies a partial patch to the security settings. Allowlist
// entries are validated as IP or CIDR before persisting. TLS changes require a
// restart to take effect (the listener is bound once at boot).
func (h *Handler) apiUpdateSecurity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AdminAllowlist *[]string `json:"adminAllowlist,omitempty"`
		TrustProxy     *bool     `json:"trustProxy,omitempty"`
		TLSCertFile    *string   `json:"tlsCertFile,omitempty"`
		TLSKeyFile     *string   `json:"tlsKeyFile,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Validate + normalize allowlist entries before persisting.
	if req.AdminAllowlist != nil {
		cleaned := make([]string, 0, len(*req.AdminAllowlist))
		for _, e := range *req.AdminAllowlist {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if strings.Contains(e, "/") {
				if _, _, err := net.ParseCIDR(e); err != nil {
					w.WriteHeader(400)
					json.NewEncoder(w).Encode(map[string]string{"error": "Invalid CIDR: " + e})
					return
				}
			} else if net.ParseIP(e) == nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "Invalid IP: " + e})
				return
			}
			cleaned = append(cleaned, e)
		}
		req.AdminAllowlist = &cleaned
	}

	if err := config.UpdateSecuritySettings(req.AdminAllowlist, req.TrustProxy, req.TLSCertFile, req.TLSKeyFile); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"warnings": config.EvaluateSecurityWarnings(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetUpstreams returns the configured upstream providers and model routes.
// Provider API keys are masked so the admin panel can display entries without
// exposing the full secret.
func (h *Handler) apiGetUpstreams(w http.ResponseWriter, r *http.Request) {
	providers, routes := config.GetUpstreamConfig()
	if providers == nil {
		providers = []config.UpstreamProvider{}
	}
	if routes == nil {
		routes = []config.ModelRoute{}
	}
	masked := make([]map[string]interface{}, len(providers))
	for i, p := range providers {
		masked[i] = publicProvider(p)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"providers": masked,
		"routes":    routes,
	})
}

// apiUpdateUpstreams replaces the upstream providers and model routes atomically.
// Because GET returns masked API keys, an incoming provider key that still looks
// masked (contains "****") is treated as "unchanged" and the previously stored
// secret for that provider ID is preserved.
func (h *Handler) apiUpdateUpstreams(w http.ResponseWriter, r *http.Request) {
	var raw struct {
		Providers []json.RawMessage   `json:"providers"`
		Routes    []config.ModelRoute `json:"routes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	existing, _ := config.GetUpstreamConfig()
	existingByID := make(map[string]config.UpstreamProvider, len(existing))
	for _, p := range existing {
		existingByID[p.ID] = p
	}

	req := struct {
		Providers []config.UpstreamProvider
		Routes    []config.ModelRoute
	}{Routes: raw.Routes}

	for _, blob := range raw.Providers {
		var p config.UpstreamProvider
		if err := json.Unmarshal(blob, &p); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
			return
		}
		var probe struct {
			Connections json.RawMessage `json:"connections"`
		}
		_ = json.Unmarshal(blob, &probe)
		if p.ID == "" {
			p.ID = config.GenerateMachineId()
		}
		stored, hasStored := existingByID[p.ID]
		if strings.Contains(p.ApiKey, "****") && hasStored {
			p.ApiKey = stored.ApiKey
		}
		// Omitted/null connections keep the stored list. An explicit array
		// (including []) replaces it, with masked secrets restored by id.
		if len(probe.Connections) == 0 || string(probe.Connections) == "null" {
			if hasStored {
				p.Connections = stored.Connections
			}
		} else {
			restoreConnectionSecrets(&p, stored)
		}
		req.Providers = append(req.Providers, p)
	}
	for i := range req.Routes {
		if req.Routes[i].ID == "" {
			req.Routes[i].ID = config.GenerateMachineId()
		}
	}

	if err := config.UpdateUpstreamConfig(req.Providers, req.Routes); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func restoreConnectionSecrets(incoming *config.UpstreamProvider, stored config.UpstreamProvider) {
	if incoming == nil {
		return
	}
	byID := make(map[string]string, len(stored.Connections))
	for _, c := range stored.Connections {
		byID[c.ID] = c.ApiKey
	}
	for i := range incoming.Connections {
		c := &incoming.Connections[i]
		if c.ID == "" {
			c.ID = config.GenerateMachineId()
		}
		if c.ApiKey == "" || strings.Contains(c.ApiKey, "****") {
			if old, ok := byID[c.ID]; ok {
				c.ApiKey = old
			}
		}
	}
}

// apiExportUpstreams handles GET /admin/api/upstreams/export.
//
// Unlike apiGetUpstreams, provider API keys are returned UNMASKED, so the file
// can be imported on another host and work immediately. This is only safe
// because every /admin/ path is password-gated (handleAdminAPI) and optionally
// IP-allowlisted (ServeHTTP) before dispatch reaches here — never expose this
// route outside that gate.
func (h *Handler) apiExportUpstreams(w http.ResponseWriter, r *http.Request) {
	bundle := config.ExportUpstreamBundle()
	// The body is plaintext secret material: keep it out of browser and
	// intermediary caches.
	w.Header().Set("Cache-Control", "no-store")
	filename := "kiro-forwarding-" + time.Now().Format("2006-01-02") + ".json"
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	json.NewEncoder(w).Encode(bundle)
}

// apiImportUpstreams handles POST /admin/api/upstreams/import. The body is an
// export bundle produced by apiExportUpstreams.
//
// Merge semantics: existing entries are never overwritten. Duplicate providers
// (matched on baseUrl+name) and duplicate routes (matched on client model) are
// skipped and reported back with a machine-readable reason the UI localizes.
// Returns 400 for a malformed or unrecognized bundle.
func (h *Handler) apiImportUpstreams(w http.ResponseWriter, r *http.Request) {
	var bundle config.UpstreamBundle
	if err := json.NewDecoder(r.Body).Decode(&bundle); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	res, err := config.ImportUpstreamBundle(bundle)
	if err != nil {
		status := 500
		if errors.Is(err, config.ErrInvalidUpstreamBundle) {
			status = 400
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Marshal empty skip lists as [] rather than null.
	skippedProviders := res.SkippedProviders
	if skippedProviders == nil {
		skippedProviders = []config.SkippedItem{}
	}
	skippedRoutes := res.SkippedRoutes
	if skippedRoutes == nil {
		skippedRoutes = []config.SkippedItem{}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":          true,
		"providersAdded":   res.ProvidersAdded,
		"providersSkipped": res.ProvidersSkipped,
		"routesAdded":      res.RoutesAdded,
		"routesSkipped":    res.RoutesSkipped,
		"skippedProviders": skippedProviders,
		"skippedRoutes":    skippedRoutes,
	})
}

// resolveUpstreamCreds resolves the base URL, API key and proxy for an upstream
// probe request. The admin panel may send a provider ID (referring to a stored
// provider) with a masked/empty key, in which case the stored secret and proxy
// are used. Explicit baseURL/apiKey in the request override the stored values.
func resolveUpstreamCreds(id, connectionID, baseURL, apiKey, proxyURL string) (string, string, string) {
	if id != "" {
		providers, _ := config.GetUpstreamConfig()
		for _, p := range providers {
			if p.ID != id {
				continue
			}
			if baseURL == "" {
				baseURL = p.BaseURL
			}
			if apiKey == "" || strings.Contains(apiKey, "****") {
				if connectionID != "" {
					if c, ok := config.FindConnection(p, connectionID); ok {
						apiKey = c.ApiKey
					}
				} else {
					apiKey = config.FirstEnabledAPIKey(p)
				}
			}
			if proxyURL == "" {
				proxyURL = p.ProxyURL
			}
			break
		}
	}
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	return baseURL, apiKey, proxyURL
}

// apiUpstreamModels fetches the model list from an upstream's /models endpoint so
// the admin panel can browse available models (like 9router's "Import from /models").
func (h *Handler) apiUpstreamModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		BaseURL  string `json:"baseUrl"`
		ApiKey   string `json:"apiKey"`
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	baseURL, apiKey, proxyURL := resolveUpstreamCreds(req.ID, "", req.BaseURL, req.ApiKey, req.ProxyURL)
	if strings.TrimSpace(baseURL) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "baseUrl is required"})
		return
	}

	url := strings.TrimRight(baseURL, "/") + "/models"
	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	httpReq.Header.Set("Accept", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("X-Api-Key", apiKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	}

	client := GetClientForProxy(proxyURL)
	resp, err := client.Do(httpReq)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": "upstream request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))

	if resp.StatusCode != 200 {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":  "upstream returned status " + strconv.Itoa(resp.StatusCode),
			"status": resp.StatusCode,
			"body":   string(respBody),
		})
		return
	}

	// Parse both OpenAI ({"data":[{"id":...}]}) and bare-array shapes.
	models := parseModelIDs(respBody)
	json.NewEncoder(w).Encode(map[string]interface{}{"models": models})
}

// parseModelIDs extracts model id strings from a /models response, tolerating the
// OpenAI {"data":[{"id"}]} shape, an Anthropic {"data":[{"id"}]} shape, a
// {"models":[...]} shape, or a bare JSON array of objects/strings.
func parseModelIDs(body []byte) []string {
	ids := make([]string, 0, 16)
	seen := make(map[string]bool)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			ids = append(ids, s)
		}
	}
	extract := func(items []json.RawMessage) {
		for _, it := range items {
			var obj map[string]interface{}
			if err := json.Unmarshal(it, &obj); err == nil {
				if id, ok := obj["id"].(string); ok {
					add(id)
					continue
				}
				if name, ok := obj["name"].(string); ok {
					add(name)
					continue
				}
			}
			var s string
			if err := json.Unmarshal(it, &s); err == nil {
				add(s)
			}
		}
	}
	var wrapped struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil {
		if len(wrapped.Data) > 0 {
			extract(wrapped.Data)
		}
		if len(wrapped.Models) > 0 {
			extract(wrapped.Models)
		}
	}
	if len(ids) == 0 {
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err == nil {
			extract(arr)
		}
	}
	return ids
}

// apiUpstreamTest sends a minimal request to an upstream model and reports whether
// it responded, the HTTP status, and the round-trip latency — like 9router's
// per-model test button.
func (h *Handler) apiUpstreamTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID           string `json:"id"`
		ConnectionID string `json:"connectionId"`
		BaseURL      string `json:"baseUrl"`
		ApiKey       string `json:"apiKey"`
		ProxyURL     string `json:"proxyURL"`
		Model        string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "model is required"})
		return
	}
	h.runUpstreamTest(w, r, req.ID, req.ConnectionID, req.BaseURL, req.ApiKey, req.ProxyURL, req.Model)
}

// runUpstreamTest is the shared body behind both the per-provider test button and
// the per-connection one. connectionID selects WHICH stored key is probed; empty
// means "the provider's first enabled key", which is what a provider-level test
// has always meant.
func (h *Handler) runUpstreamTest(w http.ResponseWriter, r *http.Request, id, connectionID, baseURL, apiKey, proxyURL, model string) {
	baseURL, apiKey, proxyURL = resolveUpstreamCreds(id, connectionID, baseURL, apiKey, proxyURL)
	if strings.TrimSpace(baseURL) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "baseUrl is required"})
		return
	}

	// One body satisfies both APIs — model + max_tokens + a single user message is
	// valid for OpenAI's /chat/completions and Anthropic's /messages alike — so the
	// shapes differ only in path and version header.
	payload, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})

	// Bounded: GetClientForProxy carries a 5-minute timeout meant for relayed SSE
	// streams, and an operator clicking Test on a black-holing host must not wait
	// that long — twice, now that two shapes may be tried.
	ctx, cancel := context.WithTimeout(r.Context(), upstreamTestTimeout)
	defer cancel()
	client := GetClientForProxy(proxyURL)

	status, latency, body, path, err := probeUpstream(ctx, client, baseURL, apiKey, payload)
	if err != nil {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":        false,
			"error":     upstreamTestErrMsg(ctx, err),
			"path":      path,
			"latencyMs": latency,
		})
		return
	}

	result := map[string]interface{}{
		"ok":        status >= 200 && status < 300,
		"status":    status,
		"latencyMs": latency,
		// Which API answered. A target that only speaks one of the two shapes still
		// forwards fine for clients hitting that side, so the operator needs to see
		// WHICH one the probe got through on, not just that something did.
		"path": path,
	}
	if status < 200 || status >= 300 {
		result["error"] = string(body)
	}
	json.NewEncoder(w).Encode(result)
}

// upstreamTestErrMsg labels a probe that never got an answer. Our own deadline is
// the common case and needs its own word: the transport error for it reads
// "context deadline exceeded", which describes the mechanism and not the fact the
// operator needs, that this upstream did not respond in time.
func upstreamTestErrMsg(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case errors.Is(ctx.Err(), context.Canceled):
		return "canceled"
	default:
		return err.Error()
	}
}

// probeUpstream sends the minimal probe to an upstream and returns the outcome of
// the attempt that decided it, including which path answered.
//
// It tries OpenAI's /chat/completions first and falls back to Anthropic's
// /messages, because the path a REAL request takes is chosen by the client API the
// caller hit, not by the provider: tryForwardUpstream appends "/messages" for a
// Claude-API request and "/chat/completions" for an OpenAI one (see its subPath
// argument). An upstream that implements only one of the two is perfectly usable
// for the half it serves, so probing a single shape and reporting its 404 would
// call a working target broken.
//
// The fallback is deliberately narrow. Only 404/405 — "this API is not served
// here" — moves on; a 401, 429 or 5xx is the upstream answering on a path it does
// own, and must be reported as-is rather than masked by a second probe. A
// transport error stops immediately: nothing answered, so the path is not the
// question.
func probeUpstream(ctx context.Context, client *http.Client, baseURL, apiKey string, payload []byte) (int, int64, []byte, string, error) {
	shapes := []struct {
		path  string
		extra map[string]string
	}{
		{path: "/chat/completions"},
		{path: "/messages", extra: map[string]string{"anthropic-version": "2023-06-01"}},
	}
	var (
		status  int
		latency int64
		body    []byte
		path    string
		err     error
	)
	for _, shape := range shapes {
		path = shape.path
		status, latency, body, err = probeUpstreamOnce(ctx, client, baseURL, apiKey, shape.path, payload, shape.extra)
		if err != nil {
			return status, latency, body, path, err
		}
		if status >= 200 && status < 300 {
			return status, latency, body, path, nil
		}
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			return status, latency, body, path, nil
		}
	}
	return status, latency, body, path, err
}

// probeUpstreamOnce sends one probe. A non-2xx status is NOT an error here: only
// the caller knows whether that status means "try the other shape".
func probeUpstreamOnce(ctx context.Context, client *http.Client, baseURL, apiKey, path string, payload []byte, extra map[string]string) (int, int64, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(baseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return 0, 0, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	for k, v := range extra {
		httpReq.Header.Set(k, v)
	}
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("X-Api-Key", apiKey)
	}
	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, time.Since(start).Milliseconds(), nil, err
	}
	defer resp.Body.Close()
	latency := time.Since(start).Milliseconds()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, latency, body, nil
}

// apiGetPublicIP probes an external service to discover the machine's public
// (internet-facing) IP. This is best-effort: reaching an external endpoint may
// fail behind restrictive networks, and the returned IP is only reachable from the
// internet if the operator has set up port-forwarding or a tunnel on their router.
func (h *Handler) apiGetPublicIP(w http.ResponseWriter, r *http.Request) {
	client := GetClientForProxy(config.GetProxyURL())
	req, err := http.NewRequest("GET", "https://api.ipify.org", nil)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ip := strings.TrimSpace(string(body))
	if resp.StatusCode != 200 || net.ParseIP(ip) == nil {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "could not determine public IP"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "ip": ip, "port": config.GetPort()})
}

// apiGetForwardStats returns aggregate metrics: overall totals, per-provider and
// per-route breakdowns, the rolling time-series, and latency percentiles.
//
// Providers are joined against the upstream config so the response also carries
// the configured display name, base URL and prices for providers that exist but
// have no traffic yet, plus the synthetic Kiro-pool entry.
//
// An `hours` query restricts the per-provider figures to that window, which is
// what the Stats tab's range selector sends. overall/percentiles stay all-time:
// they feed the Forwarding tab's KPI cards, which are not range-scoped.
func (h *Handler) apiGetForwardStats(w http.ResponseWriter, r *http.Request) {
	minutes := 60
	if v := strings.TrimSpace(r.URL.Query().Get("minutes")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minutes = n
		}
	}
	hours := 0
	if v := strings.TrimSpace(r.URL.Query().Get("hours")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"overall":     metrics.Overall(),
		"providers":   mergedProviderStats(hours),
		"routes":      metrics.RouteStats(),
		"timeseries":  metrics.TimeSeries(minutes),
		"percentiles": metrics.LatencyPercentiles(),
		"windowHours": hours,
	})
}

// apiGetTopIPs returns the source IPs that have generated the most requests.
// The optional ?limit= caps the list (default 50); limit<=0 returns all.
func (h *Handler) apiGetTopIPs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	ips := metrics.TopIPs(limit)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ips":        ips,
		"trustProxy": config.GetTrustProxy(),
	})
}

// providerRow is a provider's recorded stats plus the configuration context the
// dashboard needs to render and label it.
type providerRow struct {
	metrics.ProviderStat
	BaseURL      string  `json:"baseUrl,omitempty"`
	Enabled      bool    `json:"enabled"`
	Configured   bool    `json:"configured"`
	IsPool       bool    `json:"isPool"`
	PriceInPerM  float64 `json:"priceInPerM,omitempty"`
	PriceOutPerM float64 `json:"priceOutPerM,omitempty"`
}

// mergedProviderStats joins recorded metrics with the configured upstreams so
// the comparison table lists every provider — including ones configured but not
// yet used (which would otherwise be invisible) and ones whose config was
// deleted but whose history remains.
//
// hours > 0 scopes the recorded figures to that window; 0 means all-time.
func mergedProviderStats(hours int) []providerRow {
	stats := metrics.ProviderStats()
	if hours > 0 {
		stats = metrics.ProviderStatsWindow(hours)
	}
	byID := make(map[string]metrics.ProviderStat, len(stats))
	for _, p := range stats {
		byID[p.ProviderID] = p
	}

	providers, _ := config.GetUpstreamConfig()
	rows := make([]providerRow, 0, len(stats)+len(providers))
	seen := make(map[string]bool, len(stats))

	for _, up := range providers {
		st, ok := byID[up.ID]
		if !ok {
			st = metrics.ProviderStat{ProviderID: up.ID, SuccessRate: -1}
		}
		// The configured name wins: renaming a provider should retitle its history
		// rather than leave the old label attached to past traffic.
		st.ProviderName = up.Name
		rows = append(rows, providerRow{
			ProviderStat: st,
			BaseURL:      up.BaseURL,
			Enabled:      up.Enabled,
			Configured:   true,
			PriceInPerM:  up.PriceInPerM,
			PriceOutPerM: up.PriceOutPerM,
		})
		seen[up.ID] = true
	}

	for _, p := range stats {
		if seen[p.ProviderID] {
			continue
		}
		rows = append(rows, providerRow{
			ProviderStat: p,
			// The pool is always "configured" in the sense that it cannot be
			// deleted; anything else here is orphaned history.
			Configured: p.ProviderID == metrics.KiroPoolID,
			Enabled:    p.ProviderID == metrics.KiroPoolID,
			IsPool:     p.ProviderID == metrics.KiroPoolID,
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Requests > rows[j].Requests })
	return rows
}

// apiGetProviderDetail returns the full drill-down for one provider:
// per-model and per-account breakdowns, the status histogram, recent errors,
// percentiles and the per-minute series.
//
// An `hours` query scopes the headline ProviderStat numbers to that window,
// matching what the Stats tab's range selector sends to /forward-stats.
// Models/Accounts/Statuses are always all-time (buckets carry no per-dimension
// breakdowns). Omitting hours or passing hours=0 returns all-time totals.
func (h *Handler) apiGetProviderDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "id is required"})
		return
	}
	minutes := 60
	if v := strings.TrimSpace(r.URL.Query().Get("minutes")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minutes = n
		}
	}
	hours := 0
	if v := strings.TrimSpace(r.URL.Query().Get("hours")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}

	detail, ok := metrics.ProviderDetailWindow(id, hours, minutes)
	if !ok {
		// A configured provider with no traffic yet is a valid, empty detail —
		// not a 404 — so the panel renders zeros instead of an error.
		detail = metrics.ProviderDetail{
			ProviderStat: metrics.ProviderStat{ProviderID: id, SuccessRate: -1},
			Minutes:      []metrics.Bucket{},
		}
	}

	resp := map[string]interface{}{"detail": detail}
	if id == metrics.KiroPoolID {
		resp["isPool"] = true
		resp["name"] = metrics.KiroPoolName
	} else if providers, _ := config.GetUpstreamConfig(); providers != nil {
		for _, up := range providers {
			if up.ID != id {
				continue
			}
			resp["name"] = up.Name
			resp["baseUrl"] = up.BaseURL
			resp["enabled"] = up.Enabled
			resp["priceInPerM"] = up.PriceInPerM
			resp["priceOutPerM"] = up.PriceOutPerM
			break
		}
	}
	json.NewEncoder(w).Encode(resp)
}

// apiGetForwardHistory returns rollups for the trend chart. An empty id
// aggregates across all providers.
//
// Granularity follows the requested range: hours=1 would produce a single hourly
// bar, so sub-day ranges are served from the per-minute buckets instead and the
// response reports which unit was used.
func (h *Handler) apiGetForwardHistory(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v := strings.TrimSpace(r.URL.Query().Get("hours")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	if hours <= 1 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"providerId": id,
			"hours":      hours,
			"unit":       "minute",
			"buckets":    metrics.MinuteHistoryFor(id, 60),
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"providerId": id,
		"hours":      hours,
		"unit":       "hour",
		"buckets":    metrics.HistoryFor(id, hours),
	})
}

// apiResetProviderStats clears the counters and history for a single provider,
// leaving every other provider untouched, then persists the new state.
func (h *Handler) apiResetProviderStats(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "id is required"})
		return
	}
	if !metrics.ResetProvider(id) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "no stats for that provider"})
		return
	}
	if err := metrics.Save(forwardMetricsPath()); err != nil {
		logger.Warnf("[Metrics] failed to save after provider reset: %v", err)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// apiExportForwardEvents streams the filtered event log as CSV or JSON for
// offline analysis. It accepts the same filters as apiGetForwardEvents but
// ignores pagination, exporting every match up to exportEventsMax.
func (h *Handler) apiExportForwardEvents(w http.ResponseWriter, r *http.Request) {
	const exportEventsMax = 5000

	q := r.URL.Query()
	items, total := metrics.Events(eventFilterFromQuery(q, 0, exportEventsMax))

	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="forward-events.json"`)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"exportedAt": time.Now().UnixMilli(),
			"total":      total,
			"exported":   len(items),
			"items":      items,
		})
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="forward-events.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"time", "provider", "account", "endpoint", "clientModel", "targetModel",
		"status", "ok", "canceled", "stream", "latencyMs", "ttfbMs",
		"inputTokens", "outputTokens", "costUsd", "error",
	})
	for _, e := range items {
		_ = cw.Write([]string{
			time.UnixMilli(e.TimeMs).UTC().Format(time.RFC3339),
			e.ProviderName,
			e.AccountLabel,
			e.Endpoint,
			e.ClientModel,
			e.TargetModel,
			strconv.Itoa(e.Status),
			strconv.FormatBool(e.Ok),
			strconv.FormatBool(e.Canceled),
			strconv.FormatBool(e.Stream),
			strconv.FormatInt(e.LatencyMs, 10),
			strconv.FormatInt(e.TTFBMs, 10),
			strconv.FormatInt(e.InputTokens, 10),
			strconv.FormatInt(e.OutputTokens, 10),
			strconv.FormatFloat(e.CostUSD, 'f', -1, 64),
			e.ErrorMsg,
		})
	}
}

// eventFilterFromQuery builds an EventFilter from admin query parameters,
// shared by the paged log view and the export endpoint.
func eventFilterFromQuery(q url.Values, offset, limit int) metrics.EventFilter {
	atoi64 := func(s string) int64 {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	code := 0
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("code"))); err == nil {
		code = n
	}
	return metrics.EventFilter{
		ProviderID: strings.TrimSpace(q.Get("provider")),
		AccountID:  strings.TrimSpace(q.Get("account")),
		Status:     strings.TrimSpace(q.Get("status")),
		StatusCode: code,
		Model:      strings.TrimSpace(q.Get("model")),
		SinceMs:    atoi64(q.Get("since")),
		UntilMs:    atoi64(q.Get("until")),
		Offset:     offset,
		Limit:      limit,
	}
}

// apiResetForwardStats clears all forward metrics and persists the empty state.
func (h *Handler) apiResetForwardStats(w http.ResponseWriter, r *http.Request) {
	metrics.Reset()
	if err := metrics.Save(forwardMetricsPath()); err != nil {
		logger.Warnf("[Metrics] failed to save after reset: %v", err)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// apiGetForwardEvents returns events newest-first with optional
// provider/account/status/code/model/time-range filters and offset/limit
// pagination.
func (h *Handler) apiGetForwardEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset := 0
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("offset"))); err == nil && n > 0 {
		offset = n
	}
	limit := 50
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil && n > 0 {
		limit = n
	}
	items, total := metrics.Events(eventFilterFromQuery(q, offset, limit))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"items":  items,
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
}

// apiStreamForwardEvents streams live forward events over SSE. It mirrors
// apiStreamLogs: backfill recent events, then push live ones, coalescing bursts
// into a single flush and emitting periodic pings to keep the connection open.
func (h *Handler) apiStreamForwardEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Streaming not supported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEvent := func(e metrics.Event) {
		data, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}

	// Backfill the most recent events (newest-first from Events), oldest first
	// so the client can append in chronological order.
	recent, _ := metrics.Events(metrics.EventFilter{Limit: 50})
	for i := len(recent) - 1; i >= 0; i-- {
		writeEvent(recent[i])
	}
	flusher.Flush()

	ch, cancel := metrics.Subscribe()
	defer cancel()

	ctx := r.Context()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			writeEvent(e)
			for drained := true; drained; {
				select {
				case e2 := <-ch:
					writeEvent(e2)
				default:
					drained = false
				}
			}
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey             *string `json:"apiKey,omitempty"`
		RequireApiKey      *bool   `json:"requireApiKey,omitempty"`
		Password           string  `json:"password,omitempty"`
		AllowOverUsage     *bool   `json:"allowOverUsage,omitempty"`
		MaxPayloadBytes    *int    `json:"maxPayloadBytes,omitempty"`
		PublicModelCatalog *string `json:"publicModelCatalog,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := config.UpdateSettingsPatch(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 更新超额使用设置
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Rebuild the pool so over-quota accounts are re-included or dropped immediately.
		h.pool.Reload()
	}

	// maxPayloadBytes is read per-request by truncatePayloadToLimit, so the new
	// value takes effect on the next request — no restart or pool reload needed.
	if req.MaxPayloadBytes != nil {
		if err := config.UpdateMaxPayloadBytes(*req.MaxPayloadBytes); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if req.PublicModelCatalog != nil {
		if err := config.UpdatePublicModelCatalog(*req.PublicModelCatalog); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": h.getRequestLogs(),
	})
}

func (h *Handler) apiClearLogs(w http.ResponseWriter, r *http.Request) {
	h.requestLogsMu.Lock()
	h.requestLogs = h.requestLogs[:0]
	h.requestLogsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
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

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPIContext(r.Context(), account, kiroPayload, callback)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
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

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		_, err := h.refreshAccountToken(account, true)
		return err
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// 检查是否为封禁相关错误
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") || strings.Contains(errMsg, "Account suspended") {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 如果是 403/401，说明 token 无效，尝试刷新后重试
		if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				// 重试
				info, err = RefreshAccountInfo(account)
				if err != nil {
					// 重试后仍然失败，检查是否为封禁状态
					if strings.Contains(err.Error(), "TEMPORARILY_SUSPENDED") || strings.Contains(err.Error(), "Account suspended") {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// 其他错误才显示错误信息
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 保存到配置
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
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

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       account.AccessToken,
		"refreshToken":      account.RefreshToken,
		"clientId":          account.ClientID,
		"clientSecret":      account.ClientSecret,
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"issuerUrl":         account.IssuerURL,
		"idpClientId":       account.IdPClientID,
		"scopes":            account.Scopes,
		"loginHint":         account.LoginHint,
		"region":            account.Region,
		"profileArn":        account.ProfileArn,
		"tokenEndpoint":     account.TokenEndpoint,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"overageStatus":     account.OverageStatus,
		"overageCapability": account.OverageCapability,
		"overageCap":        account.OverageCap,
		"overageRate":       account.OverageRate,
		"currentOverages":   account.CurrentOverages,
		"overageCheckedAt":  account.OverageCheckedAt,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
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

	models, err := h.modelCache.ListModels(r.Context(), account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Publish through the gated path so a concurrent profile switch wins and the
	// aggregate rebuild drops any stale models, instead of the old grow-only merge.
	h.modelCache.PublishGated(id, account.ProfileArn, account.EffectiveApiRegion(), models)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}

// logEntryJSON is the wire shape for a captured log line.
type logEntryJSON struct {
	Ts    int64  `json:"ts"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

func toLogEntryJSON(e logger.Entry) logEntryJSON {
	return logEntryJSON{
		Ts:    e.Time.UnixMilli(),
		Level: logger.LevelName(e.Level),
		Text:  e.Text,
	}
}

// apiGetConsoleHistory GET /admin/api/logs/history - returns retained logger
// history (the SSE console's fallback when EventSource is unavailable). Distinct
// from apiGetLogs, which serves the per-request RequestLog ring buffer.
func (h *Handler) apiGetConsoleHistory(w http.ResponseWriter, r *http.Request) {
	hist := logger.History()
	out := make([]logEntryJSON, 0, len(hist))
	for _, e := range hist {
		out = append(out, toLogEntryJSON(e))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"level": logger.LevelName(logger.GetLevel()),
		"logs":  out,
	})
}

// apiStreamLogs GET /admin/api/logs/stream - Server-Sent Events stream of log lines.
func (h *Handler) apiStreamLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Streaming not supported"})
		return
	}

	// handleAdminAPI sets application/json; override for SSE.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEntry := func(e logger.Entry) {
		data, _ := json.Marshal(toLogEntryJSON(e))
		fmt.Fprintf(w, "data: %s\n\n", data)
	}

	// Backfill retained history first, then stream live entries.
	for _, e := range logger.History() {
		writeEntry(e)
	}
	flusher.Flush()

	ch, cancel := logger.Subscribe()
	defer cancel()

	ctx := r.Context()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			writeEntry(e)
			// Coalesce: drain everything already queued and write it in one
			// batch so a burst costs a single flush (one socket write) instead
			// of one per line.
			for drained := true; drained; {
				select {
				case e2 := <-ch:
					writeEntry(e2)
				default:
					drained = false
				}
			}
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// apiGetLogLevel GET /admin/api/logs/level - returns the active log level.
func (h *Handler) apiGetLogLevel(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"level": logger.LevelName(logger.GetLevel())})
}

// apiSetLogLevel POST /admin/api/logs/level - changes the active log level at runtime and persists it.
func (h *Handler) apiSetLogLevel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	lvl, ok := logger.ParseLevel(req.Level)
	if !ok {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid level, must be: debug, info, warn, or error"})
		return
	}
	logger.SetLevel(lvl)
	if err := config.UpdateLogLevel(logger.LevelName(lvl)); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"level": logger.LevelName(lvl)})
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":                cfg.Suffix,
		"openaiFormat":          cfg.OpenAIFormat,
		"claudeFormat":          cfg.ClaudeFormat,
		"defaultEffort":         cfg.DefaultEffort,
		"advertiseEffortModels": cfg.AdvertiseEffortModels,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	// The effort fields are pointers so an omitted key keeps its stored value.
	// The legacy admin page (web/index-legacy.html) still POSTs the original
	// three-field body, and treating "absent" as "clear it" would silently reset
	// the default level and the /v1/models toggle whenever that page saved.
	var req struct {
		Suffix                string  `json:"suffix"`
		OpenAIFormat          string  `json:"openaiFormat"`
		ClaudeFormat          string  `json:"claudeFormat"`
		DefaultEffort         *string `json:"defaultEffort"`
		AdvertiseEffortModels *bool   `json:"advertiseEffortModels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	current := config.GetThinkingConfig()

	// Store the default effort in canonical form so the request path can compare
	// it against the enum without re-normalizing on every turn. "" and "auto"
	// both mean "let the model choose", and both persist as "".
	defaultEffort := current.DefaultEffort
	if req.DefaultEffort != nil {
		level, ok := NormalizeThinkingEffort(*req.DefaultEffort)
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid defaultEffort, must be one of: auto, " + ThinkingEffortValues()})
			return
		}
		defaultEffort = ""
		if level != EffortUnset && level != EffortAuto {
			defaultEffort = string(level)
		}
	}

	advertiseEffortModels := current.AdvertiseEffortModels
	if req.AdvertiseEffortModels != nil {
		advertiseEffortModels = *req.AdvertiseEffortModels
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat, defaultEffort, advertiseEffortModels); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proxyURL": config.GetProxyURL(),
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" {
		if !strings.HasPrefix(req.ProxyURL, "http://") &&
			!strings.HasPrefix(req.ProxyURL, "https://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5h://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置
	applyProxyConfig(req.ProxyURL)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// netAddrInfo 描述一个可用于访问的网络地址
type netAddrInfo struct {
	IP         string `json:"ip"`         // IPv4 地址
	Iface      string `json:"iface"`      // 网卡名称 (en0, eth0...)
	IsLoopback bool   `json:"isLoopback"` // 是否为回环地址
}

// apiGetNetInterfaces 返回本机所有 IPv4 地址，供前端生成 LAN 访问 URL。
// 仅在服务器绑定 0.0.0.0 时，LAN 地址才真正可达；bound 字段告知前端当前监听地址。
func (h *Handler) apiGetNetInterfaces(w http.ResponseWriter, r *http.Request) {
	host := config.GetHost()
	// 当绑定到 0.0.0.0 或空时，服务监听所有网卡，LAN 可达
	listensAll := host == "0.0.0.0" || host == "" || host == "::"

	addrs := make([]netAddrInfo, 0, 4)
	// 始终包含 localhost 作为默认项
	addrs = append(addrs, netAddrInfo{IP: "127.0.0.1", Iface: "localhost", IsLoopback: true})

	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			// 跳过未启用的网卡
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			ifAddrs, aerr := iface.Addrs()
			if aerr != nil {
				continue
			}
			for _, a := range ifAddrs {
				var ip net.IP
				switch v := a.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip == nil || ip.IsLoopback() {
					continue
				}
				ip4 := ip.To4()
				if ip4 == nil {
					continue // 仅返回 IPv4
				}
				addrs = append(addrs, netAddrInfo{IP: ip4.String(), Iface: iface.Name})
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"port":       config.GetPort(),
		"listensAll": listensAll,
		"host":       host,
		"addrs":      addrs,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken   string `json:"accessToken"`
		CsrfToken     string `json:"csrfToken"`
		RefreshToken  string `json:"refreshToken"`
		ClientID      string `json:"clientId,omitempty"`
		ClientSecret  string `json:"clientSecret,omitempty"`
		Region        string `json:"region,omitempty"`
		ExpiresAt     int64  `json:"expiresAt"`
		AuthMethod    string `json:"authMethod,omitempty"`
		Provider      string `json:"provider,omitempty"`
		TokenEndpoint string `json:"tokenEndpoint,omitempty"`
		IssuerURL     string `json:"issuerUrl,omitempty"`
		IdPClientID   string `json:"idpClientId,omitempty"`
		Scopes        string `json:"scopes,omitempty"`
		LoginHint     string `json:"loginHint,omitempty"`
		// ProfileArn / IdpTokenEndpoint are needed to reconstruct a working
		// kiro-auth-token.json (IDE) without a fresh login. Older Kiro Account
		// Manager exports omit them; consumers that don't need them ignore them.
		ProfileArn       string `json:"profileArn,omitempty"`
		IdpTokenEndpoint string `json:"idpTokenEndpoint,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		ProfileArn   string             `json:"profileArn,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// API Key accounts are not OAuth credentials; export a flat-compatible shape.
		if config.IsAPIKeyAccount(&a) {
			exportAccounts = append(exportAccounts, ExportAccount{
				ID:        a.ID,
				Email:     a.Email,
				Nickname:  a.Nickname,
				Idp:       "APIKey",
				UserId:    a.UserId,
				MachineId: a.MachineId,
				Credentials: ExportCredentials{
					AccessToken:  a.KiroApiKey,
					RefreshToken: "",
					Region:       a.Region,
					AuthMethod:   "api_key",
					Provider:     "APIKey",
				},
				Subscription: ExportSubscription{
					Type:  a.SubscriptionType,
					Title: a.SubscriptionTitle,
				},
				Usage: ExportUsage{
					Current:     a.UsageCurrent,
					Limit:       a.UsageLimit,
					PercentUsed: a.UsagePercent,
					LastUpdated: a.LastRefresh,
				},
				Tags:       []string{"api_key"},
				Status:     "active",
				CreatedAt:  0,
				LastUsedAt: a.LastUsed,
			})
			continue
		}

		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else if a.AuthMethod == auth.MicrosoftSSOAuthMethod {
				idp = auth.MicrosoftSSOProvider
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:         a.ID,
			Email:      a.Email,
			Nickname:   a.Nickname,
			Idp:        idp,
			UserId:     a.UserId,
			ProfileArn: a.ProfileArn,
			MachineId:  a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:      a.AccessToken,
				CsrfToken:        "",
				RefreshToken:     a.RefreshToken,
				ClientID:         a.ClientID,
				ClientSecret:     a.ClientSecret,
				Region:           a.Region,
				ExpiresAt:        a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:       authMethod,
				Provider:         a.Provider,
				TokenEndpoint:    a.TokenEndpoint,
				IssuerURL:        a.IssuerURL,
				IdPClientID:      a.IdPClientID,
				Scopes:           a.Scopes,
				LoginHint:        a.LoginHint,
				ProfileArn:       a.ProfileArn,
				IdpTokenEndpoint: a.IdPTokenEndpoint,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
