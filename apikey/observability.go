package apikey

import "sync/atomic"

// Process-local counters for reservation/accounting invariants. These are not
// Prometheus; they integrate with whatever scrapes the process (tests, admin
// diagnostics). Production expectation: UnsettledReservations() == 0.
var (
	settlementsSuccess         atomic.Int64
	settlementsFailed          atomic.Int64
	settlementsCancelled       atomic.Int64
	settlementsRejected        atomic.Int64
	unsettledReservations      atomic.Int64
	eventPersistErrors         atomic.Int64
	portalSSEConnectionsActive atomic.Int64
	portalSSEConnectionsTotal  atomic.Int64
	portalSSEReplaysTotal      atomic.Int64
	portalSSEReplayedEvents    atomic.Int64
	portalSSESlowClients       atomic.Int64
	portalQueryErrors          atomic.Int64
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
func IncPortalQueryError()     { portalQueryErrors.Add(1) }

func IncPortalSSEConn() {
	portalSSEConnectionsActive.Add(1)
	portalSSEConnectionsTotal.Add(1)
}
func DecPortalSSEConn() {
	for {
		n := portalSSEConnectionsActive.Load()
		if n <= 0 {
			return
		}
		if portalSSEConnectionsActive.CompareAndSwap(n, n-1) {
			return
		}
	}
}
func IncPortalSSESlow() { portalSSESlowClients.Add(1) }
func IncPortalReplay(n int) {
	portalSSEReplaysTotal.Add(1)
	if n > 0 {
		portalSSEReplayedEvents.Add(int64(n))
	}
}

func UnsettledReservations() int64      { return unsettledReservations.Load() }
func EventPersistErrors() int64         { return eventPersistErrors.Load() }
func PortalSSEConnectionsActive() int64 { return portalSSEConnectionsActive.Load() }
func PortalSSEConnectionsTotal() int64  { return portalSSEConnectionsTotal.Load() }
func PortalSSEReplays() int64           { return portalSSEReplaysTotal.Load() }
func PortalSSEReplayedEvents() int64    { return portalSSEReplayedEvents.Load() }
func PortalSSESlowClients() int64       { return portalSSESlowClients.Load() }
func PortalQueryErrors() int64          { return portalQueryErrors.Load() }

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
	portalSSEConnectionsActive.Store(0)
	portalSSEConnectionsTotal.Store(0)
	portalSSEReplaysTotal.Store(0)
	portalSSEReplayedEvents.Store(0)
	portalSSESlowClients.Store(0)
	portalQueryErrors.Store(0)
}
