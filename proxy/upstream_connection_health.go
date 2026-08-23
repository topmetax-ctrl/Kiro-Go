package proxy

import (
	"sync"
	"time"
)

// Connection health is a runtime condition, never written back as Enabled=false.
// Policy mirrors provider cooldown: 3 failures / 30s, except a single 401/403
// immediately marks auth_failed and cools that key so we do not hammer it.

const (
	connHealthOK          = "ok"
	connHealthRateLimited = "rate_limited"
	connHealthAuthFailed  = "auth_failed"
	connHealthUnhealthy   = "unhealthy"

	connCooldownThreshold = 3
	connCooldownWindow    = 30 * time.Second
)

type connHealthState struct {
	streak     int
	lastFail   time.Time
	lastStatus int
	authFailed bool
}

type connHealthStore struct {
	mu   sync.Mutex
	byID map[string]*connHealthState
}

var connectionHealth = &connHealthStore{byID: map[string]*connHealthState{}}

func connHealthKey(providerID, connectionID string) string {
	return providerID + "\x00" + connectionID
}

func resetConnectionHealthForTest() {
	connectionHealth.mu.Lock()
	connectionHealth.byID = map[string]*connHealthState{}
	connectionHealth.mu.Unlock()
}

func (s *connHealthStore) getLocked(key string) *connHealthState {
	st := s.byID[key]
	if st == nil {
		st = &connHealthState{}
		s.byID[key] = st
	}
	return st
}

func recordConnectionSuccess(providerID, connectionID string) {
	if connectionID == "" {
		return
	}
	connectionHealth.mu.Lock()
	defer connectionHealth.mu.Unlock()
	st := connectionHealth.getLocked(connHealthKey(providerID, connectionID))
	st.streak = 0
	st.authFailed = false
	st.lastStatus = 200
}

func recordConnectionFailure(providerID, connectionID string, status int, now time.Time) {
	if connectionID == "" {
		return
	}
	connectionHealth.mu.Lock()
	defer connectionHealth.mu.Unlock()
	st := connectionHealth.getLocked(connHealthKey(providerID, connectionID))
	st.lastFail = now
	st.lastStatus = status
	if status == 401 || status == 403 {
		st.authFailed = true
		st.streak = connCooldownThreshold
		return
	}
	st.streak++
}

func connectionInCooldown(providerID, connectionID string, now time.Time) bool {
	if connectionID == "" {
		return false
	}
	connectionHealth.mu.Lock()
	defer connectionHealth.mu.Unlock()
	st := connectionHealth.byID[connHealthKey(providerID, connectionID)]
	if st == nil || st.streak < connCooldownThreshold || st.lastFail.IsZero() {
		return false
	}
	return now.Sub(st.lastFail) < connCooldownWindow
}

func connectionHealthPublic(providerID, connectionID string, now time.Time) (health string, cooldownUntil int64) {
	if connectionID == "" {
		return connHealthOK, 0
	}
	connectionHealth.mu.Lock()
	defer connectionHealth.mu.Unlock()
	st := connectionHealth.byID[connHealthKey(providerID, connectionID)]
	if st == nil {
		return connHealthOK, 0
	}
	inCD := st.streak >= connCooldownThreshold && !st.lastFail.IsZero() && now.Sub(st.lastFail) < connCooldownWindow
	until := int64(0)
	if inCD {
		until = st.lastFail.Add(connCooldownWindow).UnixMilli()
	}
	switch {
	case st.authFailed && inCD:
		return connHealthAuthFailed, until
	case st.lastStatus == 429 && inCD:
		return connHealthRateLimited, until
	case inCD:
		return connHealthUnhealthy, until
	case st.authFailed:
		return connHealthAuthFailed, 0
	default:
		return connHealthOK, 0
	}
}
