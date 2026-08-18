package apikey

import "time"

const (
	StatusActive    = "active"
	StatusDisabled  = "disabled"
	StatusExpired   = "expired"
	StatusExhausted = "exhausted"

	ResetLifetime = "lifetime"
	ResetDaily    = "daily"
	ResetWeekly   = "weekly"
	ResetMonthly  = "monthly"

	EnforceSoft   = "soft"
	EnforceStrict = "strict"

	PeriodLifetime = "lifetime"

	OutcomeSuccess   = "success"
	OutcomeFailed    = "failed"
	OutcomeCancelled = "cancelled"
	OutcomeRejected  = "rejected"

	// UsageSource identifies how token counts were obtained. Credits, when
	// present, always come from an upstream metering event.
	UsageSourceNone           = "none"
	UsageSourceUpstream       = "upstream"
	UsageSourceMetering       = "metering_event"
	UsageSourceStreamObserved = "stream_observed"
	UsageSourceEstimator      = "estimator"
)

// Key is the durable identity of a client API key. The secret itself is never stored.
type Key struct {
	ID         string
	Name       string
	KeyPrefix  string
	KeyLast4   string
	Enabled    bool
	Migrated   bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
}

// Quota is the current limit set and reset window for a key.
type Quota struct {
	TokenLimit   int64
	CreditLimit  float64
	RequestLimit int64
	ResetPolicy  string
	PeriodStart  time.Time
	PeriodEnd    *time.Time
	// EnforcementMode is stored for V2. V1 always reserves request limits
	// exactly and always applies token/credit limits as a post-commit soft
	// check. A stored value of "strict" does not hard-limit tokens or credits.
	EnforcementMode string
}

// Usage is a counter row (lifetime or the current reset period).
type Usage struct {
	Period            string
	RequestsTotal     int64
	RequestsSuccess   int64
	RequestsFailed    int64
	RequestsCancelled int64
	RequestsRejected  int64
	RequestsReserved  int64
	InputTokens       int64
	OutputTokens      int64
	TotalTokens       int64
	Credits           float64
	LastUsedAt        *time.Time
}

// RequestsAttempted is every terminal classification, including admissions
// that never executed (rejected). Distinct from RequestsTotal.
func (u Usage) RequestsAttempted() int64 {
	return u.RequestsSuccess + u.RequestsFailed + u.RequestsCancelled + u.RequestsRejected
}

// RequestsQuotaConsumed is the request-limit counter: success + failed +
// cancelled. Rejected admissions are not consumed. Equal to RequestsTotal.
func (u Usage) RequestsQuotaConsumed() int64 {
	return u.RequestsTotal
}

// Record is a key plus its quota and the usage row that enforcement reads.
type Record struct {
	Key   Key
	Quota Quota
	Usage Usage
}

func (r Record) Status(now time.Time) string {
	if !r.Key.Enabled {
		return StatusDisabled
	}
	if r.Key.ExpiresAt != nil && !r.Key.ExpiresAt.IsZero() && !r.Key.ExpiresAt.After(now) {
		return StatusExpired
	}
	if overToken, overCredit, overRequest := r.OverLimit(); overToken || overCredit || overRequest {
		return StatusExhausted
	}
	return StatusActive
}

func (r Record) OverLimit() (overToken, overCredit, overRequest bool) {
	if r.Quota.TokenLimit > 0 && r.Usage.TotalTokens >= r.Quota.TokenLimit {
		overToken = true
	}
	if r.Quota.CreditLimit > 0 && r.Usage.Credits >= r.Quota.CreditLimit {
		overCredit = true
	}
	if r.Quota.RequestLimit > 0 && r.Usage.RequestsTotal+r.Usage.RequestsReserved >= r.Quota.RequestLimit {
		overRequest = true
	}
	return
}

func (r Record) RemainingTokens() *int64 {
	if r.Quota.TokenLimit <= 0 {
		return nil
	}
	left := r.Quota.TokenLimit - r.Usage.TotalTokens
	if left < 0 {
		left = 0
	}
	return &left
}

func (r Record) RemainingCredits() *float64 {
	if r.Quota.CreditLimit <= 0 {
		return nil
	}
	left := r.Quota.CreditLimit - r.Usage.Credits
	if left < 0 {
		left = 0
	}
	return &left
}

func (r Record) RemainingRequests() *int64 {
	if r.Quota.RequestLimit <= 0 {
		return nil
	}
	left := r.Quota.RequestLimit - r.Usage.RequestsTotal - r.Usage.RequestsReserved
	if left < 0 {
		left = 0
	}
	return &left
}

func (r Record) Masked() string {
	return MaskFromParts(r.Key.KeyPrefix, r.Key.KeyLast4)
}

// CreateInput is the admin create payload. Empty Key generates a new secret.
type CreateInput struct {
	Name            string
	Key             string
	Enabled         bool
	TokenLimit      int64
	CreditLimit     float64
	RequestLimit    int64
	ExpiresAt       *time.Time
	ResetPolicy     string
	EnforcementMode string
}

// UpdateInput is a sparse admin patch. Nil pointer means "leave unchanged".
type UpdateInput struct {
	Name            *string
	Enabled         *bool
	TokenLimit      *int64
	CreditLimit     *float64
	RequestLimit    *int64
	ExpiresAt       *time.Time
	ClearExpires    bool
	ResetPolicy     *string
	EnforcementMode *string
}

// ListQuery filters and pages the admin key list.
type ListQuery struct {
	Q      string
	Status string // active|disabled|expired|exhausted
	Quota  string // unlimited|credits|tokens|requests
	Usage  string // high|never|recent
	Sort   string
	Offset int
	Limit  int
}

// LegacyKey is the config.json shape imported on startup.
type LegacyKey struct {
	ID            string
	Name          string
	Key           string
	Enabled       bool
	Migrated      bool
	CreatedAt     int64
	LastUsedAt    int64
	TokenLimit    int64
	CreditLimit   float64
	TokensUsed    int64
	CreditsUsed   float64
	RequestsCount int64
}

type ImportResult struct {
	Imported       int
	AlreadyPresent int
	Skipped        int
}

type CommitInput struct {
	RequestID      string
	Outcome        string
	InputTokens    int64
	OutputTokens   int64
	Credits        float64
	Endpoint       string
	ClientModel    string
	EffectiveModel string
	StatusCode     int
	LatencyMs      int64
	TTFBMs         int64
	Stream         bool
	ErrorCode      string
	SanitizedError string
	UsageSource    string
	UsageEstimated bool
}

type EventQuery struct {
	From      *time.Time
	To        *time.Time
	Model     string
	Endpoint  string
	Status    string
	Stream    *bool
	ErrorCode string
	SortAsc   bool
	Offset    int
	Limit     int
}

type PublicEvent struct {
	EventID        int64     `json:"eventId,omitempty"`
	RequestID      string    `json:"requestId"`
	ApiKeyID       string    `json:"apiKeyId,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
	Endpoint       string    `json:"endpoint"`
	ClientModel    string    `json:"clientModel,omitempty"`
	EffectiveModel string    `json:"effectiveModel,omitempty"`
	Model          string    `json:"model"`
	StatusCode     int       `json:"statusCode"`
	Status         string    `json:"status"`
	InputTokens    int64     `json:"inputTokens"`
	OutputTokens   int64     `json:"outputTokens"`
	TotalTokens    int64     `json:"totalTokens"`
	Credits        float64   `json:"credits"`
	LatencyMs      int64     `json:"latencyMs"`
	TTFBMs         int64     `json:"ttfbMs,omitempty"`
	Streaming      bool      `json:"streaming"`
	ErrorCode      string    `json:"errorCode,omitempty"`
	UsageSource    string    `json:"usageSource,omitempty"`
	UsageEstimated bool      `json:"usageEstimated,omitempty"`
}

type HourBucket struct {
	Hour              time.Time `json:"hour"`
	Requests          int64     `json:"requests"`
	RequestsSuccess   int64     `json:"requestsSuccess"`
	RequestsFailed    int64     `json:"requestsFailed"`
	RequestsCancelled int64     `json:"requestsCancelled"`
	InputTokens       int64     `json:"inputTokens"`
	OutputTokens      int64     `json:"outputTokens"`
	TotalTokens       int64     `json:"totalTokens"`
	Credits           float64   `json:"credits"`
	LatencyMsAvg      float64   `json:"latencyMsAvg"`
	TTFBMsAvg         float64   `json:"ttfbMsAvg"`
}

type Summary struct {
	Record         Record
	Status         string
	SuccessRate    float64
	AvgLatencyMs   float64
	HasPortalToken bool
	NextReset      *time.Time
}

type PortalTokenInfo struct {
	Prefix    string
	CreatedAt time.Time
	ExpiresAt *time.Time
}

type Options struct {
	Retention time.Duration
	Now       func() time.Time
}
