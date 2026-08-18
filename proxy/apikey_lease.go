package proxy

import (
	"context"
	"kiro-go/apikey"
	"kiro-go/logger"
	"net/http"
	"sync"
	"time"
)

// apiKeyLease is the single reservation lifecycle for one inference request.
//
// Invariant: every successful Authenticate(..., reserve=true) that binds a
// lease produces exactly one terminal Commit, even if the handler returns
// early, the client disconnects, or the process panics (defer still runs).
//
// Outcome (success/failed/cancelled/rejected) is independent of resource
// consumption. Handlers Note an outcome and may separately Note usage;
// settle() is the only Commit writer.
type apiKeyLease struct {
	keyID    string
	endpoint string
	start    time.Time

	mu      sync.Mutex
	noted   bool
	settled bool
	input   apikey.CommitInput
}

type apiKeyLeaseContextKey struct{}

// serveInference authenticates, binds a reservation lease, and guarantees
// settlement when the handler returns. Used for /v1/messages, /chat/completions
// and /responses — the only paths that reserve a request slot.
func (h *Handler) serveInference(
	w http.ResponseWriter,
	r *http.Request,
	endpoint string,
	authenticate func(http.ResponseWriter, *http.Request) *http.Request,
	handle func(http.ResponseWriter, *http.Request),
) {
	ar := authenticate(w, r)
	if ar == nil {
		return
	}
	ar = h.bindAPIKeyLease(ar, endpoint)
	w = wrapLeaseWriter(w, ar.Context())
	defer h.settleAPIKeyLease(ar.Context())
	defer func() {
		if rec := recover(); rec != nil {
			noteAPIKeyOutcome(ar.Context(), apikey.CommitInput{
				Outcome:        apikey.OutcomeFailed,
				StatusCode:     http.StatusInternalServerError,
				ErrorCode:      apikey.ErrorInternal,
				SanitizedError: "internal error",
			})
			panic(rec)
		}
	}()
	handle(w, ar)
}

func (h *Handler) bindAPIKeyLease(r *http.Request, endpoint string) *http.Request {
	if h.keys == nil || !shouldReserveRequest(r) {
		return r
	}
	id := apiKeyIDFromContext(r.Context())
	if id == "" {
		return r
	}
	lease := &apiKeyLease{
		keyID:    id,
		endpoint: endpoint,
		start:    time.Now(),
	}
	return r.WithContext(context.WithValue(r.Context(), apiKeyLeaseContextKey{}, lease))
}

func leaseFromContext(ctx context.Context) *apiKeyLease {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(apiKeyLeaseContextKey{}).(*apiKeyLease)
	return l
}

// noteAPIKeyOutcome records the terminal classification for a leased request.
// Success is sticky: a later failure note cannot overwrite a committed-to-client
// success. Usage fields merge monotonically so a later observe cannot drop
// tokens. Notes after settle are ignored.
func noteAPIKeyOutcome(ctx context.Context, in apikey.CommitInput) bool {
	l := leaseFromContext(ctx)
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled {
		return true
	}
	if in.RequestID == "" {
		in.RequestID = requestIDFromContext(ctx)
	}
	if in.Endpoint == "" {
		in.Endpoint = l.endpoint
	}
	if l.noted && l.input.Outcome == apikey.OutcomeSuccess {
		mergeCommitUsage(&l.input, in)
		return true
	}
	prev := l.input
	mergeCommitInput(&prev, in)
	if in.Outcome != "" {
		prev.Outcome = in.Outcome
		l.noted = true
	}
	l.input = prev
	return true
}

// noteAPIKeyMeta fills request identity fields without changing outcome.
func noteAPIKeyMeta(ctx context.Context, endpoint, clientModel, effectiveModel string, stream bool) {
	if leaseFromContext(ctx) == nil {
		return
	}
	in := apikey.CommitInput{
		Endpoint:       endpoint,
		ClientModel:    clientModel,
		EffectiveModel: effectiveModel,
		Stream:         stream,
	}
	l := leaseFromContext(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled {
		return
	}
	if in.Endpoint == "" {
		in.Endpoint = l.endpoint
	}
	mergeCommitInput(&l.input, in)
	if !l.noted {
		// Meta alone is not a terminal Note; settle still requires an outcome.
	}
}

// noteAPIKeyUsage merges known resource consumption without forcing an outcome.
func noteAPIKeyUsage(ctx context.Context, inputTokens, outputTokens int64, credits float64, source string, estimated bool) {
	if leaseFromContext(ctx) == nil {
		return
	}
	noteAPIKeyOutcome(ctx, apikey.CommitInput{
		InputTokens:    inputTokens,
		OutputTokens:   outputTokens,
		Credits:        credits,
		UsageSource:    source,
		UsageEstimated: estimated,
	})
}

// noteAPIKeyHTTPStatus maps an HTTP error that escaped after Reserve onto an
// outcome. 503 (no accounts) and ordinary 4xx never consume request quota so
// repeated failures cannot fake-exhaust the key. 5xx / 429 consume as failed.
func noteAPIKeyHTTPStatus(ctx context.Context, status int, errType, message string) {
	if leaseFromContext(ctx) == nil {
		return
	}
	in := apikey.CommitInput{
		Outcome:        outcomeForHTTPStatus(status),
		StatusCode:     status,
		ErrorCode:      apikey.ClassifyPublicError(status, errType, message),
		SanitizedError: message,
	}
	noteAPIKeyOutcome(ctx, in)
}

func outcomeForHTTPStatus(status int) string {
	switch {
	case status == 499 || status == http.StatusRequestTimeout:
		return apikey.OutcomeCancelled
	case status == http.StatusServiceUnavailable:
		return apikey.OutcomeRejected
	case status == http.StatusTooManyRequests:
		return apikey.OutcomeFailed
	case status >= 400 && status < 500:
		return apikey.OutcomeRejected
	default:
		return apikey.OutcomeFailed
	}
}

func mergeCommitInput(dst *apikey.CommitInput, src apikey.CommitInput) {
	if src.RequestID != "" {
		dst.RequestID = src.RequestID
	}
	if src.Endpoint != "" {
		dst.Endpoint = src.Endpoint
	}
	if src.ClientModel != "" {
		dst.ClientModel = src.ClientModel
	}
	if src.EffectiveModel != "" {
		dst.EffectiveModel = src.EffectiveModel
	}
	if src.StatusCode != 0 {
		dst.StatusCode = src.StatusCode
	}
	if src.LatencyMs != 0 {
		dst.LatencyMs = src.LatencyMs
	}
	if src.TTFBMs != 0 {
		dst.TTFBMs = src.TTFBMs
	}
	if src.Stream {
		dst.Stream = true
	}
	if src.ErrorCode != "" {
		dst.ErrorCode = src.ErrorCode
	}
	if src.SanitizedError != "" {
		dst.SanitizedError = src.SanitizedError
	}
	mergeCommitUsage(dst, src)
}

func mergeCommitUsage(dst *apikey.CommitInput, src apikey.CommitInput) {
	if src.InputTokens > dst.InputTokens {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > dst.OutputTokens {
		dst.OutputTokens = src.OutputTokens
	}
	if src.Credits > dst.Credits {
		dst.Credits = src.Credits
	}
	if src.UsageSource != "" {
		dst.UsageSource = preferUsageSource(dst.UsageSource, src.UsageSource)
	}
	if src.UsageEstimated {
		dst.UsageEstimated = true
	}
	if dst.UsageSource == apikey.UsageSourceUpstream && src.UsageSource == apikey.UsageSourceUpstream {
		dst.UsageEstimated = src.UsageEstimated && dst.UsageEstimated
	}
}

func preferUsageSource(cur, next string) string {
	rank := map[string]int{
		"":                               0,
		apikey.UsageSourceNone:           1,
		apikey.UsageSourceEstimator:      2,
		apikey.UsageSourceStreamObserved: 3,
		apikey.UsageSourceMetering:       4,
		apikey.UsageSourceUpstream:       5,
	}
	if rank[next] >= rank[cur] {
		return next
	}
	return cur
}

func (h *Handler) settleAPIKeyLease(ctx context.Context) {
	l := leaseFromContext(ctx)
	if l == nil || h.keys == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled {
		return
	}
	l.settled = true
	in := l.input
	if !l.noted {
		if ctx != nil && ctx.Err() != nil {
			in.Outcome = apikey.OutcomeCancelled
			in.StatusCode = 499
			in.ErrorCode = apikey.ErrorClientCancelled
			in.SanitizedError = "client canceled"
		} else {
			in.Outcome = apikey.OutcomeRejected
			in.ErrorCode = apikey.ErrorUnsettledReservation
			in.SanitizedError = "request ended without an explicit outcome"
			apikey.IncUnsettledReservation()
			logger.Warnf("[ApiKey] unsettled_reservation key=%s request=%s — handler forgot Note()",
				l.keyID, requestIDFromContext(ctx))
		}
	}
	if in.RequestID == "" {
		in.RequestID = requestIDFromContext(ctx)
	}
	if in.Endpoint == "" {
		in.Endpoint = l.endpoint
	}
	if in.LatencyMs == 0 && !l.start.IsZero() {
		in.LatencyMs = time.Since(l.start).Milliseconds()
	}
	if err := h.keys.Commit(l.keyID, in); err != nil {
		logger.Warnf("[ApiKey] settle %s for key %s: %v", in.Outcome, l.keyID, err)
	}
}

// leaseResponseWriter carries the lease context so sendClaudeError / sendOpenAIError
// can Note without threading context through every renderer signature.
type leaseResponseWriter struct {
	http.ResponseWriter
	ctx context.Context
}

func wrapLeaseWriter(w http.ResponseWriter, ctx context.Context) http.ResponseWriter {
	if _, ok := w.(*leaseResponseWriter); ok {
		return w
	}
	if leaseFromContext(ctx) == nil {
		return w
	}
	return &leaseResponseWriter{ResponseWriter: w, ctx: ctx}
}

func (w *leaseResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *leaseResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func leaseContextFromWriter(w http.ResponseWriter) context.Context {
	if lw, ok := w.(*leaseResponseWriter); ok {
		return lw.ctx
	}
	return nil
}
