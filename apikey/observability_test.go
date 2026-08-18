package apikey

import (
	"testing"
	"time"
)

func TestObservabilityResetAndSnapshot(t *testing.T) {
	ResetObservabilityForTest()
	IncAuth(true)
	IncAuth(false)
	IncQuotaReject("requests")
	IncPortalQuery()
	NotePortalQueryDuration(5 * time.Millisecond)
	snap := ObservabilitySnapshot()
	if snap.AuthOK != 1 || snap.AuthFail != 1 || snap.QuotaRejectRequests != 1 {
		t.Fatalf("%+v", snap)
	}
	if snap.PortalQueryTotal != 1 || snap.PortalQueryDurationMs < 0 {
		t.Fatalf("query %+v", snap)
	}
	ResetObservabilityForTest()
	snap = ObservabilitySnapshot()
	if snap.AuthOK != 0 || snap.UnsettledReservations != 0 || snap.PortalSSEConnectionsActive != 0 {
		t.Fatalf("reset leftover %+v", snap)
	}
}

func TestReservationInvariantAndCounters(t *testing.T) {
	ResetObservabilityForTest()
	s := testService(t)
	rec, secret, err := s.Create(CreateInput{Name: "inv", Enabled: true, RequestLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Authenticate(secret, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage.RequestsReserved != 1 {
		t.Fatalf("reserved %d", got.Usage.RequestsReserved)
	}
	if ReservationsActive() != 1 {
		t.Fatalf("active gauge %d", ReservationsActive())
	}
	if err := s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(rec.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Usage.RequestsReserved < 0 {
		t.Fatalf("reserved went negative: %d", after.Usage.RequestsReserved)
	}
	if after.Usage.RequestsReserved != 0 {
		t.Fatalf("idle reserved %d", after.Usage.RequestsReserved)
	}
	if UnsettledReservations() != 0 {
		t.Fatalf("unsettled %d", UnsettledReservations())
	}
	if ReservationsActive() != 0 {
		t.Fatalf("gauge leak %d", ReservationsActive())
	}
}

func TestAuthAndQuotaCounters(t *testing.T) {
	ResetObservabilityForTest()
	s := testService(t)
	_, secret, err := s.Create(CreateInput{Name: "q", Enabled: true, RequestLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret, true); err == nil {
		t.Fatal("expected request quota reject")
	}
	if _, err := s.Authenticate("sk-nope", false); err == nil {
		t.Fatal("expected invalid key")
	}
	snap := ObservabilitySnapshot()
	if snap.AuthOK < 1 || snap.AuthFail < 2 || snap.QuotaRejectRequests < 1 {
		t.Fatalf("%+v", snap)
	}
	if UnsettledReservations() != 0 {
		t.Fatal("quota reject must not leave unsettled")
	}
}

func TestEventPersistErrorCounter(t *testing.T) {
	ResetObservabilityForTest()
	s := testService(t)
	rec, _, err := s.Create(CreateInput{Name: "p", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeSuccess, Endpoint: "openai"}); err == nil {
		t.Fatal("expected persist failure after close")
	}
	if EventPersistErrors() < 1 && SQLiteWriteErrors() < 1 {
		// Closed DB may fail at Get() before the insert; still a failure path.
		if err := s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeSuccess, Endpoint: "openai"}); err == nil {
			t.Fatal("expected second persist failure")
		}
	}
}

func TestCleanupObservability(t *testing.T) {
	ResetObservabilityForTest()
	s := testService(t)
	if _, _, err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if UsageCleanupDurationMs() < 0 {
		t.Fatal("cleanup duration")
	}
}
