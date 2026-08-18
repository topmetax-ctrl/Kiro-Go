package apikey

import (
	"strings"
	"sync/atomic"
	"time"
)

// Process-local counters for reservation/accounting invariants. These are not
// Prometheus; they integrate with whatever scrapes the process (tests, admin
// diagnostics). Production expectation: UnsettledReservations() == 0.
var (
	authOK                     atomic.Int64
	authFail                   atomic.Int64
	quotaRejectTokens          atomic.Int64
	quotaRejectCredits         atomic.Int64
	quotaRejectRequests        atomic.Int64
	reservationsActive         atomic.Int64
	settlementsSuccess         atomic.Int64
	settlementsFailed          atomic.Int64
	settlementsCancelled       atomic.Int64
	settlementsRejected        atomic.Int64
	unsettledReservations      atomic.Int64
	eventPersistErrors         atomic.Int64
	portalSessionsActive       atomic.Int64
	portalSessionsTotal        atomic.Int64
	portalSSEConnectionsActive atomic.Int64
	portalSSEConnectionsTotal  atomic.Int64
	portalSSEReconnects        atomic.Int64
	portalSSEReplaysTotal      atomic.Int64
	portalSSEReplayedEvents    atomic.Int64
	portalSSESlowClients       atomic.Int64
	portalSSEReplayTruncated   atomic.Int64
	portalQueryTotal           atomic.Int64
	portalQueryErrors          atomic.Int64
	portalQueryDurationMs      atomic.Int64
	portalSessionReauthFail    atomic.Int64
	usageCleanupDurationMs     atomic.Int64
	usageCleanupRows           atomic.Int64
	sqliteBusyErrors           atomic.Int64
	sqliteWriteErrors          atomic.Int64
)

func IncAuth(ok bool) {
	if ok {
		authOK.Add(1)
		return
	}
	authFail.Add(1)
}

func IncQuotaReject(kind string) {
	switch kind {
	case "tokens":
		quotaRejectTokens.Add(1)
	case "credits":
		quotaRejectCredits.Add(1)
	case "requests":
		quotaRejectRequests.Add(1)
	}
}

func IncReservationsActive() { reservationsActive.Add(1) }
func DecReservationsActive() {
	for {
		n := reservationsActive.Load()
		if n <= 0 {
			return
		}
		if reservationsActive.CompareAndSwap(n, n-1) {
			return
		}
	}
}

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
func IncPortalQuery()          { portalQueryTotal.Add(1) }
func NotePortalQueryDuration(d time.Duration) {
	portalQueryDurationMs.Store(d.Milliseconds())
}
func IncPortalSession() {
	portalSessionsActive.Add(1)
	portalSessionsTotal.Add(1)
}
func DecPortalSession(n int64) {
	if n <= 0 {
		return
	}
	for {
		cur := portalSessionsActive.Load()
		next := cur - n
		if next < 0 {
			next = 0
		}
		if portalSessionsActive.CompareAndSwap(cur, next) {
			return
		}
	}
}
func IncPortalSessionReauthFailure() { portalSessionReauthFail.Add(1) }
func IncPortalSSEReconnect()         { portalSSEReconnects.Add(1) }

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
func IncPortalSSESlow()         { portalSSESlowClients.Add(1) }
func IncPortalReplayTruncated() { portalSSEReplayTruncated.Add(1) }
func IncPortalReplay(n int) {
	portalSSEReplaysTotal.Add(1)
	if n > 0 {
		portalSSEReplayedEvents.Add(int64(n))
	}
}

func NoteCleanup(d time.Duration, rows int) {
	usageCleanupDurationMs.Store(d.Milliseconds())
	usageCleanupRows.Add(int64(rows))
}

func ObserveSQL(err error, write bool) {
	if err == nil {
		return
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "busy") || strings.Contains(msg, "locked") {
		sqliteBusyErrors.Add(1)
	}
	if write {
		sqliteWriteErrors.Add(1)
	}
}

func UnsettledReservations() int64      { return unsettledReservations.Load() }
func EventPersistErrors() int64         { return eventPersistErrors.Load() }
func ReservationsActive() int64         { return reservationsActive.Load() }
func PortalSessionsActive() int64       { return portalSessionsActive.Load() }
func PortalSSEConnectionsActive() int64 { return portalSSEConnectionsActive.Load() }
func PortalSSEConnectionsTotal() int64  { return portalSSEConnectionsTotal.Load() }
func PortalSSEReconnects() int64        { return portalSSEReconnects.Load() }
func PortalSSEReplays() int64           { return portalSSEReplaysTotal.Load() }
func PortalSSEReplayedEvents() int64    { return portalSSEReplayedEvents.Load() }
func PortalSSESlowClients() int64       { return portalSSESlowClients.Load() }
func PortalSSEReplayTruncated() int64   { return portalSSEReplayTruncated.Load() }
func PortalQueryTotal() int64           { return portalQueryTotal.Load() }
func PortalQueryErrors() int64          { return portalQueryErrors.Load() }
func PortalQueryDurationMs() int64      { return portalQueryDurationMs.Load() }
func PortalSessionReauthFailures() int64 {
	return portalSessionReauthFail.Load()
}
func UsageCleanupDurationMs() int64 { return usageCleanupDurationMs.Load() }
func UsageCleanupRows() int64       { return usageCleanupRows.Load() }
func SQLiteBusyErrors() int64       { return sqliteBusyErrors.Load() }
func SQLiteWriteErrors() int64      { return sqliteWriteErrors.Load() }

func SettlementTotals() (success, failed, cancelled, rejected int64) {
	return settlementsSuccess.Load(), settlementsFailed.Load(),
		settlementsCancelled.Load(), settlementsRejected.Load()
}

// Snapshot is a low-cardinality dump for tests and diagnostics. No key IDs.
type Snapshot struct {
	AuthOK, AuthFail            int64
	QuotaRejectTokens           int64
	QuotaRejectCredits          int64
	QuotaRejectRequests         int64
	ReservationsActive          int64
	UnsettledReservations       int64
	EventPersistErrors          int64
	PortalSessionsActive        int64
	PortalSSEConnectionsActive  int64
	PortalSSEReconnects         int64
	PortalSSEReplays            int64
	PortalSSEReplayTruncated    int64
	PortalSSESlowClients        int64
	PortalQueryTotal            int64
	PortalQueryErrors           int64
	PortalQueryDurationMs       int64
	PortalSessionReauthFailures int64
	SQLiteBusyErrors            int64
	SQLiteWriteErrors           int64
}

func ObservabilitySnapshot() Snapshot {
	return Snapshot{
		AuthOK:                      authOK.Load(),
		AuthFail:                    authFail.Load(),
		QuotaRejectTokens:           quotaRejectTokens.Load(),
		QuotaRejectCredits:          quotaRejectCredits.Load(),
		QuotaRejectRequests:         quotaRejectRequests.Load(),
		ReservationsActive:          reservationsActive.Load(),
		UnsettledReservations:       unsettledReservations.Load(),
		EventPersistErrors:          eventPersistErrors.Load(),
		PortalSessionsActive:        portalSessionsActive.Load(),
		PortalSSEConnectionsActive:  portalSSEConnectionsActive.Load(),
		PortalSSEReconnects:         portalSSEReconnects.Load(),
		PortalSSEReplays:            portalSSEReplaysTotal.Load(),
		PortalSSEReplayTruncated:    portalSSEReplayTruncated.Load(),
		PortalSSESlowClients:        portalSSESlowClients.Load(),
		PortalQueryTotal:            portalQueryTotal.Load(),
		PortalQueryErrors:           portalQueryErrors.Load(),
		PortalQueryDurationMs:       portalQueryDurationMs.Load(),
		PortalSessionReauthFailures: portalSessionReauthFail.Load(),
		SQLiteBusyErrors:            sqliteBusyErrors.Load(),
		SQLiteWriteErrors:           sqliteWriteErrors.Load(),
	}
}

// ResetObservabilityForTest clears process counters. Tests that assert
// UnsettledReservations()==0 should call this in t.Cleanup if they expect a
// deliberate unsettled event.
func ResetObservabilityForTest() {
	authOK.Store(0)
	authFail.Store(0)
	quotaRejectTokens.Store(0)
	quotaRejectCredits.Store(0)
	quotaRejectRequests.Store(0)
	reservationsActive.Store(0)
	settlementsSuccess.Store(0)
	settlementsFailed.Store(0)
	settlementsCancelled.Store(0)
	settlementsRejected.Store(0)
	unsettledReservations.Store(0)
	eventPersistErrors.Store(0)
	portalSessionsActive.Store(0)
	portalSessionsTotal.Store(0)
	portalSSEConnectionsActive.Store(0)
	portalSSEConnectionsTotal.Store(0)
	portalSSEReconnects.Store(0)
	portalSSEReplaysTotal.Store(0)
	portalSSEReplayedEvents.Store(0)
	portalSSESlowClients.Store(0)
	portalSSEReplayTruncated.Store(0)
	portalQueryTotal.Store(0)
	portalQueryErrors.Store(0)
	portalQueryDurationMs.Store(0)
	portalSessionReauthFail.Store(0)
	usageCleanupDurationMs.Store(0)
	usageCleanupRows.Store(0)
	sqliteBusyErrors.Store(0)
	sqliteWriteErrors.Store(0)
}
