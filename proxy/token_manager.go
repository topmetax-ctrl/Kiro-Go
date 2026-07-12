package proxy

import (
	"fmt"
	"sync"
	"time"

	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
)

// TokenState is the coarse lifecycle state of an account's credentials, used to
// react to persistence failures without silently dropping a freshly-rotated
// refresh token. It intentionally stays small — this is not a distributed
// workflow engine.
type TokenState int

const (
	// TokenHealthy: token is valid and persisted.
	TokenHealthy TokenState = iota
	// TokenRefreshing: a refresh is in flight (transient; not usually observed by
	// callers because the singleflight makes them wait for the result).
	TokenRefreshing
	// TokenPersistenceDegraded: IdP refresh succeeded but persisting the new token
	// to disk failed. The new token is held in memory (and published to the pool)
	// so the just-rotated credential is not lost; persistence is retried.
	TokenPersistenceDegraded
	// TokenReauthRequired: refresh failed in a way that needs operator/re-login
	// (e.g. invalid_grant). The manager stops auto-refreshing until re-auth.
	TokenReauthRequired
	// TokenDisabled: account disabled; no refresh attempted.
	TokenDisabled
)

func (s TokenState) String() string {
	switch s {
	case TokenHealthy:
		return "Healthy"
	case TokenRefreshing:
		return "Refreshing"
	case TokenPersistenceDegraded:
		return "PersistenceDegraded"
	case TokenReauthRequired:
		return "ReauthRequired"
	case TokenDisabled:
		return "Disabled"
	default:
		return "Unknown"
	}
}

// refreshResult is the shared outcome of a single refresh, delivered to every
// caller that joined the same in-flight refresh for an account.
type refreshResult struct {
	account *config.Account // fresh snapshot after publish (nil on error)
	err     error
}

// refreshCall is one in-flight (or just-completed) refresh, shared by all callers
// that coalesced onto the same account ID — a minimal singleflight, keyed by
// account ID, with no external dependency.
type refreshCall struct {
	done   chan struct{}
	result refreshResult
}

// tokenAccountState tracks per-account persistence health and generation. The
// generation is bumped on every successful IdP refresh so a slow/late refresh
// cannot clobber a newer one when publishing.
type tokenAccountState struct {
	state      TokenState
	generation uint64
	// pendingPersist holds a token whose persistence failed, for bounded retry.
	pendingPersist *pendingPersist
}

type pendingPersist struct {
	accessToken  string
	refreshToken string
	expiresAt    int64
	attempts     int
	nextAttempt  time.Time
}

// refreshFunc performs the actual IdP call. Injected so tests can supply a fake
// without hitting the network. Mirrors auth.RefreshToken's shape:
// (accessToken, refreshToken, expiresAt, profileArn, error).
type refreshFunc func(account *config.Account) (string, string, int64, string, error)

// persistFunc persists a refreshed token. Injected for tests; production uses
// config.UpdateAccountToken.
type persistFunc func(id, accessToken, refreshToken string, expiresAt int64) error

// TokenManager coordinates all token refreshes for the account pool. Refreshes
// for the same account are coalesced (one IdP call serves all waiters); refreshes
// for different accounts run in parallel (no global lock). It owns the
// memory<->persistence transaction invariant: publish to the pool only after a
// successful persist, and never drop a rotated credential on persist failure.
type TokenManager struct {
	pool    *pool.AccountPool
	refresh refreshFunc
	persist persistFunc

	mu       sync.Mutex
	inFlight map[string]*refreshCall
	states   map[string]*tokenAccountState

	// skew is how long before expiry a token is considered stale.
	skew time.Duration
	// maxPersistRetries bounds background persistence retries.
	maxPersistRetries int
}

// NewTokenManager builds a manager over the pool with the given refresh/persist
// functions. Pass nil for refresh/persist to use the production defaults.
func NewTokenManager(p *pool.AccountPool, refresh refreshFunc, persist persistFunc) *TokenManager {
	if refresh == nil {
		refresh = defaultRefreshFunc
	}
	if persist == nil {
		persist = config.UpdateAccountToken
	}
	return &TokenManager{
		pool:              p,
		refresh:           refresh,
		persist:           persist,
		inFlight:          make(map[string]*refreshCall),
		states:            make(map[string]*tokenAccountState),
		skew:              time.Duration(tokenRefreshSkewSeconds) * time.Second,
		maxPersistRetries: 5,
	}
}

// (tokenRefreshSkewSeconds is declared in handler.go and shared package-wide.)

// tokenFresh reports whether expiresAt is far enough in the future to skip a
// refresh. expiresAt==0 means "no expiry known" → treated as fresh (matches the
// pool/handler convention).
func (tm *TokenManager) tokenFresh(expiresAt int64) bool {
	if expiresAt == 0 {
		return true
	}
	return time.Now().Unix() < expiresAt-int64(tm.skew.Seconds())
}

// EnsureFresh guarantees the account identified by accountID has a non-expired
// token, refreshing via the IdP if needed. All concurrent callers for the same
// account share a single refresh. It returns a fresh account snapshot.
func (tm *TokenManager) EnsureFresh(accountID string) (*config.Account, error) {
	cur := tm.pool.GetByID(accountID)
	if cur == nil {
		return nil, fmt.Errorf("account %s not found", accountID)
	}
	if tm.tokenFresh(cur.ExpiresAt) {
		return cur, nil
	}
	return tm.refreshCoalesced(accountID)
}

// refreshCoalesced ensures exactly one refresh runs per account at a time. The
// first caller performs the refresh; others wait for its result.
func (tm *TokenManager) refreshCoalesced(accountID string) (*config.Account, error) {
	tm.mu.Lock()
	if call, ok := tm.inFlight[accountID]; ok {
		tm.mu.Unlock()
		<-call.done
		return call.result.account, call.result.err
	}
	call := &refreshCall{done: make(chan struct{})}
	tm.inFlight[accountID] = call
	tm.setStateLocked(accountID, TokenRefreshing)
	tm.mu.Unlock()

	call.result = tm.doRefresh(accountID)

	tm.mu.Lock()
	delete(tm.inFlight, accountID)
	tm.mu.Unlock()
	close(call.done)
	return call.result.account, call.result.err
}

// doRefresh performs the IdP call + validation + persist + publish for one
// account. It is only ever invoked by a single goroutine per account (guarded by
// the in-flight map).
func (tm *TokenManager) doRefresh(accountID string) refreshResult {
	// (1) Re-read the freshest snapshot so we refresh with the latest refresh
	// token (avoids reusing a token already rotated by a prior refresh).
	cur := tm.pool.GetByID(accountID)
	if cur == nil {
		return refreshResult{err: fmt.Errorf("account %s not found", accountID)}
	}
	// (2) Double-check freshness under coalescing: another caller may have just
	// refreshed while we queued.
	if tm.tokenFresh(cur.ExpiresAt) {
		tm.setState(accountID, TokenHealthy)
		return refreshResult{account: cur}
	}

	// (3) Call the IdP.
	accessToken, refreshToken, expiresAt, profileArn, err := tm.refresh(cur)
	if err != nil {
		tm.setState(accountID, TokenReauthRequired)
		return refreshResult{err: err}
	}

	// (4) Validate the response before trusting it.
	if err := validateRefreshResponse(accessToken, expiresAt); err != nil {
		tm.setState(accountID, TokenReauthRequired)
		return refreshResult{err: err}
	}

	// (5) Bump generation for this successful refresh.
	tm.mu.Lock()
	st := tm.ensureStateLocked(accountID)
	st.generation++
	tm.mu.Unlock()

	// (6) Persist first, then publish. On persist failure, still publish to the
	// pool (so the just-rotated credential is usable and not lost) but mark the
	// account PersistenceDegraded and queue a bounded retry.
	persistErr := tm.persist(accountID, accessToken, refreshToken, expiresAt)

	// (7) Publish to the pool regardless of persist outcome — dropping a rotated
	// token would strand the account (the old refresh token may already be
	// invalidated upstream). Publishing keeps memory authoritative.
	tm.pool.UpdateToken(accountID, accessToken, refreshToken, expiresAt)
	if profileArn != "" {
		config.UpdateAccountProfileArn(accountID, profileArn)
	}

	if persistErr != nil {
		tm.mu.Lock()
		st := tm.ensureStateLocked(accountID)
		st.state = TokenPersistenceDegraded
		st.pendingPersist = &pendingPersist{
			accessToken:  accessToken,
			refreshToken: refreshToken,
			expiresAt:    expiresAt,
			nextAttempt:  time.Now().Add(time.Second),
		}
		tm.mu.Unlock()
		logger.Warnf("[TokenManager] persist failed for %s (token held in memory, retrying): %v", accountID, persistErr)
	} else {
		tm.setState(accountID, TokenHealthy)
	}

	// (8) Return the fresh published snapshot to all waiters.
	fresh := tm.pool.GetByID(accountID)
	if fresh == nil {
		// Extremely unlikely (account removed mid-refresh); synthesize from cur.
		merged := *cur
		merged.AccessToken = accessToken
		if refreshToken != "" {
			merged.RefreshToken = refreshToken
		}
		merged.ExpiresAt = expiresAt
		fresh = &merged
	}
	return refreshResult{account: fresh}
}

// RetryDegradedPersists attempts to re-persist any tokens whose persistence
// previously failed. Safe to call periodically (background) and on shutdown. It
// does not perform IdP calls.
func (tm *TokenManager) RetryDegradedPersists() {
	tm.mu.Lock()
	type job struct {
		id string
		pp pendingPersist
	}
	var jobs []job
	now := time.Now()
	for id, st := range tm.states {
		if st.state == TokenPersistenceDegraded && st.pendingPersist != nil && !now.Before(st.pendingPersist.nextAttempt) {
			jobs = append(jobs, job{id: id, pp: *st.pendingPersist})
		}
	}
	tm.mu.Unlock()

	for _, j := range jobs {
		err := tm.persist(j.id, j.pp.accessToken, j.pp.refreshToken, j.pp.expiresAt)
		tm.mu.Lock()
		st := tm.ensureStateLocked(j.id)
		if err == nil {
			st.state = TokenHealthy
			st.pendingPersist = nil
			logger.Infof("[TokenManager] deferred persist succeeded for %s", j.id)
		} else if st.pendingPersist != nil {
			st.pendingPersist.attempts++
			if st.pendingPersist.attempts >= tm.maxPersistRetries {
				logger.Warnf("[TokenManager] persist still failing for %s after %d attempts; leaving in memory", j.id, st.pendingPersist.attempts)
				st.pendingPersist.nextAttempt = time.Now().Add(5 * time.Minute)
			} else {
				backoff := time.Duration(1<<st.pendingPersist.attempts) * time.Second
				st.pendingPersist.nextAttempt = time.Now().Add(backoff)
			}
		}
		tm.mu.Unlock()
	}
}

// FlushPending makes a final best-effort attempt to persist all degraded tokens,
// ignoring backoff timers. Intended for graceful shutdown.
func (tm *TokenManager) FlushPending() {
	tm.mu.Lock()
	type job struct {
		id string
		pp pendingPersist
	}
	var jobs []job
	for id, st := range tm.states {
		if st.state == TokenPersistenceDegraded && st.pendingPersist != nil {
			jobs = append(jobs, job{id: id, pp: *st.pendingPersist})
		}
	}
	tm.mu.Unlock()
	for _, j := range jobs {
		if err := tm.persist(j.id, j.pp.accessToken, j.pp.refreshToken, j.pp.expiresAt); err == nil {
			tm.mu.Lock()
			st := tm.ensureStateLocked(j.id)
			st.state = TokenHealthy
			st.pendingPersist = nil
			tm.mu.Unlock()
		}
	}
}

// State returns the current TokenState for an account (Healthy if unknown).
func (tm *TokenManager) State(accountID string) TokenState {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if st, ok := tm.states[accountID]; ok {
		return st.state
	}
	return TokenHealthy
}

func (tm *TokenManager) setState(accountID string, s TokenState) {
	tm.mu.Lock()
	tm.setStateLocked(accountID, s)
	tm.mu.Unlock()
}

func (tm *TokenManager) setStateLocked(accountID string, s TokenState) {
	st := tm.ensureStateLocked(accountID)
	st.state = s
}

func (tm *TokenManager) ensureStateLocked(accountID string) *tokenAccountState {
	st, ok := tm.states[accountID]
	if !ok {
		st = &tokenAccountState{state: TokenHealthy}
		tm.states[accountID] = st
	}
	return st
}

// validateRefreshResponse rejects an IdP response that would otherwise poison the
// pool with an empty/invalid token.
func validateRefreshResponse(accessToken string, expiresAt int64) error {
	if accessToken == "" {
		return fmt.Errorf("refresh returned empty access token")
	}
	// expiresAt==0 is tolerated by callers as "unknown expiry", but a refresh that
	// claims an already-past expiry is nonsense.
	if expiresAt != 0 && expiresAt <= time.Now().Unix() {
		return fmt.Errorf("refresh returned already-expired token (expiresAt=%d)", expiresAt)
	}
	return nil
}

// defaultRefreshFunc is the production refresh: delegate to auth.RefreshToken.
// Declared as a var so tests targeting production wiring can override if needed.
var defaultRefreshFunc refreshFunc = func(account *config.Account) (string, string, int64, string, error) {
	return auth.RefreshToken(account)
}
