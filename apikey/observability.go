package apikey

import "sync/atomic"

// Process-local counters for reservation/accounting invariants. These are not
// Prometheus; they integrate with whatever scrapes the process (tests, admin
// diagnostics). Production expectation: UnsettledReservations() == 0.
var (
	settlementsSuccess    atomic.Int64
	settlementsFailed     atomic.Int64
	settlementsCancelled  atomic.Int64
	settlementsRejected   atomic.Int64
	unsettledReservations atomic.Int64
	eventPersistErrors    atomic.Int64
)

func IncSettlement(outcome string) {
	switch outcome {
	case OutcomeSuccess:
		settlementsSuccess.Add(1)
	case OutcomeFailed:
		settlementsFailed.Add(1)
	case OutcomeCancelled:
		settlementsCancelled.Add(1)
	case OutcomeRejected:
		settlementsRejected.Add(1)
	}
}

func IncUnsettledReservation() { unsettledReservations.Add(1) }
func IncEventPersistError()    { eventPersistErrors.Add(1) }

func UnsettledReservations() int64 { return unsettledReservations.Load() }
func EventPersistErrors() int64    { return eventPersistErrors.Load() }

func SettlementTotals() (success, failed, cancelled, rejected int64) {
	return settlementsSuccess.Load(), settlementsFailed.Load(),
		settlementsCancelled.Load(), settlementsRejected.Load()
}

// ResetObservabilityForTest clears process counters. Tests that assert
// UnsettledReservations()==0 should call this in t.Cleanup if they expect a
// deliberate unsettled event.
func ResetObservabilityForTest() {
	settlementsSuccess.Store(0)
	settlementsFailed.Store(0)
	settlementsCancelled.Store(0)
	settlementsRejected.Store(0)
	unsettledReservations.Store(0)
	eventPersistErrors.Store(0)
}
