package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/apikey"
	"kiro-go/config"
	accountpool "kiro-go/pool"
)

const reservationClaudeBody = `{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
const reservationOpenAIBody = `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`
const reservationResponsesBody = `{"model":"claude-sonnet-4.5","input":"hi"}`

type reservationEnv struct {
	h      *Handler
	svc    *apikey.Service
	secret string
	rec    apikey.Record
}

func newReservationEnv(t *testing.T, requestLimit int64, accounts ...config.Account) *reservationEnv {
	t.Helper()
	mustInitConfig(t)
	on := true
	if err := config.UpdateSettingsPatch(nil, &on, ""); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatal(err)
	}
	pepper, err := config.GetOrCreateAPIKeyPepper()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apikey.Open(filepath.Join(t.TempDir(), "apikeys.db"), pepper, apikey.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	rec, secret, err := svc.Create(apikey.CreateInput{
		Name: "lease-test", Enabled: true, RequestLimit: requestLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		pool:               accountpool.NewTestPool(accounts...),
		keys:               svc,
		promptCache:        newPromptCacheTracker(defaultPromptCacheTTL),
		memory:             noopMemoryProvider{},
		conversationRunner: NewKiroConversationRunner(),
	}
	return &reservationEnv{h: h, svc: svc, secret: secret, rec: rec}
}

func (env *reservationEnv) post(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.secret)
	rec := httptest.NewRecorder()
	env.h.ServeHTTP(rec, req)
	return rec
}

func (env *reservationEnv) postCtx(ctx context.Context, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.secret)
	rec := httptest.NewRecorder()
	env.h.ServeHTTP(rec, req)
	return rec
}

func (env *reservationEnv) usage() apikey.Usage {
	got, err := env.svc.Get(env.rec.Key.ID)
	if err != nil {
		panic(err)
	}
	return got.Usage
}

func (env *reservationEnv) events() []apikey.PublicEvent {
	page, err := env.svc.ListEvents(env.rec.Key.ID, apikey.EventQuery{Limit: 50})
	if err != nil {
		panic(err)
	}
	return page.Items
}

func assertReservedZero(t *testing.T, env *reservationEnv) {
	t.Helper()
	u := env.usage()
	if u.RequestsReserved != 0 {
		t.Fatalf("requests_reserved leaked: %+v", u)
	}
}

func TestReservation_NoAccount503DoesNotLeak(t *testing.T) {
	env := newReservationEnv(t, 1)
	rec := env.post("/v1/messages", reservationClaudeBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No available accounts") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	assertReservedZero(t, env)
	u := env.usage()
	if u.RequestsTotal != 0 {
		t.Fatalf("503 must not consume request quota %+v", u)
	}
	if u.RequestsRejected != 1 {
		t.Fatalf("expected rejected=1 %+v", u)
	}
	evs := env.events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].Status != apikey.OutcomeRejected || evs[0].StatusCode != 503 {
		t.Fatalf("event %+v", evs[0])
	}
	if evs[0].TotalTokens != 0 || evs[0].Credits != 0 {
		t.Fatalf("503 charged tokens/credits %+v", evs[0])
	}
}

func TestReservation_Repeated503DoesNotFakeExhaust(t *testing.T) {
	env := newReservationEnv(t, 1)
	for i := 0; i < 5; i++ {
		rec := env.post("/v1/messages", reservationClaudeBody)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("iter %d: status=%d body=%s (quota fake-exhausted?)", i, rec.Code, rec.Body.String())
		}
		assertReservedZero(t, env)
	}
	u := env.usage()
	if u.RequestsTotal != 0 {
		t.Fatalf("repeated 503 consumed quota %+v", u)
	}
	if u.RequestsRejected != 5 {
		t.Fatalf("expected 5 rejected events in usage %+v", u)
	}
}

func TestReservation_ConcurrentFailureNoLeak(t *testing.T) {
	env := newReservationEnv(t, 3)
	const n = 20
	var wg sync.WaitGroup
	var status503, status429 int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			rec := env.post("/v1/messages", reservationClaudeBody)
			switch rec.Code {
			case http.StatusServiceUnavailable:
				atomic.AddInt64(&status503, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&status429, 1)
			default:
				t.Errorf("unexpected status %d %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	assertReservedZero(t, env)
	if status503 == 0 {
		t.Fatalf("expected some 503s, got 503=%d 429=%d", status503, status429)
	}
	u := env.usage()
	if u.RequestsTotal != 0 {
		t.Fatalf("concurrent 503 consumed quota %+v", u)
	}
	rec := env.post("/v1/messages", reservationClaudeBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("subsequent request blocked: %d %s usage=%+v", rec.Code, rec.Body.String(), env.usage())
	}
	assertReservedZero(t, env)
}

func TestReservation_OpenAIAndResponses503AlsoSettle(t *testing.T) {
	env := newReservationEnv(t, 2)
	openai := env.post("/v1/chat/completions", reservationOpenAIBody)
	if openai.Code != http.StatusServiceUnavailable {
		t.Fatalf("openai %d %s", openai.Code, openai.Body.String())
	}
	resp := env.post("/v1/responses", reservationResponsesBody)
	if resp.Code != http.StatusServiceUnavailable && resp.Code != http.StatusOK {
		// responses stream/non-stream 503 is sendOpenAIError 503, or stream
		// response.failed with 200 headers already. Non-stream uses sendOpenAIError.
		t.Fatalf("responses %d %s", resp.Code, resp.Body.String())
	}
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("responses expected 503, got %d %s", resp.Code, resp.Body.String())
	}
	assertReservedZero(t, env)
	if env.usage().RequestsTotal != 0 {
		t.Fatalf("quota consumed %+v", env.usage())
	}
}

func TestReservation_InvalidJSONReleasesSlot(t *testing.T) {
	env := newReservationEnv(t, 1)
	rec := env.post("/v1/messages", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertReservedZero(t, env)
	if env.usage().RequestsTotal != 0 {
		t.Fatalf("4xx consumed quota %+v", env.usage())
	}
	rec = env.post("/v1/messages", reservationClaudeBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("slot not reusable: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReservation_CountTokensDoesNotReserve(t *testing.T) {
	env := newReservationEnv(t, 1)
	rec := env.post("/v1/messages/count_tokens", reservationClaudeBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	u := env.usage()
	if u.RequestsReserved != 0 || u.RequestsTotal != 0 {
		t.Fatalf("count_tokens reserved/committed %+v", u)
	}
}

func TestReservation_503ThenSuccessStillRuns(t *testing.T) {
	env := newReservationEnv(t, 1)
	first := env.post("/v1/messages", reservationClaudeBody)
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	assertReservedZero(t, env)

	acct := config.Account{
		ID:          "acct-lease",
		Enabled:     true,
		AccessToken: "token-lease",
		ProfileArn:  "arn:aws:codewhisperer:profile/lease",
	}
	env.h.pool = accountpool.NewTestPool(acct)
	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()

	second := env.post("/v1/messages", reservationClaudeBody)
	if second.Code != http.StatusOK {
		t.Fatalf("subsequent valid request failed: %d %s usage=%+v", second.Code, second.Body.String(), env.usage())
	}
	assertReservedZero(t, env)
	u := env.usage()
	if u.RequestsSuccess != 1 || u.RequestsTotal != 1 {
		t.Fatalf("expected one success after 503 %+v", u)
	}
}

func TestReservation_SuccessAndProviderFailureAndMissingUsage(t *testing.T) {
	acct := config.Account{
		ID:          "acct-matrix",
		Enabled:     true,
		AccessToken: "token-matrix",
		ProfileArn:  "arn:aws:codewhisperer:profile/matrix",
	}
	env := newReservationEnv(t, 10, acct)

	fb := newFakeKiroBackend(t, caseAFrames()...)
	defer swapKiroEndpointsForTest(t, fb.server)()
	ok := env.post("/v1/messages", reservationClaudeBody)
	if ok.Code != http.StatusOK {
		t.Fatalf("success %d %s", ok.Code, ok.Body.String())
	}
	assertReservedZero(t, env)
	if env.usage().RequestsSuccess != 1 {
		t.Fatalf("success usage %+v", env.usage())
	}

	streamOK := env.post("/v1/messages", `{"model":"claude-sonnet-4.5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if streamOK.Code != http.StatusOK {
		t.Fatalf("stream success %d %s", streamOK.Code, streamOK.Body.String())
	}
	assertReservedZero(t, env)

	// Missing usage: text + terminal metadata only. Still a handled success.
	missing := newFakeKiroBackend(t, kiroFrame{
		eventType: "assistantResponseEvent",
		payload:   map[string]interface{}{"content": "no usage"},
	})
	defer swapKiroEndpointsForTest(t, missing.server)()
	miss := env.post("/v1/messages", reservationClaudeBody)
	if miss.Code != http.StatusOK {
		t.Fatalf("missing usage %d %s", miss.Code, miss.Body.String())
	}
	assertReservedZero(t, env)

	// Provider failure: upstream 500, executor exhausts the one account.
	fail := newFakeKiroBackend(t)
	fail.statusCode = http.StatusInternalServerError
	defer swapKiroEndpointsForTest(t, fail.server)()
	bad := env.post("/v1/messages", reservationClaudeBody)
	if bad.Code != http.StatusInternalServerError && bad.Code != http.StatusBadGateway && bad.Code != http.StatusServiceUnavailable {
		t.Fatalf("provider fail %d %s", bad.Code, bad.Body.String())
	}
	assertReservedZero(t, env)
	u := env.usage()
	if u.RequestsFailed < 1 {
		t.Fatalf("expected failed counter %+v events=%+v", u, env.events())
	}
}

func TestReservation_TimeoutSettlesCancelled(t *testing.T) {
	acct := config.Account{
		ID:          "acct-to",
		Enabled:     true,
		AccessToken: "token-to",
		ProfileArn:  "arn:aws:codewhisperer:profile/to",
	}
	env := newReservationEnv(t, 2, acct)
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	defer swapKiroEndpointsForTest(t, hang)()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	rec := env.postCtx(ctx, "/v1/messages", reservationClaudeBody)
	_ = rec
	assertReservedZero(t, env)
	u := env.usage()
	if u.RequestsCancelled < 1 && u.RequestsRejected < 1 && u.RequestsFailed < 1 {
		t.Fatalf("timeout did not settle %+v events=%+v", u, env.events())
	}
	if u.OutputTokens != 0 {
		t.Fatalf("timeout before output charged output %+v", u)
	}
}

func TestReservation_ClientCancelSettles(t *testing.T) {
	acct := config.Account{
		ID:          "acct-cx",
		Enabled:     true,
		AccessToken: "token-cx",
		ProfileArn:  "arn:aws:codewhisperer:profile/cx",
	}
	env := newReservationEnv(t, 2, acct)
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	defer swapKiroEndpointsForTest(t, hang)()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = env.postCtx(ctx, "/v1/messages", reservationClaudeBody)
	assertReservedZero(t, env)
}

func TestReservation_StreamingPartialSettles(t *testing.T) {
	acct := config.Account{
		ID:          "acct-tr",
		Enabled:     true,
		AccessToken: "token-tr",
		ProfileArn:  "arn:aws:codewhisperer:profile/tr",
	}
	env := newReservationEnv(t, 2, acct)
	fb := newFakeKiroBackend(t, kiroFrame{
		eventType: "assistantResponseEvent",
		payload:   map[string]interface{}{"content": "partial"},
	})
	fb.truncate = true
	defer swapKiroEndpointsForTest(t, fb.server)()

	rec := env.post("/v1/messages", `{"model":"claude-sonnet-4.5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	_ = rec
	assertReservedZero(t, env)
	if len(env.events()) == 0 {
		t.Fatalf("truncated stream persisted no event")
	}
}

func TestReservation_LateUsageStillSuccess(t *testing.T) {
	acct := config.Account{
		ID:          "acct-late",
		Enabled:     true,
		AccessToken: "token-late",
		ProfileArn:  "arn:aws:codewhisperer:profile/late",
	}
	env := newReservationEnv(t, 2, acct)
	fb := newFakeKiroBackend(t,
		kiroFrame{eventType: "assistantResponseEvent", payload: map[string]interface{}{"content": "hello"}},
		kiroFrame{eventType: "usageEvent", payload: map[string]interface{}{
			"usage": map[string]interface{}{"inputTokens": 11, "outputTokens": 7},
		}},
		kiroFrame{eventType: "meteringEvent", payload: map[string]interface{}{"usage": 0.5}},
	)
	defer swapKiroEndpointsForTest(t, fb.server)()
	rec := env.post("/v1/messages", reservationClaudeBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("late usage %d %s", rec.Code, rec.Body.String())
	}
	assertReservedZero(t, env)
	if env.usage().RequestsSuccess != 1 {
		t.Fatalf("usage %+v", env.usage())
	}
}

func TestOutcomeForHTTPStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{503, apikey.OutcomeRejected},
		{400, apikey.OutcomeRejected},
		{404, apikey.OutcomeRejected},
		{429, apikey.OutcomeFailed},
		{500, apikey.OutcomeFailed},
		{502, apikey.OutcomeFailed},
		{499, apikey.OutcomeCancelled},
		{408, apikey.OutcomeCancelled},
	}
	for _, tc := range cases {
		if got := outcomeForHTTPStatus(tc.status); got != tc.want {
			t.Fatalf("status %d: got %s want %s", tc.status, got, tc.want)
		}
	}
}

func TestPortalSSEIsolatedAndReconnect(t *testing.T) {
	h, svc := testKeyHandler(t)
	a, sa, err := svc.Create(apikey.CreateInput{Name: "alice", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := svc.Create(apikey.CreateInput{Name: "bob", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Commit(b.Key.ID, apikey.CommitInput{
		RequestID: "bob-secret", Outcome: apikey.OutcomeSuccess, InputTokens: 77, Endpoint: "openai",
	})
	_ = a

	srv := httptest.NewServer(http.HandlerFunc(h.handlePortal))
	t.Cleanup(srv.Close)

	open := func() []*http.Cookie {
		res, err := http.Post(srv.URL+"/portal/api/session", "application/json", strings.NewReader(`{"key":"`+sa+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("session %d", res.StatusCode)
		}
		return res.Cookies()
	}

	readStream := func(cookies []*http.Cookie, want string, emit func()) string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/portal/api/events/stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if emit != nil {
			time.Sleep(40 * time.Millisecond)
			emit()
		}
		var b strings.Builder
		buf := make([]byte, 512)
		for {
			n, rerr := res.Body.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if strings.Contains(b.String(), want) {
				cancel()
				break
			}
			if rerr != nil {
				break
			}
		}
		return b.String()
	}

	cookies := open()
	body := readStream(cookies, "alice-live", func() {
		_ = svc.Commit(b.Key.ID, apikey.CommitInput{RequestID: "bob-live", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
		_ = svc.Commit(a.Key.ID, apikey.CommitInput{RequestID: "alice-live", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	})
	if strings.Contains(body, "bob-secret") || strings.Contains(body, "bob-live") {
		t.Fatalf("alice SSE leaked bob event: %s", body)
	}
	if !strings.Contains(body, "alice-live") {
		t.Fatalf("alice SSE missed own live event: %s", body)
	}

	body2 := readStream(cookies, "alice-live-2", func() {
		_ = svc.Commit(b.Key.ID, apikey.CommitInput{RequestID: "bob-live-2", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
		_ = svc.Commit(a.Key.ID, apikey.CommitInput{RequestID: "alice-live-2", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	})
	if strings.Contains(body2, "bob-live") || strings.Contains(body2, "bob-secret") {
		t.Fatalf("reconnect leaked bob: %s", body2)
	}
	if !strings.Contains(body2, "alice-live-2") {
		t.Fatalf("reconnect missed alice-live-2: %s", body2)
	}
}

func TestLeaseNoteSuccessIsSticky(t *testing.T) {
	ctx := context.WithValue(context.Background(), apiKeyLeaseContextKey{}, &apiKeyLease{
		keyID: "k", endpoint: "claude", start: time.Now(),
	})
	if !noteAPIKeyOutcome(ctx, apikey.CommitInput{Outcome: apikey.OutcomeSuccess, InputTokens: 4}) {
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
