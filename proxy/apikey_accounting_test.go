package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/apikey"
	"kiro-go/config"
)

func TestAccounting_MidStreamCancelChargesObserved(t *testing.T) {
	acct := config.Account{
		ID: "acct-partial", Enabled: true, AccessToken: "tok",
		ProfileArn: "arn:aws:codewhisperer:profile/partial",
	}
	env := newReservationEnv(t, 5, acct)
	payload := strings.Repeat("hello world ", 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": payload}))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Stay open long enough for the client to parse the frame after CallKiro's
		// connect jitter, then wait for the test's context cancel.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	defer swapKiroEndpointsForTest(t, srv)()
	kiroHttpStore.Store(&http.Client{Timeout: 0, Transport: &http.Transport{}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = env.postCtx(ctx, "/v1/messages", `{"model":"claude-sonnet-4.5","stream":true,"max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)
	assertReservedZero(t, env)
	u := env.usage()
	if u.RequestsCancelled < 1 && u.RequestsFailed < 1 {
		t.Fatalf("expected cancel/fail %+v events=%+v", u, env.events())
	}
	if u.OutputTokens == 0 {
		t.Fatalf("mid-stream cancel charged 0 output (quota bypass) %+v events=%+v", u, env.events())
	}
	evs := env.events()
	if len(evs) == 0 {
		t.Fatal("no event")
	}
	if evs[0].Status == apikey.OutcomeSuccess {
		t.Fatalf("cancel recorded as success %+v", evs[0])
	}
	if evs[0].ErrorCode != "" && !apikey.KnownErrorCode(evs[0].ErrorCode) {
		t.Fatalf("unknown errorCode %q", evs[0].ErrorCode)
	}
}

func TestAccounting_ProviderFailAfterPartialCharges(t *testing.T) {
	acct := config.Account{
		ID: "acct-partial-fail", Enabled: true, AccessToken: "tok",
		ProfileArn: "arn:aws:codewhisperer:profile/pf",
	}
	env := newReservationEnv(t, 3, acct)
	fb := newFakeKiroBackend(t, kiroFrame{
		eventType: "assistantResponseEvent",
		payload:   map[string]interface{}{"content": strings.Repeat("token ", 80)},
	})
	fb.truncate = true
	defer swapKiroEndpointsForTest(t, fb.server)()

	_ = env.post("/v1/messages", `{"model":"claude-sonnet-4.5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	assertReservedZero(t, env)
	u := env.usage()
	if u.RequestsReserved != 0 {
		t.Fatalf("leaked %+v", u)
	}
	if len(env.events()) == 0 {
		t.Fatal("missing event")
	}
	if env.events()[0].Status == apikey.OutcomeSuccess {
		t.Fatalf("truncated stream counted success %+v", env.events()[0])
	}
	if u.OutputTokens == 0 {
		t.Fatalf("partial output not charged %+v events=%+v", u, env.events())
	}
}

func TestAccounting_EventContractAcrossEndpoints(t *testing.T) {
	acct := config.Account{
		ID: "acct-contract", Enabled: true, AccessToken: "tok",
		ProfileArn: "arn:aws:codewhisperer:profile/contract",
	}
	env := newReservationEnv(t, 10, acct)

	// Rejected 503 still has client model from the parsed body.
	env.post("/v1/messages", reservationClaudeBody)
	env.post("/v1/chat/completions", reservationOpenAIBody)
	env.post("/v1/responses", reservationResponsesBody)
	env.post("/v1/messages", `{`)

	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()
	ok := env.post("/v1/messages", reservationClaudeBody)
	if ok.Code != http.StatusOK {
		t.Fatalf("success %d %s", ok.Code, ok.Body.String())
	}

	evs := env.events()
	if len(evs) < 4 {
		t.Fatalf("expected several events, got %d", len(evs))
	}
	var sawSuccess bool
	for _, ev := range evs {
		if err := apikey.ValidatePublicEvent(ev); err != nil {
			t.Errorf("contract: %v event=%+v", err, ev)
		}
		if ev.Status == apikey.OutcomeSuccess {
			sawSuccess = true
			if ev.ClientModel == "" && ev.EffectiveModel == "" {
				t.Errorf("success missing model %+v", ev)
			}
		}
		if ev.Status == apikey.OutcomeRejected && ev.ErrorCode == apikey.ErrorNoAvailableAccounts {
			if ev.ClientModel == "" {
				t.Errorf("503 missing clientModel %+v", ev)
			}
		}
	}
	if !sawSuccess {
		t.Fatal("no success event in contract set")
	}
}

func TestApiKeyLeasePanicSettlement(t *testing.T) {
	env := newReservationEnv(t, 1)
	before := apikey.UnsettledReservations()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reservationClaudeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.secret)
	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		env.h.serveInference(rec, req, "claude", env.h.authenticateForClaude, func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})
	}()
	assertReservedZero(t, env)
	evs := env.events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d %+v", len(evs), evs)
	}
	if evs[0].Status == apikey.OutcomeSuccess {
		t.Fatalf("panic settled as success %+v", evs[0])
	}
	if strings.Contains(strings.ToLower(evs[0].ErrorCode+evs[0].RequestID), "boom") {
		t.Fatalf("panic text leaked %+v", evs[0])
	}
	if env.secret != "" && strings.Contains(rec.Body.String(), env.secret) {
		t.Fatal("secret in response")
	}
	if apikey.UnsettledReservations() != before {
		t.Fatalf("panic path must Note internal_error so unsettled stays %d, got %d", before, apikey.UnsettledReservations())
	}
}

func TestApiKeyLeaseSettleExactlyOnce(t *testing.T) {
	env := newReservationEnv(t, 5)
	if _, err := env.svc.Authenticate(env.secret, true); err != nil {
		t.Fatal(err)
	}
	lease := &apiKeyLease{keyID: env.rec.Key.ID, endpoint: "claude", start: time.Now()}
	ctx := context.WithValue(context.Background(), apiKeyLeaseContextKey{}, lease)
	noteAPIKeyOutcome(ctx, apikey.CommitInput{Outcome: apikey.OutcomeSuccess, InputTokens: 7, OutputTokens: 3, RequestID: "once"})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			noteAPIKeyHTTPStatus(ctx, 500, "api_error", "should not double")
			env.h.settleAPIKeyLease(ctx)
		}()
	}
	wg.Wait()
	assertReservedZero(t, env)
	u := env.usage()
	if u.RequestsSuccess != 1 || u.TotalTokens != 10 {
		t.Fatalf("double settle? %+v", u)
	}
	evs := env.events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
}

func TestAccounting_UnsettledReservationIncrementsCounter(t *testing.T) {
	env := newReservationEnv(t, 1)
	before := apikey.UnsettledReservations()
	if _, err := env.svc.Authenticate(env.secret, true); err != nil {
		t.Fatal(err)
	}
	lease := &apiKeyLease{keyID: env.rec.Key.ID, endpoint: "claude", start: time.Now()}
	ctx := context.WithValue(context.Background(), apiKeyLeaseContextKey{}, lease)
	env.h.settleAPIKeyLease(ctx)
	if apikey.UnsettledReservations() != before+1 {
		t.Fatalf("unsettled %d want %d", apikey.UnsettledReservations(), before+1)
	}
	assertReservedZero(t, env)
	evs := env.events()
	if len(evs) != 1 || evs[0].ErrorCode != apikey.ErrorUnsettledReservation {
		t.Fatalf("events %+v", evs)
	}
}

func TestLeaseNoteSuccessIsStickyKeepsUsage(t *testing.T) {
	ctx := context.WithValue(context.Background(), apiKeyLeaseContextKey{}, &apiKeyLease{
		keyID: "k", endpoint: "claude", start: time.Now(),
	})
	if !noteAPIKeyOutcome(ctx, apikey.CommitInput{Outcome: apikey.OutcomeSuccess, InputTokens: 4, OutputTokens: 2}) {
		t.Fatal("expected lease")
	}
	noteAPIKeyHTTPStatus(ctx, 500, "api_error", "should not overwrite")
	l := leaseFromContext(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.input.Outcome != apikey.OutcomeSuccess || l.input.InputTokens != 4 {
		t.Fatalf("success was overwritten: %+v", l.input)
	}
}

func TestAccounting_MissingUsageStillSuccess(t *testing.T) {
	acct := config.Account{
		ID: "acct-miss", Enabled: true, AccessToken: "tok",
		ProfileArn: "arn:aws:codewhisperer:profile/miss",
	}
	env := newReservationEnv(t, 2, acct)
	missing := newFakeKiroBackend(t, kiroFrame{
		eventType: "assistantResponseEvent",
		payload:   map[string]interface{}{"content": "no usage"},
	})
	defer swapKiroEndpointsForTest(t, missing.server)()
	rec := env.post("/v1/messages", reservationClaudeBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	assertReservedZero(t, env)
	u := env.usage()
	if u.RequestsSuccess != 1 {
		t.Fatalf("usage %+v", u)
	}
	if u.TotalTokens == 0 {
		t.Fatalf("missing upstream usage should still estimate tokens %+v events=%+v", u, env.events())
	}
	if err := apikey.ValidatePublicEvent(env.events()[0]); err != nil {
		t.Fatal(err)
	}
}
