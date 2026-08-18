package apikey

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testService(t *testing.T) *Service {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "apikeys.db"), []byte("test-pepper-32-bytes-long!!!!!!"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateLookupRotateDelete(t *testing.T) {
	s := testService(t)
	rec, secret, err := s.Create(CreateInput{Name: "alpha", Enabled: true, TokenLimit: 1000})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if secret == "" || rec.Key.ID == "" {
		t.Fatalf("missing secret or id")
	}
	got, err := s.Lookup(secret)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Key.ID != rec.Key.ID {
		t.Fatalf("id mismatch")
	}
	if got.Masked() == secret {
		t.Fatalf("masked leaked plaintext")
	}

	rotated, newSecret, err := s.Rotate(rec.Key.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Key.ID != rec.Key.ID {
		t.Fatalf("rotate must keep id")
	}
	if _, err := s.Lookup(secret); err == nil {
		t.Fatalf("old secret must fail")
	}
	if _, err := s.Lookup(newSecret); err != nil {
		t.Fatalf("new secret: %v", err)
	}

	if err := s.Delete(rec.Key.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(rec.Key.ID); err != ErrNotFound {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestImportLegacyIdempotent(t *testing.T) {
	s := testService(t)
	entries := []LegacyKey{{
		ID: "keep-me", Name: "legacy", Key: "sk-legacy-plain", Enabled: true,
		CreatedAt: 1000, TokenLimit: 50, TokensUsed: 10, CreditsUsed: 1.5, RequestsCount: 3,
	}}
	r1, err := s.ImportLegacy(entries)
	if err != nil {
		t.Fatalf("import1: %v", err)
	}
	if r1.Imported != 1 {
		t.Fatalf("imported %d", r1.Imported)
	}
	r2, err := s.ImportLegacy(entries)
	if err != nil {
		t.Fatalf("import2: %v", err)
	}
	if r2.AlreadyPresent != 1 || r2.Imported != 0 {
		t.Fatalf("second import %+v", r2)
	}
	got, err := s.Lookup("sk-legacy-plain")
	if err != nil {
		t.Fatalf("lookup migrated: %v", err)
	}
	if got.Key.ID != "keep-me" || got.Usage.TotalTokens != 10 || got.Usage.Credits != 1.5 || got.Usage.RequestsTotal != 3 {
		t.Fatalf("retained fields: %+v %+v", got.Key, got.Usage)
	}
}

func TestQuotaRequestReservation(t *testing.T) {
	s := testService(t)
	_, secret, err := s.Create(CreateInput{Name: "q", Enabled: true, RequestLimit: 2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("auth1: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("auth2: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err == nil {
		t.Fatalf("expected request quota on third")
	} else if ae, ok := err.(*AuthError); !ok || ae.Message != "request limit exceeded" {
		t.Fatalf("want request limit error, got %v", err)
	}
}

func TestQuotaTokenSoftAndCommit(t *testing.T) {
	s := testService(t)
	rec, secret, err := s.Create(CreateInput{Name: "t", Enabled: true, TokenLimit: 100})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeSuccess, InputTokens: 60, OutputTokens: 50, Endpoint: "openai"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := s.Get(rec.Key.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Usage.TotalTokens != 110 || got.Usage.RequestsSuccess != 1 {
		t.Fatalf("usage %+v", got.Usage)
	}
	if _, err := s.Authenticate(secret, true); err == nil {
		t.Fatalf("expected token limit after commit")
	}
}

func TestDisabledAndExpired(t *testing.T) {
	s := testService(t)
	exp := s.now().Add(-time.Hour)
	_, secret, err := s.Create(CreateInput{Name: "x", Enabled: false})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Authenticate(secret, false); err == nil {
		t.Fatalf("disabled must fail")
	}
	on := true
	rec, _ := s.Lookup(secret)
	if _, err := s.Update(rec.Key.ID, UpdateInput{Enabled: &on, ExpiresAt: &exp}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.Authenticate(secret, false); err == nil {
		t.Fatalf("expired must fail")
	}
}

func TestCancelChargesKnownUsage(t *testing.T) {
	s := testService(t)
	rec, secret, err := s.Create(CreateInput{Name: "c", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeCancelled, InputTokens: 9, OutputTokens: 9, Credits: 1, UsageSource: UsageSourceStreamObserved, UsageEstimated: true}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, _ := s.Get(rec.Key.ID)
	if got.Usage.TotalTokens != 18 || got.Usage.Credits != 1 || got.Usage.RequestsCancelled != 1 || got.Usage.RequestsTotal != 1 {
		t.Fatalf("cancel usage %+v", got.Usage)
	}
	if got.Usage.RequestsAttempted() != 1 || got.Usage.RequestsQuotaConsumed() != 1 {
		t.Fatalf("counter helpers %+v", got.Usage)
	}
}

func TestRejectedIgnoresTokens(t *testing.T) {
	s := testService(t)
	rec, secret, err := s.Create(CreateInput{Name: "rj", Enabled: true, RequestLimit: 2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := s.Commit(rec.Key.ID, CommitInput{
		Outcome: OutcomeRejected, InputTokens: 99, OutputTokens: 99, Credits: 5,
		StatusCode: 503, ErrorCode: ErrorNoAvailableAccounts,
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, _ := s.Get(rec.Key.ID)
	if got.Usage.TotalTokens != 0 || got.Usage.Credits != 0 || got.Usage.RequestsRejected != 1 || got.Usage.RequestsTotal != 0 {
		t.Fatalf("rejected charged usage %+v", got.Usage)
	}
}

func TestFailedChargesKnownUsage(t *testing.T) {
	s := testService(t)
	rec, secret, err := s.Create(CreateInput{Name: "f2", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := s.Commit(rec.Key.ID, CommitInput{
		Outcome: OutcomeFailed, InputTokens: 12, OutputTokens: 4, Endpoint: "openai",
		ClientModel: "claude-sonnet-4.5", EffectiveModel: "claude-sonnet-4.5",
		StatusCode: 500, ErrorCode: ErrorProviderError, UsageSource: UsageSourceStreamObserved, UsageEstimated: true,
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, _ := s.Get(rec.Key.ID)
	if got.Usage.TotalTokens != 16 || got.Usage.RequestsFailed != 1 {
		t.Fatalf("failed usage %+v", got.Usage)
	}
	evs, n, err := s.ListEvents(rec.Key.ID, EventQuery{Limit: 5})
	if err != nil || n != 1 {
		t.Fatalf("events n=%d err=%v", n, err)
	}
	if err := ValidatePublicEvent(evs[0]); err != nil {
		t.Fatalf("contract: %v %+v", err, evs[0])
	}
	if evs[0].ClientModel == "" || evs[0].UsageSource != UsageSourceStreamObserved {
		t.Fatalf("event fields %+v", evs[0])
	}
}

func TestCommitRejectedReleasesReservation(t *testing.T) {
	s := testService(t)
	rec, secret, err := s.Create(CreateInput{Name: "r", Enabled: true, RequestLimit: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("auth: %v", err)
	}
	got, _ := s.Get(rec.Key.ID)
	if got.Usage.RequestsReserved != 1 {
		t.Fatalf("expected reserved=1, got %+v", got.Usage)
	}
	if err := s.Commit(rec.Key.ID, CommitInput{
		Outcome: OutcomeRejected, StatusCode: 503, ErrorCode: "api_error",
		SanitizedError: "No available accounts", Endpoint: "claude",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, _ = s.Get(rec.Key.ID)
	if got.Usage.RequestsReserved != 0 {
		t.Fatalf("rejected leaked reserved %+v", got.Usage)
	}
	if got.Usage.RequestsTotal != 0 || got.Usage.RequestsRejected != 1 || got.Usage.TotalTokens != 0 {
		t.Fatalf("rejected must not consume quota %+v", got.Usage)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("slot should be free after rejected settle: %v", err)
	}
	evs, n, err := s.ListEvents(rec.Key.ID, EventQuery{Limit: 10})
	if err != nil || n != 1 || evs[0].Status != OutcomeRejected || evs[0].StatusCode != 503 {
		t.Fatalf("event n=%d evs=%+v err=%v", n, evs, err)
	}
}

func TestCommitFailedConsumesRequestSlot(t *testing.T) {
	s := testService(t)
	rec, secret, err := s.Create(CreateInput{Name: "f", Enabled: true, RequestLimit: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeFailed, StatusCode: 500, ErrorCode: "api_error"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, _ := s.Get(rec.Key.ID)
	if got.Usage.RequestsReserved != 0 || got.Usage.RequestsFailed != 1 || got.Usage.RequestsTotal != 1 {
		t.Fatalf("failed usage %+v", got.Usage)
	}
	if _, err := s.Authenticate(secret, true); err == nil {
		t.Fatalf("failed should consume the request slot")
	}
}

func TestPortalIsolationAndSession(t *testing.T) {
	s := testService(t)
	a, sa, _ := s.Create(CreateInput{Name: "a", Enabled: true})
	b, _, _ := s.Create(CreateInput{Name: "b", Enabled: true})
	_ = s.Commit(a.Key.ID, CommitInput{Outcome: OutcomeSuccess, InputTokens: 5})
	_ = s.Commit(b.Key.ID, CommitInput{Outcome: OutcomeSuccess, InputTokens: 99})

	sid, rec, err := s.OpenSessionByKey(sa)
	if err != nil || rec.Key.ID != a.Key.ID {
		t.Fatalf("session: %v %+v", err, rec)
	}
	got, err := s.SessionRecord(sid)
	if err != nil || got.Key.ID != a.Key.ID {
		t.Fatalf("session record: %v", err)
	}
	events, _, err := s.ListEvents(got.Key.ID, EventQuery{Limit: 10})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range events {
		if ev.InputTokens == 99 {
			t.Fatalf("leaked key B event")
		}
	}

	tok, _, err := s.CreatePortalToken(a.Key.ID, time.Hour)
	if err != nil {
		t.Fatalf("portal token: %v", err)
	}
	sid2, rec2, err := s.OpenSessionByPortalToken(tok)
	if err != nil || rec2.Key.ID != a.Key.ID || sid2 == "" {
		t.Fatalf("open by token: %v", err)
	}
	if _, _, err := s.OpenSessionByPortalToken("pt-nope"); err == nil {
		t.Fatalf("bogus token")
	}
	_ = s.RevokePortalToken(a.Key.ID)
	if _, _, err := s.OpenSessionByPortalToken(tok); err == nil {
		t.Fatalf("revoked token still worked")
	}
}

func TestConcurrentRequestLimit(t *testing.T) {
	s := testService(t)
	_, secret, err := s.Create(CreateInput{Name: "race", Enabled: true, RequestLimit: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var ok, denied int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Authenticate(secret, true); err != nil {
				atomic.AddInt64(&denied, 1)
				return
			}
			atomic.AddInt64(&ok, 1)
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("expected exactly 1 reserved, got ok=%d denied=%d", ok, denied)
	}
}

func TestListFiltersAndPagination(t *testing.T) {
	s := testService(t)
	_, _, _ = s.Create(CreateInput{Name: "one", Enabled: true, TokenLimit: 10})
	disabled, _, _ := s.Create(CreateInput{Name: "two", Enabled: false})
	_ = disabled
	items, total, err := s.List(ListQuery{Status: "disabled", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Key.Name != "two" {
		t.Fatalf("filter disabled: total=%d items=%d", total, len(items))
	}
}

func TestResetUsage(t *testing.T) {
	s := testService(t)
	rec, _, _ := s.Create(CreateInput{Name: "r", Enabled: true})
	_ = s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeSuccess, InputTokens: 7})
	got, err := s.ResetUsage(rec.Key.ID)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got.Usage.TotalTokens != 0 || got.Usage.RequestsTotal != 0 {
		t.Fatalf("not zeroed %+v", got.Usage)
	}
}

func TestCleanupRetention(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "apikeys.db"), []byte("pepper"), Options{
		Retention: time.Hour,
		Now:       func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	rec, _, _ := s.Create(CreateInput{Name: "old", Enabled: true})
	// Insert an old event directly.
	_, err = s.db.Exec(`INSERT INTO request_events(request_id,key_id,ts,status) VALUES('r',?,?,'success')`,
		rec.Key.ID, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, _, err := s.Cleanup()
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected to delete old event, n=%d", n)
	}
}
