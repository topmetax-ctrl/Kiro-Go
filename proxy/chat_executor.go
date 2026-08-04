package proxy

import (
	"context"

	"kiro-go/config"
	"kiro-go/pool"
)

// streamGuard tracks whether any client-visible byte has been committed for a
// response. Once Commit() has been called, the account-retry loop MUST NOT fail
// over to another account: a second account's stream would corrupt (interleave
// with) the bytes already flushed to the client. Renderers call Commit() at the
// exact point they previously set their per-protocol "started" flag
// (Claude messageStarted, OpenAI/Responses responseStarted) — i.e. before/at the
// first client-visible content byte.
//
// Non-stream responses never Commit() (they buffer fully and only write on
// success), so a late upstream error can still retry — invariant #7. Making the
// "no retry after first byte" rule a property of this one type, checked in one
// place (ChatExecutor.Run), replaces the hand-maintained flag that used to be
// duplicated in every streaming handler.
//
// A guard is owned by a single request goroutine. The Kiro stream callbacks run
// synchronously in that goroutine (the same reason the plain-bool
// messageStarted/responseStarted flags it replaces needed no locking), so no
// synchronization is required here.
type streamGuard struct {
	committed bool
}

// Commit marks the response as having emitted a client-visible byte. Idempotent.
func (g *streamGuard) Commit() { g.committed = true }

// Committed reports whether Commit has been called.
func (g *streamGuard) Committed() bool { return g != nil && g.committed }

// attemptOutcome is what an attemptFunc reports back to the executor after one
// upstream call against a single account.
type attemptOutcome struct {
	// accountFailed is true when the upstream call failed in a way that is the
	// account's fault (a Kiro/CodeWhisperer error). The executor then excludes the
	// account and either retries the next one (guard not yet committed) or hands
	// off to the committed-failure renderer (guard committed). When false, the
	// attempt fully handled its response — either a success (with its own success
	// accounting + render) or a self-surfaced terminal error that must NOT fail the
	// account or trigger the exhaustion tail (e.g. a web_search misconfiguration).
	accountFailed bool
	// err is the upstream error, retained for the exhaustion 500 tail. Only
	// meaningful when accountFailed is true.
	err error
	// blameless marks a failure that should rotate onto another account WITHOUT
	// recording it against this one. Set for upstream-side faults such as a
	// truncated event stream: the account answered and its credentials are fine,
	// so cooling it down would drain a healthy pool over an upstream hiccup.
	// Only meaningful when accountFailed is true.
	blameless bool
}

// attemptHandled reports that the attempt fully rendered its outcome (success, or
// a self-surfaced terminal error). The executor stops with no further work.
func attemptHandled() attemptOutcome { return attemptOutcome{} }

// attemptAccountFailed reports that the upstream call failed and the account
// should be blamed/rotated. The executor retries (before commit) or renders the
// committed-failure surface (after commit).
func attemptAccountFailed(err error) attemptOutcome {
	return attemptOutcome{accountFailed: true, err: err}
}

// attemptRotateWithoutBlame reports a failure that is the upstream's fault, not
// the account's. The executor excludes the account for the rest of THIS request
// (so the retry lands elsewhere) but does not call onFailure, leaving the
// account's health untouched. Used for stream-integrity failures, where the
// account authenticated and answered but the event stream ended truncated.
func attemptRotateWithoutBlame(err error) attemptOutcome {
	return attemptOutcome{accountFailed: true, err: err, blameless: true}
}

// attemptFunc runs ONE upstream attempt against account, rendering the protocol's
// output as it goes. A streaming renderer MUST call guard.Commit() before/at the
// first client-visible content byte. It reports the outcome via attemptHandled()
// or attemptAccountFailed(err).
type attemptFunc func(ctx context.Context, account *config.Account) attemptOutcome

// committedFailureFunc renders a protocol's mid-stream (post-commit) failure
// surface. It is called at most once, only when an attempt reports
// attemptAccountFailed AFTER the guard was already committed — i.e. exactly the
// "no retry after first byte" branch. The executor does NOT record the account
// failure or the request failure here: the callback owns precisely what each
// protocol does, keeping the deliberate asymmetry:
//   - Claude stream:   handleAccountFailure + recordFailure + emit an `error` SSE.
//   - OpenAI stream:   handleAccountFailure + recordFailure, then go SILENT
//     (no error object, no [DONE]).
//   - Responses stream: recordFailure + emit `response.failed` (no
//     handleAccountFailure — matching the pre-refactor behavior).
//
// Non-stream handlers never commit, so their guard can never reach this branch;
// they pass nil.
type committedFailureFunc func(account *config.Account, err error)

// exhaustionFunc renders the loop's terminal tail after every attempt was used up
// without a handled outcome. noAccounts is true when no account was ever usable
// (503-class); otherwise every attempted account failed (500-class) and lastErr
// carries the final error. The callback owns any recordFailure accounting, since
// that too differs per protocol (e.g. Responses-stream emits response.failed with
// no recordFailure when noAccounts).
type exhaustionFunc func(noAccounts bool, lastErr error)

// ChatExecutor is the single account-retry loop shared by every inference
// protocol (Claude / OpenAI / Responses, stream and non-stream). It owns account
// selection, the excluded set, token validation, lastErr tracking, and — the one
// invariant that used to be copy-pasted six times — the guard.Committed() check
// that decides retry-vs-stop. It bakes in NO protocol-specific rendering or error
// shape; those live entirely in the injected callbacks.
//
// Dependencies are injected in the same style as ModelCache (see model_cache.go)
// and the conversation runner: the pool, Handler.ensureValidToken, and
// Handler.handleAccountFailure are passed as funcs so tests can supply fakes.
type ChatExecutor struct {
	pool        *pool.AccountPool
	ensureToken func(*config.Account) error
	onFailure   func(*config.Account, error)
	model       string
}

// newChatExecutor wires an executor for one request's model over the shared pool.
func newChatExecutor(
	p *pool.AccountPool,
	ensureToken func(*config.Account) error,
	onFailure func(*config.Account, error),
	model string,
) *ChatExecutor {
	return &ChatExecutor{pool: p, ensureToken: ensureToken, onFailure: onFailure, model: model}
}

// Run drives the account-retry loop. For each attempt it selects the next account
// for the model (excluding ones that already failed), ensures a valid token, then
// invokes attempt. On a token failure or an attemptAccountFailed outcome BEFORE
// the guard is committed, it excludes the account, records the failure, and tries
// the next account. On an attemptAccountFailed outcome AFTER the guard is
// committed, it invokes onCommitted (the "no retry after first byte" branch) and
// stops. When the attempts are exhausted with no handled outcome, it invokes
// onExhausted.
func (ex *ChatExecutor) Run(
	ctx context.Context,
	guard *streamGuard,
	attempt attemptFunc,
	onCommitted committedFailureFunc,
	onExhausted exhaustionFunc,
) {
	excluded := make(map[string]bool)
	var lastErr error

	for i := 0; i < maxAccountRetryAttempts; i++ {
		account := ex.pool.GetNextForModelExcluding(ex.model, excluded)
		if account == nil {
			break
		}
		if err := ex.ensureToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			ex.onFailure(account, err)
			continue
		}

		outcome := attempt(ctx, account)
		if !outcome.accountFailed {
			// Success, or a terminal error the attempt already surfaced. Stop.
			return
		}

		// The upstream call failed.
		if !guard.Committed() {
			// Nothing client-visible has been flushed yet: exclude this account and
			// retry onto the next one. A blameless outcome still rotates but leaves
			// the account's health untouched — the fault was upstream, not here.
			lastErr = outcome.err
			excluded[account.ID] = true
			if !outcome.blameless {
				ex.onFailure(account, outcome.err)
			}
			continue
		}

		// A client-visible byte was already flushed. The single structural
		// enforcement of "no retry after first byte": never fail over here. The
		// protocol's committed-failure renderer owns its own accounting/surface.
		if onCommitted != nil {
			onCommitted(account, outcome.err)
		}
		return
	}

	onExhausted(lastErr == nil, lastErr)
}
