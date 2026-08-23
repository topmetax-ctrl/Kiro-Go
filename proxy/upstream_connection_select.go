package proxy

import (
	"sync"
	"sync/atomic"
	"time"

	"kiro-go/config"
)

// rrCursors holds a per-provider atomic counter used only for the first pick
// of a request. Failover then walks the remaining candidates in order.
// Counters are runtime-only and never written to config.
type rrCursors struct {
	mu   sync.Mutex
	byID map[string]*uint64
}

var connectionRR = &rrCursors{byID: map[string]*uint64{}}

func resetConnectionRRForTest() {
	connectionRR.mu.Lock()
	connectionRR.byID = map[string]*uint64{}
	connectionRR.mu.Unlock()
}

func (c *rrCursors) next(providerID string) uint64 {
	c.mu.Lock()
	ctr := c.byID[providerID]
	if ctr == nil {
		var n uint64
		ctr = &n
		c.byID[providerID] = ctr
	}
	c.mu.Unlock()
	return atomic.AddUint64(ctr, 1) - 1
}

func enabledConnections(p config.UpstreamProvider) []config.UpstreamConnection {
	all := config.ResolvedConnections(p)
	out := make([]config.UpstreamConnection, 0, len(all))
	for _, c := range all {
		if c.Enabled {
			out = append(out, c)
		}
	}
	return out
}

// orderProviderConnections returns the connections to try for one provider
// attempt. Disabled keys are omitted. Cooldown keys are demoted (tried last)
// unless every enabled key is cold, in which case they are still tried so a
// route cannot silently become unrouted.
func orderProviderConnections(p config.UpstreamProvider, now time.Time) []config.UpstreamConnection {
	enabled := enabledConnections(p)
	if len(enabled) == 0 {
		return nil
	}
	healthy := make([]config.UpstreamConnection, 0, len(enabled))
	cold := make([]config.UpstreamConnection, 0, len(enabled))
	for _, c := range enabled {
		if connectionInCooldown(p.ID, c.ID, now) {
			cold = append(cold, c)
			continue
		}
		healthy = append(healthy, c)
	}
	pool := healthy
	if len(pool) == 0 {
		pool = enabled
	}
	if config.NormalizeConnectionStrategy(p.ConnectionStrategy) == config.ConnectionStrategyPrimary {
		return pool
	}
	if len(pool) == 1 {
		return pool
	}
	start := int(connectionRR.next(p.ID) % uint64(len(pool)))
	out := make([]config.UpstreamConnection, 0, len(pool))
	out = append(out, pool[start:]...)
	out = append(out, pool[:start]...)
	return out
}

func retryableConnectionStatus(status int) bool {
	return status == 401 || status == 403 || status == 429 || status >= 500
}

func isAuthStatus(status int) bool {
	return status == 401 || status == 403
}
