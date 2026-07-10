package metrics

import (
	"encoding/json"
	"os"
)

// persistedState is the subset of metrics written to disk: the aggregate
// counters that should survive a restart. The event ring buffer and
// time-series buckets are intentionally in-memory only — they are realtime
// views that start fresh each run, keeping the on-disk file small.
type persistedState struct {
	Overall    persistedCounter            `json:"overall"`
	ByProvider map[string]persistedProvider `json:"byProvider"`
	ByRoute    map[string]persistedRoute    `json:"byRoute"`
}

type persistedCounter struct {
	Requests       int64 `json:"requests"`
	Success        int64 `json:"success"`
	Failed         int64 `json:"failed"`
	TotalLatencyMs int64 `json:"totalLatencyMs"`
	LastUsed       int64 `json:"lastUsed"`
}

type persistedProvider struct {
	persistedCounter
	Name string `json:"name"`
}

type persistedRoute struct {
	persistedCounter
	ClientModel string `json:"clientModel"`
	TargetModel string `json:"targetModel"`
	ProviderID  string `json:"providerId"`
}

func toPersistedCounter(c counter) persistedCounter {
	return persistedCounter{
		Requests:       c.requests,
		Success:        c.success,
		Failed:         c.failed,
		TotalLatencyMs: c.totalLatencyMs,
		LastUsed:       c.lastUsed,
	}
}

func fromPersistedCounter(p persistedCounter) counter {
	return counter{
		requests:       p.Requests,
		success:        p.Success,
		failed:         p.Failed,
		totalLatencyMs: p.TotalLatencyMs,
		lastUsed:       p.LastUsed,
	}
}

// Save writes the aggregate counters to path atomically (temp file + rename).
// The event log and time-series are not persisted.
func Save(path string) error {
	s.mu.Lock()
	st := persistedState{
		Overall:    toPersistedCounter(s.overall),
		ByProvider: make(map[string]persistedProvider, len(s.byProvider)),
		ByRoute:    make(map[string]persistedRoute, len(s.byRoute)),
	}
	for id, p := range s.byProvider {
		st.ByProvider[id] = persistedProvider{
			persistedCounter: toPersistedCounter(p.counter),
			Name:             p.name,
		}
	}
	for id, r := range s.byRoute {
		st.ByRoute[id] = persistedRoute{
			persistedCounter: toPersistedCounter(r.counter),
			ClientModel:      r.clientModel,
			TargetModel:      r.targetModel,
			ProviderID:       r.providerID,
		}
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load restores aggregate counters from path. A missing file is not an error
// (fresh start). Event log and time-series always start empty.
func Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.overall = fromPersistedCounter(st.Overall)
	s.byProvider = make(map[string]*providerAgg, len(st.ByProvider))
	for id, p := range st.ByProvider {
		s.byProvider[id] = &providerAgg{
			counter: fromPersistedCounter(p.persistedCounter),
			name:    p.Name,
		}
	}
	s.byRoute = make(map[string]*routeAgg, len(st.ByRoute))
	for id, r := range st.ByRoute {
		s.byRoute[id] = &routeAgg{
			counter:     fromPersistedCounter(r.persistedCounter),
			clientModel: r.ClientModel,
			targetModel: r.TargetModel,
			providerID:  r.ProviderID,
		}
	}
	return nil
}
