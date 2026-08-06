// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	ErrAccountNotFound       = errors.New("account not found")
	ErrDuplicateAccountID    = errors.New("account ID already exists")
	ErrDuplicateRefreshToken = errors.New("account refresh token already exists")
	ErrDuplicateAPIKey       = errors.New("account API key already exists")
	ErrEmptyAPIKey           = errors.New("kiroApiKey is empty")
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken  string `json:"accessToken"`  // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"` // OAuth refresh token for token renewal
	// RefreshTokenFingerprint is a one-way identifier for the credential that
	// originally created this account. It prevents a previously imported token
	// from being imported again after the provider rotates it.
	RefreshTokenFingerprint string `json:"refreshTokenFingerprint,omitempty"`
	// KiroApiKey is a headless Kiro API key (typically ksk_...). When set,
	// AuthMethod is "api_key" and the key is used directly as the Bearer token
	// without OAuth refresh. AccessToken is kept in sync for the shared request path.
	KiroApiKey   string `json:"kiroApiKey,omitempty"`
	ClientID     string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	AuthMethod   string `json:"authMethod"`             // Authentication method: "idc", "social", "external_idp", or "api_key"
	Provider     string `json:"provider,omitempty"`     // Identity provider name (e.g., "BuilderId", "GitHub", "AzureAD")
	Region       string `json:"region"`                 // AWS region (fallback for both auth and API region)
	AuthRegion   string `json:"authRegion,omitempty"`   // Region for token refresh endpoints; falls back to region
	ApiRegion    string `json:"apiRegion,omitempty"`    // Region for API request hosts; falls back to region
	StartUrl     string `json:"startUrl,omitempty"`     // AWS SSO start URL

	// External IdP authentication fields (for "Your organization" Kiro SSO flow).
	// IssuerURL and Scopes live in the Microsoft Enterprise SSO block below --
	// both flows are external OAuth2 and share those two fields.
	IdPClientID string `json:"idpClientId,omitempty"` // Client ID registered with the external IdP (from Kiro portal)
	LoginHint   string `json:"loginHint,omitempty"`   // User email used as login_hint during IdP authorization

	IdPTokenEndpoint string `json:"idpTokenEndpoint,omitempty"` // Cached IdP token endpoint (resolved via OIDC discovery during login)

	ExpiresAt  int64  `json:"expiresAt,omitempty"`  // Token expiration timestamp (Unix seconds); unused for API Key
	MachineId  string `json:"machineId,omitempty"`  // UUID machine identifier for request tracking
	ProfileArn string `json:"profileArn,omitempty"` // CodeWhisperer/Kiro profile ARN for generation requests

	// Microsoft Enterprise SSO uses an external OAuth2 public client. These
	// fields are deliberately separate from the AWS IdC client secret/region:
	// Account.Region remains the AWS authentication region, while data-plane
	// routing treats ProfileArn as authoritative whenever it is available.
	TokenEndpoint string `json:"tokenEndpoint,omitempty"` // External IdP OAuth2 token endpoint
	IssuerURL     string `json:"issuerUrl,omitempty"`     // External IdP OIDC issuer
	Scopes        string `json:"scopes,omitempty"`        // Space-separated external IdP scopes

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the only switch that decides whether to keep dispatching once UsageLimit is reached.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed
}

// IsApiKeyCredential returns true if this account is authenticated via API key.
// An account is an API-key credential if KiroApiKey is non-empty OR if authMethod
// is "api_key" or "apikey" (case-insensitive).
func (a *Account) IsApiKeyCredential() bool {
	if a.KiroApiKey != "" {
		return true
	}
	method := strings.ToLower(a.AuthMethod)
	return method == "api_key" || method == "apikey"
}

// EffectiveAuthRegion returns the effective auth region for this account,
// resolved using the fallback chain:
// account.authRegion > account.region > global authRegion > global region > "us-east-1"
func (a *Account) EffectiveAuthRegion() string {
	if a.AuthRegion != "" {
		return a.AuthRegion
	}
	if a.Region != "" {
		return a.Region
	}
	authRegion := GetGlobalAuthRegion()
	if authRegion != "us-east-1" {
		return authRegion
	}
	globalRegion := GetGlobalRegion()
	if globalRegion != "us-east-1" {
		return globalRegion
	}
	return "us-east-1"
}

// EffectiveApiRegion returns the effective API region for this account,
// resolved using the fallback chain:
// account.apiRegion > account.region > global apiRegion > global region > "us-east-1"
func (a *Account) EffectiveApiRegion() string {
	if a.ApiRegion != "" {
		return a.ApiRegion
	}
	if a.Region != "" {
		return a.Region
	}
	apiRegion := GetGlobalApiRegion()
	if apiRegion != "us-east-1" {
		return apiRegion
	}
	globalRegion := GetGlobalRegion()
	if globalRegion != "us-east-1" {
		return globalRegion
	}
	return "us-east-1"
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// UpstreamProvider describes an external OpenAI/Anthropic-compatible endpoint that
// selected models can be forwarded to (e.g. a 9router or xpiki instance). When a
// client model matches an enabled ModelRoute, the raw request is passed through to
// the route's provider instead of being dispatched to the Kiro account pool.
type UpstreamProvider struct {
	ID       string `json:"id"`                 // Unique identifier (UUID)
	Name     string `json:"name"`               // Human-readable label
	BaseURL  string `json:"baseUrl"`            // Base URL incl. version, e.g. https://api.xpiki.com/v1
	ApiKey   string `json:"apiKey"`             // Bearer token sent to the upstream
	ProxyURL string `json:"proxyURL,omitempty"` // Optional per-provider outbound proxy (falls back to global)
	Enabled  bool   `json:"enabled"`            // Whether this provider may receive forwards

	// Operator-supplied prices in USD per 1M tokens, used only to estimate the
	// cost shown in the stats dashboard. Zero means "unpriced": no cost is
	// attributed, and the UI shows "—" rather than a misleading $0.00. Nothing
	// here is billed or sent upstream.
	PriceInPerM  float64 `json:"priceInPerM,omitempty"`
	PriceOutPerM float64 `json:"priceOutPerM,omitempty"`
}

// CostUSD estimates the cost of a request from the provider's configured
// per-1M-token prices. It returns 0 when the provider is unpriced, so callers
// can distinguish "free" from "unknown" by checking the prices themselves.
func (p UpstreamProvider) CostUSD(inputTokens, outputTokens int64) float64 {
	if p.PriceInPerM <= 0 && p.PriceOutPerM <= 0 {
		return 0
	}
	return (float64(inputTokens)*p.PriceInPerM + float64(outputTokens)*p.PriceOutPerM) / 1e6
}

// RouteTarget is one upstream candidate for a ModelRoute. A route may list
// several, which is what makes "switch provider" a reordering rather than a
// destructive edit, and what allows failover when a provider is down.
//
// Selection (see ResolveRoute): eligible targets are ordered by Priority
// ascending; targets sharing a Priority form one tier and are picked from by
// Weight. Tiers are tried in order, so a route degrades from its preferred
// provider to its backups instead of failing outright.
//
// This mirrors established gateway designs: Priority is LiteLLM's deployment
// `order` (each tier exhausted before the next), Weight is Envoy's
// weighted_clusters (proportional split among equals).
type RouteTarget struct {
	UpstreamID  string `json:"upstreamId"`            // Target UpstreamProvider.ID
	TargetModel string `json:"targetModel,omitempty"` // Optional model name to rewrite to; empty = keep original
	Priority    int    `json:"priority"`              // Lower is preferred; 0 is the top tier
	Weight      int    `json:"weight,omitempty"`      // Share within its tier; <=0 is treated as 1
	Enabled     bool   `json:"enabled"`               // Whether this target may be selected
}

// ModelRoute maps a client-supplied model name to one or more upstream targets.
// Matching is exact on Model; only the choice of destination is ranked.
//
// Loop-safety note: the set of routed model names MUST be disjoint from the model
// names the upstream forwards back to this proxy. A back-referenced Kiro model with
// no matching enabled route falls through to the default Kiro pool, breaking the loop.
// Multi-target changes nothing here — a route with zero eligible targets resolves to
// nothing and falls through exactly as an unrouted model does.
type ModelRoute struct {
	ID      string        `json:"id"`                // Unique identifier (UUID)
	Model   string        `json:"model"`             // Client model name to match (exact)
	Targets []RouteTarget `json:"targets,omitempty"` // Ranked upstream candidates
	Enabled bool          `json:"enabled"`           // Whether this route is active

	// UpstreamID and TargetModel are the pre-multi-target 1:1 schema.
	//
	// Deprecated: migrated into Targets on load (see migrateModelRoutes). Kept so
	// older config files and exported bundles still read, and so a downgrade to a
	// build without Targets keeps working. Do not read these at request time —
	// use Targets.
	UpstreamID  string `json:"upstreamId,omitempty"`
	TargetModel string `json:"targetModel,omitempty"`
}

// migrateModelRoutes promotes the legacy 1:1 route schema into Targets, and
// reports whether anything changed so the caller can decide to persist.
//
// It is idempotent (a route that already has Targets is left alone) and
// non-destructive (the legacy fields are preserved rather than cleared, unlike
// the AllowOverage migration, because exported bundles are read by other hosts
// that may still be on the old build).
//
// Called from both Load and MergeUpstreamBundle: a v1 bundle imported into this
// build must get Targets populated too, or its routes would silently never match.
func migrateModelRoutes(routes []ModelRoute) bool {
	changed := false
	for i := range routes {
		r := &routes[i]
		if len(r.Targets) > 0 || strings.TrimSpace(r.UpstreamID) == "" {
			continue
		}
		r.Targets = []RouteTarget{{
			UpstreamID:  r.UpstreamID,
			TargetModel: r.TargetModel,
			Priority:    0,
			Weight:      1,
			// The legacy schema had no per-target enable — the route's own
			// Enabled flag was the only switch. Enable the target so the
			// route's effective behavior is unchanged by this migration.
			Enabled: true,
		}}
		changed = true
	}
	return changed
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID         string `json:"id"`                 // Unique identifier (UUID)
	Name       string `json:"name,omitempty"`     // Human-readable label
	Key        string `json:"key"`                // The actual key value clients send
	Enabled    bool   `json:"enabled"`            // Whether this key may authenticate
	Migrated   bool   `json:"migrated,omitempty"` // True if migrated from legacy single ApiKey field
	CreatedAt  int64  `json:"createdAt"`          // Creation timestamp (Unix seconds)
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	// Cumulative usage (never auto-reset)
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8080)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	KiroVersion   string        `json:"kiroVersion,omitempty"`
	SystemVersion string        `json:"systemVersion,omitempty"`
	NodeVersion   string        `json:"nodeVersion,omitempty"`
	Accounts      []Account     `json:"accounts"` // Registered Kiro accounts

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"
	// DefaultThinkingEffort is the reasoning depth applied when a thinking request
	// names no level: "low", "medium", "high", "xhigh", "max", or "" / "auto" to
	// let the model choose. Empty keeps the historical behavior.
	DefaultThinkingEffort string `json:"defaultThinkingEffort,omitempty"`
	// AdvertiseEffortModels adds one "(level)" variant per level to /v1/models.
	// Off by default: it multiplies the list size, and clients that own a level
	// picker (9router) do not need the variants advertised to use them.
	AdvertiseEffortModels bool `json:"advertiseEffortModels,omitempty"`

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// Global default regions. Used as a fallback when an account does not
	// specify its own region. All default to "us-east-1" when empty.
	Region     string `json:"region,omitempty"`     // Default region for both auth and API
	AuthRegion string `json:"authRegion,omitempty"` // Default region for token refresh endpoints
	ApiRegion  string `json:"apiRegion,omitempty"`  // Default region for API request hosts

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// MaxPayloadBytes is the upper bound for the serialized Kiro request body.
	// Requests above this are truncated (oldest history dropped) before dispatch.
	// 0 means "use DefaultMaxPayloadBytes". Read per-request, so changes apply
	// at runtime without a restart. Do not set >~2.15MB: AWS rejects oversized
	// bodies with 400 CONTENT_LENGTH_EXCEEDS_THRESHOLD and there is no shrink-retry.
	MaxPayloadBytes int `json:"maxPayloadBytes,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// Security hardening (all opt-in; zero values preserve legacy behavior).
	AdminAllowlist []string `json:"adminAllowlist,omitempty"` // IPs/CIDRs allowed to reach /admin*. Empty = allow all.
	TrustProxy     bool     `json:"trustProxy,omitempty"`     // Parse X-Forwarded-For / X-Real-IP for client IP when true.
	TLSCertFile    string   `json:"tlsCertFile,omitempty"`    // PEM cert path. Both cert+key required to enable HTTPS.
	TLSKeyFile     string   `json:"tlsKeyFile,omitempty"`     // PEM key path. Empty = plain HTTP.

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// Upstreams and ModelRoutes configure forwarding of selected models to external
	// OpenAI/Anthropic-compatible endpoints instead of the Kiro account pool.
	Upstreams   []UpstreamProvider `json:"upstreams,omitempty"`
	ModelRoutes []ModelRoute       `json:"modelRoutes,omitempty"`

	// WebSearch configures server-side execution of the web_search tool. The Kiro
	// backend emits a tool_use(web_search) and then waits for a tool_result it
	// never receives on its own; when enabled, the proxy runs the search itself
	// (free-first: SearXNG primary, Tavily optional fallback) and feeds the
	// result back so the model can answer.
	WebSearch WebSearchConfig `json:"webSearch,omitempty"`

	// Memory configures the optional long-term memory sidecar (Mem0 self-hosted).
	// Off by default. When enabled, the proxy can store and retrieve memories via
	// a dedicated admin API; it does NOT inject memory into the LLM request path.
	Memory MemoryConfig `json:"memory,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed
}

// WebSearchConfig controls proxy-side execution of the web_search tool. It is
// free-first: SearXNG (self-hosted, no API cost) is the primary provider and
// Tavily is an optional, budget-capped fallback that is off unless explicitly
// enabled. The structure is nested by concern; GetWebSearchConfig resolves
// zero-valued fields to their defaults centrally.
type WebSearchConfig struct {
	// Enabled turns on the server-side search sub-loop. When false, a
	// tool_use(web_search) is passed through to the client unchanged (legacy
	// behavior), which stalls unless the client executes the search itself.
	Enabled bool `json:"enabled,omitempty"`

	// Routing selects providers and the free-first policy.
	Routing WebSearchRouting `json:"routing,omitempty"`

	// Limits bounds rounds, searches, concurrency, results, and total time.
	Limits WebSearchLimits `json:"limits,omitempty"`

	// SearXNG configures the primary (free) discovery provider.
	SearXNG SearXNGConfig `json:"searxng,omitempty"`

	// Tavily configures the optional paid-tier fallback (free-only by default).
	Tavily TavilyConfig `json:"tavily,omitempty"`

	// Cache configures the process-level result cache.
	Cache WebSearchCacheConfig `json:"cache,omitempty"`

	// Reranking configures deterministic result reranking.
	Reranking WebSearchRerankConfig `json:"reranking,omitempty"`

	// AppendSources controls whether a deterministic "Sources:" list is appended
	// to the final answer. Defaults to true when unset (see GetWebSearchConfig).
	AppendSources *bool `json:"appendSources,omitempty"`

	// EmitNativeToolBlocks controls whether the proxy synthesizes Anthropic-native
	// server_tool_use / web_search_tool_result content blocks (and the
	// usage.server_tool_use.web_search_requests counter) so clients like Claude
	// Code display the searches they ran instead of "Did 0 searches". Defaults to
	// true when unset. A kill-switch: set false if a client rejects the synthetic
	// shape.
	EmitNativeToolBlocks *bool `json:"emitNativeToolBlocks,omitempty"`
}

// WebSearchRouting selects providers and enforces the free-first / no-paid-usage
// policy.
type WebSearchRouting struct {
	// Mode is the routing strategy. Only "free-first" is implemented: try the
	// primary (free) provider, fall back only on failure/low quality. Empty means
	// DefaultWebSearchRoutingMode.
	Mode string `json:"mode,omitempty"`

	// PrimaryProvider names the first provider to try. Empty means
	// DefaultWebSearchPrimaryProvider ("searxng").
	PrimaryProvider string `json:"primaryProvider,omitempty"`

	// FallbackProviders are tried in order when the primary yields no usable
	// result. nil means the default (["tavily"]).
	FallbackProviders []string `json:"fallbackProviders,omitempty"`

	// AllowPaidUsage is a hard gate. When false (default), a provider that would
	// incur cost (Tavily beyond its free budget, or with a non-basic depth) is
	// never used. This is enforced independently of per-provider enabled flags.
	AllowPaidUsage bool `json:"allowPaidUsage,omitempty"`
}

// WebSearchLimits bounds the search loop. maxRounds and maxSearchesPerRequest are
// distinct budgets: one round may issue several searches.
type WebSearchLimits struct {
	// MaxRounds bounds Kiro reasoning round-trips. 0 means DefaultWebSearchMaxRounds.
	MaxRounds int `json:"maxRounds,omitempty"`

	// MaxSearchesPerRequest bounds total provider calls for the whole logical
	// request across all rounds. 0 means DefaultWebSearchMaxSearches.
	MaxSearchesPerRequest int `json:"maxSearchesPerRequest,omitempty"`

	// MaxConcurrentSearches bounds parallel provider calls within one round.
	// 0 means DefaultWebSearchMaxConcurrency.
	MaxConcurrentSearches int `json:"maxConcurrentSearches,omitempty"`

	// MaxResultsPerSearch caps results fed back per query. 0 means
	// DefaultWebSearchMaxResults.
	MaxResultsPerSearch int `json:"maxResultsPerSearch,omitempty"`

	// TotalTimeoutSeconds bounds the entire web-search logical request. 0 means
	// DefaultWebSearchTotalTimeoutSeconds.
	TotalTimeoutSeconds int `json:"totalTimeoutSeconds,omitempty"`
}

// SearXNGConfig configures the self-hosted SearXNG JSON search API (primary).
type SearXNGConfig struct {
	// Enabled turns SearXNG on as a provider. Defaults to true (free-first).
	Enabled *bool `json:"enabled,omitempty"`

	// BaseURL is the SearXNG instance root (e.g. http://searxng:8080). Validated
	// at startup; NEVER taken from a request (SSRF guard). Empty means
	// DefaultSearXNGBaseURL.
	BaseURL string `json:"baseUrl,omitempty"`

	// TimeoutSeconds bounds a single SearXNG call. 0 means DefaultSearXNGTimeout.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// Language biases results ("auto" or an ISO code). Empty means "auto".
	Language string `json:"language,omitempty"`

	// SafeSearch maps to SearXNG safesearch (0=none, 1=moderate, 2=strict). Use a
	// pointer so 0 is distinguishable from unset; nil means DefaultSearXNGSafeSearch.
	SafeSearch *int `json:"safeSearch,omitempty"`

	// Categories restricts SearXNG categories. nil means ["general"].
	Categories []string `json:"categories,omitempty"`

	// MinimumResults is the quality floor: fewer usable results than this triggers
	// fallback. 0 means DefaultSearXNGMinResults.
	MinimumResults int `json:"minimumResults,omitempty"`
}

// TavilyConfig configures the optional Tavily fallback. Off by default; strictly
// free-only unless routing.allowPaidUsage is set.
type TavilyConfig struct {
	// Enabled turns Tavily on as a fallback. Defaults to false.
	Enabled bool `json:"enabled,omitempty"`

	// APIKey authenticates against Tavily. TAVILY_API_KEY env overrides it.
	APIKey string `json:"apiKey,omitempty"`

	// FreeOnly forbids paid features (advanced depth, auto_parameters) and stops
	// once the monthly credit budget is spent. Defaults to true.
	FreeOnly *bool `json:"freeOnly,omitempty"`

	// MonthlyCreditLimit caps Tavily credits spent per calendar month. 0 means
	// DefaultTavilyMonthlyCredits.
	MonthlyCreditLimit int `json:"monthlyCreditLimit,omitempty"`

	// SearchDepth maps to Tavily search_depth. Under FreeOnly it is forced to
	// "basic". Empty means "basic".
	SearchDepth string `json:"searchDepth,omitempty"`

	// AutoParameters enables Tavily auto_parameters. Forbidden under FreeOnly.
	AutoParameters bool `json:"autoParameters,omitempty"`

	// TimeoutSeconds bounds a single Tavily call. 0 means DefaultTavilyTimeout.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// RetryMax bounds retries for transient failures (429/5xx/timeout). 0 means
	// DefaultWebSearchRetryMax. Terminal errors (400/401/403) never retry.
	RetryMax int `json:"retryMax,omitempty"`

	// RetryBaseDelayMs is the base backoff between retries. 0 means
	// DefaultWebSearchRetryBaseDelayMs.
	RetryBaseDelayMs int `json:"retryBaseDelayMs,omitempty"`
}

// WebSearchCacheConfig configures the bounded process-level result cache.
type WebSearchCacheConfig struct {
	// Enabled turns the process cache on. Defaults to true.
	Enabled *bool `json:"enabled,omitempty"`

	// Provider is the cache backend. Only "memory" is implemented. Empty means
	// "memory".
	Provider string `json:"provider,omitempty"`

	// TTLSeconds bounds a cache entry's lifetime. 0 means DefaultWebSearchCacheTTL.
	TTLSeconds int `json:"ttlSeconds,omitempty"`

	// MaxEntries bounds the LRU. 0 means DefaultWebSearchCacheEntries.
	MaxEntries int `json:"maxEntries,omitempty"`
}

// WebSearchRerankConfig configures deterministic reranking of merged results.
type WebSearchRerankConfig struct {
	// Provider is the rerank strategy. Only "heuristic" is implemented (no paid
	// LLM). Empty means "heuristic".
	Provider string `json:"provider,omitempty"`

	// MaxCandidates bounds results considered before reranking. 0 means
	// DefaultWebSearchRerankCandidates.
	MaxCandidates int `json:"maxCandidates,omitempty"`

	// MaxFinalResults bounds results kept after reranking. 0 means
	// DefaultWebSearchMaxResults.
	MaxFinalResults int `json:"maxFinalResults,omitempty"`
}

// MemoryConfig controls the optional long-term memory sidecar (Mem0 self-hosted).
// It is off by default (Enabled zero value = false). When enabled, the proxy can
// store/retrieve memories through a dedicated admin API, and optionally inject
// retrieved memories into the LLM request (Inject) and auto-capture each turn's
// Q&A (WriteMode != "explicit"). Injection mutates req.Messages BEFORE translation,
// never the translated Kiro payload, so the translator's HTTP-400-avoidance logic
// is untouched. GetMemoryConfig resolves zero-valued fields to their defaults
// centrally.
type MemoryConfig struct {
	// Enabled turns on the memory sidecar. When false, the factory returns a
	// no-op provider and no calls are made to the backend.
	Enabled bool `json:"enabled,omitempty"`

	// Provider selects the memory backend. Only "mem0" is implemented. Empty
	// means DefaultMemoryProvider.
	Provider string `json:"provider,omitempty"`

	// BaseURL is the self-hosted Mem0 server root, e.g. "http://localhost:8888".
	// Required when Enabled; MemoryEnabled() is false until it is set.
	BaseURL string `json:"baseURL,omitempty"`

	// APIKey is the X-Api-Key sent to Mem0. Masked when returned to the admin UI.
	APIKey string `json:"apiKey,omitempty"`

	// WriteMode governs how memories are captured: "explicit" (only via the
	// store API — no auto-capture), "curated", or "automatic". Empty means
	// DefaultMemoryWriteMode ("explicit"). Any mode other than "explicit" enables
	// auto-capture of each successful turn's Q&A (see MemoryCaptureEnabled).
	WriteMode string `json:"writeMode,omitempty"`

	// RetrievalLimit bounds how many memories a search returns. 0 means
	// DefaultMemoryRetrievalLimit.
	RetrievalLimit int `json:"retrievalLimit,omitempty"`

	// Inject turns on reading memories and injecting them into the LLM request
	// (into the last user message, before translation). Independent of WriteMode:
	// a deployment can inject without capturing, or capture without injecting.
	// Defaults to false when unset — memory never touches the request path until
	// the operator opts in.
	Inject *bool `json:"inject,omitempty"`

	// MaxInjectTokens bounds the size of the injected memory context block. 0 means
	// DefaultMemoryMaxInjectTokens. Keeps the injected block small so it cannot push
	// the request toward the upstream byte cap.
	MaxInjectTokens int `json:"maxInjectTokens,omitempty"`

	// Redaction bounds what may be persisted. Applied even in automatic mode.
	Redaction MemoryRedaction `json:"redaction,omitempty"`

	// Timeouts bounds per-call latency to the backend.
	Timeouts MemoryTimeouts `json:"timeouts,omitempty"`

	// FailOpen controls whether backend errors degrade silently (recall returns
	// empty, writes are dropped) instead of surfacing to the caller. Defaults to
	// true when unset, so a memory outage never breaks a request.
	FailOpen *bool `json:"failOpen,omitempty"`
}

// MemoryRedaction bounds what content may be persisted. These guards apply
// regardless of WriteMode — automatic mode does NOT bypass them.
type MemoryRedaction struct {
	// RedactSecrets masks obvious credentials (API keys, bearer tokens) before a
	// memory is written. Defaults to true when unset.
	RedactSecrets *bool `json:"redactSecrets,omitempty"`

	// StoreSourceCode allows source-code-looking candidates (large fenced code
	// blocks, diffs, .env dumps) to be persisted. Defaults to false: coding
	// sessions routinely contain private source and secrets.
	StoreSourceCode bool `json:"storeSourceCode,omitempty"`
}

// MemoryTimeouts bounds per-call latency to the memory backend. Mem0 "add"
// triggers an LLM extraction call upstream, so the write timeout is larger.
type MemoryTimeouts struct {
	// SearchMs bounds a retrieval call. 0 means DefaultMemorySearchTimeoutMs.
	SearchMs int `json:"searchMs,omitempty"`

	// WriteMs bounds an add call. 0 means DefaultMemoryWriteTimeoutMs.
	WriteMs int `json:"writeMs,omitempty"`
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version
const Version = "1.2.6"

var (
	cfg     *Config
	cfgLock sync.RWMutex
	cfgPath string
)

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	// Guard the cfgPath write: every other mutation of the package globals runs
	// under cfgLock, and Save() (called while a caller holds the lock) reads
	// cfgPath. Load() takes the write lock itself, so release before calling it.
	cfgLock.Lock()
	cfgPath = path
	cfgLock.Unlock()
	return Load()
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default configuration.
			// Binds to loopback by default: a fresh install ships with the default
			// "changeme" password and auth off, so a public bind would expose an
			// unauthenticated admin surface. Operators who need a public/container
			// bind set Host explicitly (and are then subject to the startup safety
			// gate — see config/startup_safety.go).
			cfg = &Config{
				Password:      "changeme",
				Port:          8080,
				Host:          "127.0.0.1",
				RequireApiKey: false,
				Accounts:      []Account{},
			}
			return saveLocked()
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		// Primary is present but corrupt. Fall back to the last-known-good backup
		// written by atomicWriteFile before the most recent rename, rather than
		// refusing to start (which would strand every account/token).
		if bakData, bakErr := os.ReadFile(cfgPath + ".bak"); bakErr == nil {
			var bc Config
			if json.Unmarshal(bakData, &bc) == nil {
				cfg = &bc
				// Re-establish a good primary from the backup.
				if werr := atomicWriteFile(cfgPath, bakData, 0600); werr != nil {
					return fmt.Errorf("primary config corrupt (%v); restored from backup but failed to rewrite primary: %w", err, werr)
				}
				goto migrations
			}
		}
		return fmt.Errorf("config file is corrupt and no valid backup exists: %w", err)
	}
	cfg = &c

migrations:

	// Migration: if a legacy single ApiKey is present and the new ApiKeys list is empty,
	// promote it into the new structure. The migrated entry inherits the legacy
	// RequireApiKey state — if the legacy deployment was public (RequireApiKey=false),
	// we mark the entry disabled so it doesn't accidentally start enforcing auth.
	// Operators can flip it on later from the admin UI. The legacy field is kept
	// for backward compatibility when reading older config files.
	if cfg.ApiKey != "" && len(cfg.ApiKeys) == 0 {
		cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
			ID:        newUUID(),
			Name:      "legacy",
			Key:       cfg.ApiKey,
			Enabled:   cfg.RequireApiKey,
			Migrated:  true,
			CreatedAt: time.Now().Unix(),
		})
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: per-account AllowOverage → OverageStatus.
	// Pre-Overages-switch deployments stored `allowOverage: true` to mean "keep
	// dispatching when quota is exhausted". The new model reads OverageStatus
	// from the upstream AWS Q switch instead. To avoid silently disabling
	// previously-allowed accounts on first launch, treat allowOverage=true as
	// OverageStatus="ENABLED" (operators can refresh from AWS later). The
	// legacy field is then cleared so future saves don't re-emit it.
	overageMigrated := false
	for i := range cfg.Accounts {
		if cfg.Accounts[i].LegacyAllowOverage {
			if cfg.Accounts[i].OverageStatus == "" {
				cfg.Accounts[i].OverageStatus = "ENABLED"
			}
			cfg.Accounts[i].LegacyAllowOverage = false
			overageMigrated = true
		}
	}
	if overageMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: legacy 1:1 model routes → ranked Targets. Idempotent; only
	// persists when a route was actually rewritten.
	if migrateModelRoutes(cfg.ModelRoutes) {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// saveLocked persists cfg to disk. Caller MUST already hold cfgLock.
// This is identical to Save() (which does not take the lock either) but is named
// distinctly so call sites that already hold cfgLock are explicit about it.
func saveLocked() error {
	return Save()
}

// newUUID returns a UUID v4 string. Defined here to avoid pulling extra deps in this file.
func newUUID() string {
	return GenerateMachineId()
}

// Save persists the current configuration to the JSON file atomically and
// durably. A plain os.WriteFile truncates the target before writing, so a crash
// (or full disk) mid-write can leave a truncated/corrupt config and lose every
// account and token. Instead we:
//  1. marshal and validate (round-trip) the JSON,
//  2. write it to a temp file in the same directory,
//  3. fsync + close the temp file,
//  4. copy the current good config to a .bak (last-known-good),
//  5. atomically rename the temp file over the target,
//  6. best-effort fsync the parent directory so the rename is durable.
//
// Callers already serialize on cfgLock, so there is no concurrent writer.
func Save() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Validate the bytes round-trip before we let them near the primary file, so
	// a marshaling bug can never overwrite a good config with garbage.
	var probe Config
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("refusing to save unparsable config: %w", err)
	}
	return atomicWriteFile(cfgPath, data, 0600)
}

// atomicWriteFile writes data to path via a temp file + fsync + rename, keeping a
// .bak of the previous contents. It never truncates the primary in place.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}

	// Keep a last-known-good backup of the existing primary (ignore if absent).
	if existing, rerr := os.ReadFile(path); rerr == nil {
		_ = os.WriteFile(path+".bak", existing, perm)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // renamed successfully; disable cleanup

	// Durably record the directory entry for the rename (best-effort: not all
	// filesystems support directory fsync, and failure here is non-fatal).
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "."
	}
	dir := cfgPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

// GetGlobalRegion returns the global default region. Defaults to "us-east-1".
func GetGlobalRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.Region == "" {
		return "us-east-1"
	}
	return cfg.Region
}

// GetGlobalAuthRegion returns the global default auth region. Defaults to "us-east-1".
func GetGlobalAuthRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AuthRegion == "" {
		return "us-east-1"
	}
	return cfg.AuthRegion
}

// GetGlobalApiRegion returns the global default API region. Defaults to "us-east-1".
func GetGlobalApiRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ApiRegion == "" {
		return "us-east-1"
	}
	return cfg.ApiRegion
}

func GetAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

// GetAccountByID returns a copy of the account with the given ID, or nil if no
// such account exists. Unlike the pool, this sees ALL accounts including disabled
// and banned ones — admin/setup operations (e.g. profile discovery) need to act on
// accounts that are not currently routable.
func GetAccountByID(id string) *Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	// cfg is nil before Init runs. Callers on the request path (e.g. metrics
	// labeling) must get a nil result rather than a panic.
	if cfg == nil {
		return nil
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			a := cfg.Accounts[i]
			return &a
		}
	}
	return nil
}

func GetEnabledAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

// RefreshTokenFingerprint returns a stable, non-reversible identifier for an
// opaque refresh token. Empty tokens do not receive a fingerprint.
func RefreshTokenFingerprint(refreshToken string) string {
	if refreshToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(refreshToken))
	return fmt.Sprintf("%x", sum[:])
}

// APIKeyFingerprint returns a stable, non-reversible identifier for a Kiro API key.
func APIKeyFingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return fmt.Sprintf("%x", sum[:])
}

// IsAPIKeyAccount reports whether the account authenticates with a Kiro API key.
func IsAPIKeyAccount(account *Account) bool {
	if account == nil {
		return false
	}
	if strings.TrimSpace(account.KiroApiKey) != "" {
		return true
	}
	method := strings.ToLower(strings.TrimSpace(account.AuthMethod))
	return method == "api_key" || method == "apikey"
}

// SplitKiroAPIKeyAndRegion parses the convenience form "key|region".
// The key itself is not restricted to a fixed prefix so future formats remain compatible.
func SplitKiroAPIKeyAndRegion(raw string) (key, region string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", ErrEmptyAPIKey
	}
	parts := strings.Split(trimmed, "|")
	if len(parts) > 2 {
		return "", "", errors.New("multiple pipe separators are not allowed")
	}
	key = strings.TrimSpace(parts[0])
	if key == "" {
		return "", "", errors.New("key before pipe is empty")
	}
	if len(parts) == 2 {
		region = strings.TrimSpace(parts[1])
		if region == "" {
			return "", "", errors.New("region after pipe is empty")
		}
		if err := validateKiroRegionHostLabel(region); err != nil {
			return "", "", err
		}
	}
	return key, region, nil
}

func validateKiroRegionHostLabel(region string) error {
	region = strings.TrimSpace(region)
	if region == "" {
		return errors.New("region is empty")
	}
	for _, r := range region {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return errors.New("region contains host-unsafe characters")
	}
	if strings.Contains(region, " ") || strings.ContainsAny(region, "\n\r\t./") {
		return errors.New("region contains host-unsafe characters")
	}
	return nil
}

// MachineIdFromAPIKey derives the machine id used by Kiro CLI/API-key clients:
// sha256 hex of "KiroAPIKey/<api_key>".
func MachineIdFromAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte("KiroAPIKey/" + apiKey))
	return fmt.Sprintf("%x", sum[:])
}

// NormalizeAPIKeyAccount fills API-key credential defaults in place.
// It accepts "ksk_xxx|region", sets AuthMethod=api_key, copies the key into
// AccessToken for the shared Bearer path, clears OAuth-only fields, and
// derives MachineId when missing.
func NormalizeAPIKeyAccount(account *Account) error {
	if account == nil {
		return errors.New("account is nil")
	}
	raw := strings.TrimSpace(account.KiroApiKey)
	if raw == "" {
		raw = strings.TrimSpace(account.AccessToken)
	}
	key, region, err := SplitKiroAPIKeyAndRegion(raw)
	if err != nil {
		return err
	}
	account.KiroApiKey = key
	account.AccessToken = key
	account.AuthMethod = "api_key"
	account.RefreshToken = ""
	account.RefreshTokenFingerprint = ""
	account.ClientID = ""
	account.ClientSecret = ""
	account.TokenEndpoint = ""
	account.IssuerURL = ""
	account.Scopes = ""
	account.ProfileArn = ""
	account.ExpiresAt = 0
	if region != "" {
		if strings.TrimSpace(account.Region) == "" {
			account.Region = region
		}
	}
	if strings.TrimSpace(account.Region) == "" {
		account.Region = "us-east-1"
	}
	if err := validateKiroRegionHostLabel(account.Region); err != nil {
		return err
	}
	if strings.TrimSpace(account.MachineId) == "" {
		account.MachineId = MachineIdFromAPIKey(key)
	}
	if strings.TrimSpace(account.Provider) == "" {
		account.Provider = "APIKey"
	}
	if strings.TrimSpace(account.Email) == "" {
		// Stable display label without leaking the full secret.
		fp := APIKeyFingerprint(key)
		if len(fp) > 12 {
			fp = fp[:12]
		}
		account.Email = "api-key-" + fp
	}
	return nil
}

// AccountCredentialExists checks both the current refresh token and the
// original credential fingerprint while holding the configuration read lock.
func AccountCredentialExists(refreshToken string) bool {
	if refreshToken == "" {
		return false
	}
	fingerprint := RefreshTokenFingerprint(refreshToken)
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, account := range cfg.Accounts {
		if account.RefreshToken == refreshToken ||
			(fingerprint != "" && account.RefreshTokenFingerprint == fingerprint) {
			return true
		}
	}
	return false
}

// AccountAPIKeyExists reports whether a Kiro API key is already persisted.
func AccountAPIKeyExists(apiKey string) bool {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false
	}
	fingerprint := APIKeyFingerprint(apiKey)
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, account := range cfg.Accounts {
		existing := strings.TrimSpace(account.KiroApiKey)
		if existing == "" {
			continue
		}
		if existing == apiKey || APIKeyFingerprint(existing) == fingerprint {
			return true
		}
	}
	return false
}

// AccountIDExists reports whether an account ID is already persisted.
func AccountIDExists(id string) bool {
	if id == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, account := range cfg.Accounts {
		if account.ID == id {
			return true
		}
	}
	return false
}

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if IsAPIKeyAccount(&account) {
		if err := NormalizeAPIKeyAccount(&account); err != nil {
			return err
		}
	} else if account.RefreshTokenFingerprint == "" {
		account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
	}
	for _, existing := range cfg.Accounts {
		if account.ID != "" && existing.ID == account.ID {
			return ErrDuplicateAccountID
		}
		if account.RefreshToken != "" && existing.RefreshToken == account.RefreshToken {
			return ErrDuplicateRefreshToken
		}
		existingFingerprint := existing.RefreshTokenFingerprint
		if existingFingerprint == "" {
			existingFingerprint = RefreshTokenFingerprint(existing.RefreshToken)
		}
		if account.RefreshTokenFingerprint != "" &&
			existingFingerprint == account.RefreshTokenFingerprint {
			return ErrDuplicateRefreshToken
		}
		if account.KiroApiKey != "" {
			existingKey := strings.TrimSpace(existing.KiroApiKey)
			if existingKey != "" && (existingKey == account.KiroApiKey ||
				APIKeyFingerprint(existingKey) == APIKeyFingerprint(account.KiroApiKey)) {
				return ErrDuplicateAPIKey
			}
		}
	}
	cfg.Accounts = append(cfg.Accounts, account)
	if err := Save(); err != nil {
		cfg.Accounts = cfg.Accounts[:len(cfg.Accounts)-1]
		return err
	}
	return nil
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			// Current callers use UpdateAccount for administrative/status fields.
			// Preserve the authoritative credential state so a stale account
			// snapshot cannot overwrite a refresh-token rotation that completed
			// while an upstream status request was in flight.
			account.AccessToken = a.AccessToken
			account.RefreshToken = a.RefreshToken
			account.RefreshTokenFingerprint = a.RefreshTokenFingerprint
			account.KiroApiKey = a.KiroApiKey
			account.ClientID = a.ClientID
			account.ClientSecret = a.ClientSecret
			account.AuthMethod = a.AuthMethod
			account.Provider = a.Provider
			account.Region = a.Region
			account.StartUrl = a.StartUrl
			account.ExpiresAt = a.ExpiresAt
			account.ProfileArn = a.ProfileArn
			account.TokenEndpoint = a.TokenEndpoint
			account.IssuerURL = a.IssuerURL
			account.Scopes = a.Scopes
			if account.RefreshTokenFingerprint == "" {
				account.RefreshTokenFingerprint = RefreshTokenFingerprint(a.RefreshToken)
			}
			previous := cfg.Accounts[i]
			cfg.Accounts[i] = account
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// UpdateAccountOverageStatus persists the cached upstream overage status fields.
// Called after a successful setUserPreference or getUsageLimits round-trip.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

// SetAccountEnabled toggles the enabled state of an account and persists the change.
// Used to disable accounts whose refresh token has been revoked (401 Bad credentials)
// so subsequent requests skip them automatically.
func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previous := cfg.Accounts[i]
			cfg.Accounts[i].Enabled = enabled
			if !enabled {
				cfg.Accounts[i].BanStatus = "DISABLED"
				cfg.Accounts[i].BanTime = time.Now().Unix()
			}
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// SetAccountBanStatus marks an account as banned/disabled with a reason.
// Reason is recorded so operators can see why the account was auto-disabled.
func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previous := cfg.Accounts[i]
			cfg.Accounts[i].BanStatus = status
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = time.Now().Unix()
			if status == "BANNED" || status == "DISABLED" {
				cfg.Accounts[i].Enabled = false
			}
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// ClearAccountBanStatus marks an account active without replacing any
// credential fields from a potentially stale caller snapshot.
func ClearAccountBanStatus(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, account := range cfg.Accounts {
		if account.ID == id {
			previous := cfg.Accounts[i]
			cfg.Accounts[i].BanStatus = "ACTIVE"
			cfg.Accounts[i].BanReason = ""
			cfg.Accounts[i].BanTime = 0
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previous := cfg.Accounts[i].ProfileArn
			cfg.Accounts[i].ProfileArn = profileArn
			if err := Save(); err != nil {
				cfg.Accounts[i].ProfileArn = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// UpdateAccountProfileArnWithRegion persists a resolved profile ARN and, when
// apiRegion is non-empty, pins the account's data-plane region to it. Kiro /
// Q Developer profiles are regional (e.g. KiroProfile-eu-central-1), and the
// region that owns a profile can differ from the SSO/auth region. Recording the
// API region alongside the ARN ensures subsequent data-plane calls target the
// correct regional endpoint without re-probing. The auth region (account.Region)
// is intentionally left untouched so OIDC token refresh keeps working.
func UpdateAccountProfileArnWithRegion(id, profileArn, apiRegion string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			if apiRegion != "" {
				cfg.Accounts[i].ApiRegion = apiRegion
			}
			return Save()
		}
	}
	return nil
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return Save()
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	return UpdateAccountCredentialState(id, accessToken, refreshToken, expiresAt, "")
}

// UpdateAccountCredentialState atomically updates all fields produced by one
// refresh-token exchange. If persistence fails, the in-memory configuration is
// restored so a rotated token is never published from a state that cannot
// survive restart.
func UpdateAccountCredentialState(
	id string,
	accessToken string,
	refreshToken string,
	expiresAt int64,
	profileArn string,
) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previous := cfg.Accounts[i]
			if cfg.Accounts[i].RefreshTokenFingerprint == "" {
				cfg.Accounts[i].RefreshTokenFingerprint = RefreshTokenFingerprint(a.RefreshToken)
			}
			cfg.Accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				cfg.Accounts[i].RefreshToken = refreshToken
			}
			cfg.Accounts[i].ExpiresAt = expiresAt
			if profileArn != "" {
				cfg.Accounts[i].ProfileArn = profileArn
			}
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return ErrAccountNotFound
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

// GetAdminAllowlist returns a defensive copy of the admin IP allowlist.
func GetAdminAllowlist() []string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if len(cfg.AdminAllowlist) == 0 {
		return nil
	}
	out := make([]string, len(cfg.AdminAllowlist))
	copy(out, cfg.AdminAllowlist)
	return out
}

// GetTrustProxy reports whether forwarded-IP headers should be trusted.
func GetTrustProxy() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TrustProxy
}

// GetTLSFiles returns the configured TLS cert and key file paths.
func GetTLSFiles() (cert, key string) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TLSCertFile, cfg.TLSKeyFile
}

// IsTLSEnabled reports whether both TLS cert and key paths are configured.
func IsTLSEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TLSCertFile != "" && cfg.TLSKeyFile != ""
}

// UpdateSecuritySettings applies a partial patch of the security-related fields.
// Only non-nil arguments are written. One lock/Save cycle.
func UpdateSecuritySettings(allowlist *[]string, trustProxy *bool, tlsCert *string, tlsKey *string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if allowlist != nil {
		cfg.AdminAllowlist = *allowlist
	}
	if trustProxy != nil {
		cfg.TrustProxy = *trustProxy
	}
	if tlsCert != nil {
		cfg.TLSCertFile = *tlsCert
	}
	if tlsKey != nil {
		cfg.TLSKeyFile = *tlsKey
	}
	return Save()
}

// ConfigWarning describes a weak-configuration condition worth surfacing to the
// operator, both at startup (logs) and in the admin UI (banner). The Code is a
// stable identifier the UI re-localizes; Msg is an English fallback.
type ConfigWarning struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

// EvaluateSecurityWarnings inspects the live config for weak settings that make
// public exposure dangerous. Returned most-severe first.
func EvaluateSecurityWarnings() []ConfigWarning {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	defaultPassword := cfg.Password == "changeme"
	// Auth is effectively off whenever RequireApiKey is false: authenticate()
	// short-circuits to success before any key is consulted. Key count is
	// irrelevant here.
	authDisabled := !cfg.RequireApiKey
	// Resolve host the same way the listener does: GetHost() maps an empty Host
	// to "127.0.0.1", so an empty value is NOT a public bind. Only an explicit
	// wildcard address exposes the server. (Resolve inline — do not call GetHost()
	// here, it would re-acquire the same RLock.)
	publicBind := cfg.Host == "0.0.0.0" || cfg.Host == "::"

	var out []ConfigWarning
	if defaultPassword {
		out = append(out, ConfigWarning{
			Code: "defaultPassword",
			Msg:  `Admin password is still the default "changeme". Change it now.`,
		})
	}
	if authDisabled {
		out = append(out, ConfigWarning{
			Code: "authDisabled",
			Msg:  "API-key authentication is disabled; anyone can use the proxy.",
		})
	}
	if publicBind && (defaultPassword || authDisabled) {
		out = append(out, ConfigWarning{
			Code: "publicBind",
			Msg:  "Server is bound to a public address (0.0.0.0) with weak settings above.",
		})
	}
	return out
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return Save()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			return Save()
		}
	}
	return nil
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			return Save()
		}
	}
	return nil
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
	// DefaultEffort is the fallback reasoning depth for thinking requests that
	// name no level. Empty means "let the model choose" (historical behavior).
	DefaultEffort string `json:"defaultEffort"`
	// AdvertiseEffortModels reports whether /v1/models should list "(level)"
	// variants alongside each base and thinking model.
	AdvertiseEffortModels bool `json:"advertiseEffortModels"`
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	// Before Load() runs, cfg is nil. This getter is on the request-translation
	// path now (it supplies the default reasoning level), not just admin HTTP
	// handlers, so it has to return usable defaults instead of panicking.
	if cfg == nil {
		return ThinkingConfig{
			Suffix:       "-thinking",
			OpenAIFormat: "reasoning_content",
			ClaudeFormat: "thinking",
		}
	}

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:                suffix,
		OpenAIFormat:          openaiFormat,
		ClaudeFormat:          claudeFormat,
		DefaultEffort:         cfg.DefaultThinkingEffort,
		AdvertiseEffortModels: cfg.AdvertiseEffortModels,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat, defaultEffort string, advertiseEffortModels bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	cfg.DefaultThinkingEffort = defaultEffort
	cfg.AdvertiseEffortModels = advertiseEffortModels
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// DefaultMaxPayloadBytes is the serialized-request byte cap used when the setting
// is unset (0). 2,000,000 sits safely below the ~2.15MB AWS upstream ceiling while
// leaving room for headers and serialization overhead.
const DefaultMaxPayloadBytes = 2_000_000

// GetMaxPayloadBytes returns the configured request byte cap, falling back to
// DefaultMaxPayloadBytes when unset or non-positive.
func GetMaxPayloadBytes() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.MaxPayloadBytes <= 0 {
		return DefaultMaxPayloadBytes
	}
	return cfg.MaxPayloadBytes
}

// UpdateMaxPayloadBytes sets the request byte cap and persists the change.
func UpdateMaxPayloadBytes(n int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.MaxPayloadBytes = n
	return Save()
}

// Web search sub-loop defaults, used when the corresponding config field is 0.
const (
	DefaultWebSearchMaxResults     = 5
	DefaultWebSearchMaxRounds      = 4
	DefaultWebSearchMaxSearches    = 5
	DefaultWebSearchMaxConcurrency = 2
	DefaultWebSearchTotalTimeout   = 90
	DefaultWebSearchRetryMax       = 2
	DefaultWebSearchRetryBaseMs    = 300

	DefaultWebSearchRoutingMode     = "free-first"
	DefaultWebSearchPrimaryProvider = "searxng"

	DefaultSearXNGBaseURL    = "http://searxng:8080"
	DefaultSearXNGTimeout    = 12
	DefaultSearXNGSafeSearch = 1
	DefaultSearXNGMinResults = 3

	DefaultTavilyMonthlyCredits = 1000
	DefaultTavilyTimeout        = 15

	DefaultWebSearchCacheTTL         = 900
	DefaultWebSearchCacheEntries     = 1000
	DefaultWebSearchRerankCandidates = 20
)

// Memory sidecar defaults, used when the corresponding config field is 0/empty.
const (
	DefaultMemoryProvider       = "mem0"
	DefaultMemoryWriteMode      = "explicit"
	DefaultMemoryRetrievalLimit = 8

	DefaultMemorySearchTimeoutMs = 2000
	DefaultMemoryWriteTimeoutMs  = 5000

	// DefaultMemoryMaxInjectTokens bounds the injected memory-context block so a
	// large recall cannot balloon the request (and trip the translator's payload
	// cap). Used when MaxInjectTokens is 0.
	DefaultMemoryMaxInjectTokens = 1200
)

// Valid memory write modes. "explicit" is the safe default; "automatic" is
// operator opt-in only (surfaces a security warning in the admin UI).
const (
	MemoryWriteModeExplicit  = "explicit"
	MemoryWriteModeCurated   = "curated"
	MemoryWriteModeAutomatic = "automatic"
)

// ProviderSearXNG / ProviderTavily are the canonical provider name tokens used
// in routing config and metrics labels. Exported so the search package can share
// them without redefining (search imports config, never the reverse).
const (
	ProviderSearXNG = "searxng"
	ProviderTavily  = "tavily"
)

// GetWebSearchConfig returns the web_search execution settings with zero-valued
// caps resolved to their defaults. Enabled and the API key are returned as-is.
// The returned value is a copy; callers cannot mutate shared config state.
func GetWebSearchConfig() WebSearchConfig {
	cfgLock.RLock()
	var ws WebSearchConfig
	if cfg != nil {
		ws = cfg.WebSearch
	}
	cfgLock.RUnlock()
	return resolveWebSearchDefaults(ws)
}

// resolveWebSearchDefaults fills zero-valued fields across every nested section
// with their defaults. Applied centrally so resolution is identical whether or
// not a config is loaded.
func resolveWebSearchDefaults(ws WebSearchConfig) WebSearchConfig {
	// Routing.
	if strings.TrimSpace(ws.Routing.Mode) == "" {
		ws.Routing.Mode = DefaultWebSearchRoutingMode
	}
	if strings.TrimSpace(ws.Routing.PrimaryProvider) == "" {
		ws.Routing.PrimaryProvider = DefaultWebSearchPrimaryProvider
	}
	if ws.Routing.FallbackProviders == nil {
		ws.Routing.FallbackProviders = []string{ProviderTavily}
	}

	// Limits.
	if ws.Limits.MaxRounds <= 0 {
		ws.Limits.MaxRounds = DefaultWebSearchMaxRounds
	}
	if ws.Limits.MaxSearchesPerRequest <= 0 {
		ws.Limits.MaxSearchesPerRequest = DefaultWebSearchMaxSearches
	}
	if ws.Limits.MaxConcurrentSearches <= 0 {
		ws.Limits.MaxConcurrentSearches = DefaultWebSearchMaxConcurrency
	}
	if ws.Limits.MaxResultsPerSearch <= 0 {
		ws.Limits.MaxResultsPerSearch = DefaultWebSearchMaxResults
	}
	if ws.Limits.TotalTimeoutSeconds <= 0 {
		ws.Limits.TotalTimeoutSeconds = DefaultWebSearchTotalTimeout
	}

	// SearXNG (primary, free). Enabled defaults to true.
	if ws.SearXNG.Enabled == nil {
		t := true
		ws.SearXNG.Enabled = &t
	}
	if strings.TrimSpace(ws.SearXNG.BaseURL) == "" {
		ws.SearXNG.BaseURL = DefaultSearXNGBaseURL
	}
	if ws.SearXNG.TimeoutSeconds <= 0 {
		ws.SearXNG.TimeoutSeconds = DefaultSearXNGTimeout
	}
	if strings.TrimSpace(ws.SearXNG.Language) == "" {
		ws.SearXNG.Language = "auto"
	}
	if ws.SearXNG.SafeSearch == nil {
		s := DefaultSearXNGSafeSearch
		ws.SearXNG.SafeSearch = &s
	}
	if len(ws.SearXNG.Categories) == 0 {
		ws.SearXNG.Categories = []string{"general"}
	}
	if ws.SearXNG.MinimumResults <= 0 {
		ws.SearXNG.MinimumResults = DefaultSearXNGMinResults
	}

	// Tavily (optional fallback). FreeOnly defaults to true.
	if ws.Tavily.FreeOnly == nil {
		t := true
		ws.Tavily.FreeOnly = &t
	}
	if ws.Tavily.MonthlyCreditLimit <= 0 {
		ws.Tavily.MonthlyCreditLimit = DefaultTavilyMonthlyCredits
	}
	if strings.TrimSpace(ws.Tavily.SearchDepth) == "" {
		ws.Tavily.SearchDepth = "basic"
	}
	if ws.Tavily.TimeoutSeconds <= 0 {
		ws.Tavily.TimeoutSeconds = DefaultTavilyTimeout
	}
	if ws.Tavily.RetryMax <= 0 {
		ws.Tavily.RetryMax = DefaultWebSearchRetryMax
	}
	if ws.Tavily.RetryBaseDelayMs <= 0 {
		ws.Tavily.RetryBaseDelayMs = DefaultWebSearchRetryBaseMs
	}

	// Cache. Enabled defaults to true.
	if ws.Cache.Enabled == nil {
		t := true
		ws.Cache.Enabled = &t
	}
	if strings.TrimSpace(ws.Cache.Provider) == "" {
		ws.Cache.Provider = "memory"
	}
	if ws.Cache.TTLSeconds <= 0 {
		ws.Cache.TTLSeconds = DefaultWebSearchCacheTTL
	}
	if ws.Cache.MaxEntries <= 0 {
		ws.Cache.MaxEntries = DefaultWebSearchCacheEntries
	}

	// Reranking.
	if strings.TrimSpace(ws.Reranking.Provider) == "" {
		ws.Reranking.Provider = "heuristic"
	}
	if ws.Reranking.MaxCandidates <= 0 {
		ws.Reranking.MaxCandidates = DefaultWebSearchRerankCandidates
	}
	if ws.Reranking.MaxFinalResults <= 0 {
		ws.Reranking.MaxFinalResults = DefaultWebSearchMaxResults
	}

	return ws
}

// WebSearchAppendSources reports whether a deterministic "Sources:" list should
// be appended to the final answer. Defaults to true when unset.
func WebSearchAppendSources() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.WebSearch.AppendSources == nil {
		return true
	}
	return *cfg.WebSearch.AppendSources
}

// WebSearchEmitNativeToolBlocks reports whether the proxy should synthesize
// Anthropic-native server_tool_use / web_search_tool_result content blocks (and
// the usage.server_tool_use.web_search_requests counter). Defaults to true when
// unset, so clients display the searches the proxy ran.
func WebSearchEmitNativeToolBlocks() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.WebSearch.EmitNativeToolBlocks == nil {
		return true
	}
	return *cfg.WebSearch.EmitNativeToolBlocks
}

// TavilyAPIKeyResolved returns the effective Tavily API key, with the
// TAVILY_API_KEY environment variable taking precedence over config.
func TavilyAPIKeyResolved() string {
	if env := strings.TrimSpace(os.Getenv("TAVILY_API_KEY")); env != "" {
		return env
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.WebSearch.Tavily.APIKey)
}

// WebSearchToggledOn reports only the operator toggle, independent of whether a
// provider is configured. Lets the handler distinguish "feature off" (pass
// through silently) from "on but misconfigured" (fail fast with a config error).
func WebSearchToggledOn() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.WebSearch.Enabled
}

// Usage reporting modes govern only the CLIENT-VISIBLE usage numbers. Internal
// accounting (per-key TokensUsed, per-account TotalTokens, global counters) is
// always upstream-accurate regardless of this setting; it never uses context
// occupancy as an input-token count.
const (
	// UsageReportingLegacy preserves the historical client-facing behavior:
	// input_tokens carries context-window occupancy (contextPct * window) when a
	// contextUsageEvent arrived. This is the default so existing clients that key
	// off the old number are not broken.
	UsageReportingLegacy = "legacy"
	// UsageReportingAccurate reports the upstream-accurate input token count to the
	// client (upstream-first, estimator fallback), matching the internal sinks.
	UsageReportingAccurate = "accurate"
)

// GetUsageReportingMode returns the client-facing usage reporting mode, read from
// the KIRO_USAGE_REPORTING environment variable (values "legacy" or "accurate";
// anything else, including empty, means legacy). It is intentionally env-driven
// rather than a config-schema field so enabling accurate reporting is a single
// operator toggle with a trivial rollback (unset the var) and no persisted state.
func GetUsageReportingMode() string {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("KIRO_USAGE_REPORTING"))) {
	case UsageReportingAccurate:
		return UsageReportingAccurate
	default:
		return UsageReportingLegacy
	}
}

// SearXNGProviderEnabled reports whether SearXNG is usable: enabled (default
// true) with a non-empty base URL. No API key is needed — it is the free path.
func SearXNGProviderEnabled() bool {
	ws := GetWebSearchConfig()
	return ws.SearXNG.Enabled != nil && *ws.SearXNG.Enabled && strings.TrimSpace(ws.SearXNG.BaseURL) != ""
}

// TavilyProviderEnabled reports whether Tavily is usable as a fallback: turned on
// AND a key is resolvable (env or config).
func TavilyProviderEnabled() bool {
	ws := GetWebSearchConfig()
	return ws.Tavily.Enabled && TavilyAPIKeyResolved() != ""
}

// WebSearchAllowPaidUsage reports the hard paid-usage gate. When false (default),
// no provider may incur cost regardless of per-provider settings.
func WebSearchAllowPaidUsage() bool {
	ws := GetWebSearchConfig()
	return ws.Routing.AllowPaidUsage
}

// WebSearchEnabled reports whether the server-side search sub-loop should run:
// the operator toggle is on AND at least one provider is usable. Free-first:
// SearXNG alone (no API key) satisfies this; Tavily is not required.
func WebSearchEnabled() bool {
	if !WebSearchToggledOn() {
		return false
	}
	return SearXNGProviderEnabled() || TavilyProviderEnabled()
}

// GetMemoryConfig returns the memory sidecar settings with zero-valued fields
// resolved to their defaults. The APIKey is returned as-is (mask at the admin
// boundary, not here). The returned value is a copy; callers cannot mutate
// shared config state.
func GetMemoryConfig() MemoryConfig {
	cfgLock.RLock()
	var m MemoryConfig
	if cfg != nil {
		m = cfg.Memory
	}
	cfgLock.RUnlock()
	return resolveMemoryDefaults(m)
}

// GetMemoryConfigRaw returns the stored memory settings WITHOUT resolving
// zero-valued fields to their defaults. Use this as the base for a partial
// admin patch: reading the resolved copy and saving it would persist the
// current defaults as explicit values, freezing them against future default
// changes (default-drift). The returned value is a copy.
func GetMemoryConfigRaw() MemoryConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return MemoryConfig{}
	}
	return cfg.Memory
}

// resolveMemoryDefaults fills zero-valued fields with their defaults. Applied
// centrally so resolution is identical whether or not a config is loaded.
func resolveMemoryDefaults(m MemoryConfig) MemoryConfig {
	if strings.TrimSpace(m.Provider) == "" {
		m.Provider = DefaultMemoryProvider
	}
	if strings.TrimSpace(m.WriteMode) == "" {
		m.WriteMode = DefaultMemoryWriteMode
	}
	if m.RetrievalLimit <= 0 {
		m.RetrievalLimit = DefaultMemoryRetrievalLimit
	}
	if m.Timeouts.SearchMs <= 0 {
		m.Timeouts.SearchMs = DefaultMemorySearchTimeoutMs
	}
	if m.Timeouts.WriteMs <= 0 {
		m.Timeouts.WriteMs = DefaultMemoryWriteTimeoutMs
	}
	// RedactSecrets defaults to true (nil → on).
	if m.Redaction.RedactSecrets == nil {
		t := true
		m.Redaction.RedactSecrets = &t
	}
	// FailOpen defaults to true (nil → on): a memory outage never breaks a request.
	if m.FailOpen == nil {
		t := true
		m.FailOpen = &t
	}
	// Inject defaults to false (nil → off): reading/injecting memory into the LLM
	// request is opt-in, independent of the store/retrieve toggle.
	if m.Inject == nil {
		f := false
		m.Inject = &f
	}
	if m.MaxInjectTokens <= 0 {
		m.MaxInjectTokens = DefaultMemoryMaxInjectTokens
	}
	return m
}

// UpdateMemoryConfig saves the memory sidecar settings atomically.
func UpdateMemoryConfig(m MemoryConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Memory = m
	return Save()
}

// MemoryEnabled reports whether the memory sidecar should be active: the
// operator toggle is on AND a backend base URL is configured. Mirrors the
// WebSearchToggledOn/provider-usable split so a bare toggle without a backend
// resolves to a no-op provider rather than erroring.
func MemoryEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.Memory.Enabled && strings.TrimSpace(cfg.Memory.BaseURL) != ""
}

// MemoryRedactSecrets reports whether secret masking is applied before a write.
// Defaults to true when unset.
func MemoryRedactSecrets() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.Memory.Redaction.RedactSecrets == nil {
		return true
	}
	return *cfg.Memory.Redaction.RedactSecrets
}

// MemoryFailOpen reports whether backend errors degrade silently instead of
// surfacing to the caller. Defaults to true when unset.
func MemoryFailOpen() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.Memory.FailOpen == nil {
		return true
	}
	return *cfg.Memory.FailOpen
}

// MemoryInjectEnabled reports whether relevant memories should be retrieved and
// injected into the LLM request. Requires the sidecar to be usable (MemoryEnabled)
// AND the operator to have turned injection on (defaults off when unset), so a
// bare memory sidecar does not silently start rewriting requests.
func MemoryInjectEnabled() bool {
	if !MemoryEnabled() {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Memory.Inject != nil && *cfg.Memory.Inject
}

// MemoryCaptureEnabled reports whether a completed turn should be captured into
// memory. Requires the sidecar to be usable AND a non-explicit write mode
// ("automatic"/"curated"); "explicit" (the default) means store only via the
// admin API, never automatically from the request path.
func MemoryCaptureEnabled() bool {
	if !MemoryEnabled() {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	mode := strings.TrimSpace(cfg.Memory.WriteMode)
	if mode == "" {
		mode = DefaultMemoryWriteMode
	}
	return mode != MemoryWriteModeExplicit
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}

// GetUpstreamConfig returns copies of the configured upstream providers and model
// routes. Copies are returned so callers cannot mutate shared state without the lock.
func GetUpstreamConfig() ([]UpstreamProvider, []ModelRoute) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil, nil
	}
	providers := make([]UpstreamProvider, len(cfg.Upstreams))
	copy(providers, cfg.Upstreams)
	routes := make([]ModelRoute, len(cfg.ModelRoutes))
	copy(routes, cfg.ModelRoutes)
	return providers, routes
}

// UpdateUpstreamConfig replaces the upstream providers and model routes atomically
// and persists the change. Passing nil for either slice clears it.
//
// Legacy 1:1 routes are migrated into Targets here, not just on Load: the admin
// UI and the bundle importer both write routes carrying only UpstreamID, and
// ResolveRoute reads Targets exclusively. Without this the saved route resolves
// to nothing until the next restart re-runs the Load-time migration.
func UpdateUpstreamConfig(providers []UpstreamProvider, routes []ModelRoute) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	migrateModelRoutes(routes)
	cfg.Upstreams = providers
	cfg.ModelRoutes = routes
	return Save()
}
