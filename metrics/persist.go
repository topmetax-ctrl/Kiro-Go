package metrics

import (
	"encoding/json"
	"os"
	"time"
)

// persistedState is the subset of metrics written to disk: the aggregate
// counters that should survive a restart, plus the per-provider hourly rollups
// that back the long-term trend charts. The event ring buffer and the
// per-minute time series are intentionally in-memory only — they are realtime
// views that start fresh each run, keeping the on-disk file small.
type persistedState struct {
	Overall    persistedCounter             `json:"overall"`
	ByProvider map[string]persistedProvider `json:"byProvider"`
	ByRoute    map[string]persistedRoute    `json:"byRoute"`
}

type persistedCounter struct {
	Requests       int64   `json:"requests"`
	Success        int64   `json:"success"`
	Failed         int64   `json:"failed"`
	Canceled       int64   `json:"canceled,omitempty"`
	Streamed       int64   `json:"streamed,omitempty"`
	TotalLatencyMs int64   `json:"totalLatencyMs"`
	TotalTTFBMs    int64   `json:"totalTtfbMs,omitempty"`
	TTFBCount      int64   `json:"ttfbCount,omitempty"`
	InputTokens    int64   `json:"inputTokens,omitempty"`
	OutputTokens   int64   `json:"outputTokens,omitempty"`
	CostUSD        float64 `json:"costUsd,omitempty"`
	LastUsed       int64   `json:"lastUsed"`
}

type persistedProvider struct {
	persistedCounter
	Name       string                      `json:"name"`
	ByStatus   map[string]int64            `json:"byStatus,omitempty"`
	ByModel    map[string]persistedCounter `json:"byModel,omitempty"`
	ByAccount  map[string]persistedAccount `json:"byAccount,omitempty"`
	RecentErrs []ErrorSample               `json:"recentErrors,omitempty"`
	MaxStreak  int64                       `json:"maxFailStreak,omitempty"`
	FailStreak int64                       `json:"failStreak,omitempty"`
	LastOk     int64                       `json:"lastOk,omitempty"`
	LastFail   int64                       `json:"lastFail,omitempty"`
	PeakFlight int64                       `json:"peakInFlight,omitempty"`
	Hours      []persistedBucket           `json:"hours,omitempty"`
}

type persistedAccount struct {
	persistedCounter
	Label string `json:"label,omitempty"`
}

type persistedBucket struct {
	Hour         int64 `json:"h"`
	Requests     int64 `json:"r"`
	Success      int64 `json:"s"`
	Failed       int64 `json:"f"`
	InputTokens  int64   `json:"i,omitempty"`
	OutputTokens int64   `json:"o,omitempty"`
	CostUSD      float64 `json:"c,omitempty"`
	SumLatencyMs int64   `json:"l,omitempty"`
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
		Canceled:       c.canceled,
		Streamed:       c.streamed,
		TotalLatencyMs: c.totalLatencyMs,
		TotalTTFBMs:    c.totalTTFBMs,
		TTFBCount:      c.ttfbCount,
		InputTokens:    c.inputTokens,
		OutputTokens:   c.outputTokens,
		CostUSD:        c.costUSD,
		LastUsed:       c.lastUsed,
	}
}

func fromPersistedCounter(p persistedCounter) counter {
	return counter{
		requests:       p.Requests,
		success:        p.Success,
		failed:         p.Failed,
		canceled:       p.Canceled,
		streamed:       p.Streamed,
		totalLatencyMs: p.TotalLatencyMs,
		totalTTFBMs:    p.TotalTTFBMs,
		ttfbCount:      p.TTFBCount,
		inputTokens:    p.InputTokens,
		outputTokens:   p.OutputTokens,
		costUSD:        p.CostUSD,
		lastUsed:       p.LastUsed,
	}
}

// statusKey/parseStatusKey convert the int status map to string keys, because
// JSON object keys must be strings.
func statusKey(code int) string {
	if code < 0 {
		return "0"
	}
	// Status codes are small; avoid strconv for a hot-ish path.
	digits := [4]byte{}
	n, i := code, len(digits)
	if n == 0 {
		return "0"
	}
	for n > 0 && i > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func parseStatusKey(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// Save writes the aggregate counters and hourly history to path atomically
// (temp file + rename). The event log and per-minute series are not persisted.
func Save(path string) error {
	s.mu.Lock()
	cutoffHour := time.Now().UnixMilli()/3600000 - hourWindowHours
	st := persistedState{
		Overall:    toPersistedCounter(s.overall),
		ByProvider: make(map[string]persistedProvider, len(s.byProvider)),
		ByRoute:    make(map[string]persistedRoute, len(s.byRoute)),
	}
	for id, p := range s.byProvider {
		pp := persistedProvider{
			persistedCounter: toPersistedCounter(p.counter),
			Name:             p.name,
			MaxStreak:        p.maxStreak,
			FailStreak:       p.curStreak,
			LastOk:           p.lastOkMs,
			LastFail:         p.lastFailMs,
			PeakFlight:       p.peakFlight,
		}
		if len(p.byStatus) > 0 {
			pp.ByStatus = make(map[string]int64, len(p.byStatus))
			for code, n := range p.byStatus {
				pp.ByStatus[statusKey(code)] = n
			}
		}
		if len(p.byModel) > 0 {
			pp.ByModel = make(map[string]persistedCounter, len(p.byModel))
			for m, c := range p.byModel {
				pp.ByModel[m] = toPersistedCounter(*c)
			}
		}
		if len(p.byAccount) > 0 {
			pp.ByAccount = make(map[string]persistedAccount, len(p.byAccount))
			for aid, a := range p.byAccount {
				pp.ByAccount[aid] = persistedAccount{
					persistedCounter: toPersistedCounter(a.counter),
					Label:            a.label,
				}
			}
		}
		if len(p.recentErrs) > 0 {
			pp.RecentErrs = append([]ErrorSample(nil), p.recentErrs...)
		}
		for h, b := range p.hours {
			if h < cutoffHour {
				continue
			}
			pp.Hours = append(pp.Hours, persistedBucket{
				Hour:         h,
				Requests:     b.Requests,
				Success:      b.Success,
				Failed:       b.Failed,
				InputTokens:  b.InputTokens,
				OutputTokens: b.OutputTokens,
				CostUSD:      b.CostUSD,
				SumLatencyMs: b.SumLatencyMs,
			})
		}
		st.ByProvider[id] = pp
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

	// Compact (not indented): with 30 days of hourly rollups per provider the
	// indented form roughly triples the file for no operator benefit.
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load restores aggregate counters and hourly history from path. A missing file
// is not an error (fresh start). Event log and per-minute series always start
// empty. Files written by older builds simply lack the newer fields and load
// with those dimensions zeroed.
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

	cutoffHour := time.Now().UnixMilli()/3600000 - hourWindowHours

	s.mu.Lock()
	defer s.mu.Unlock()
	s.overall = fromPersistedCounter(st.Overall)
	s.byProvider = make(map[string]*providerAgg, len(st.ByProvider))
	for id, p := range st.ByProvider {
		agg := newProviderAgg()
		agg.counter = fromPersistedCounter(p.persistedCounter)
		agg.name = p.Name
		agg.maxStreak = p.MaxStreak
		agg.curStreak = p.FailStreak
		agg.lastOkMs = p.LastOk
		agg.lastFailMs = p.LastFail
		agg.peakFlight = p.PeakFlight
		for code, n := range p.ByStatus {
			agg.byStatus[parseStatusKey(code)] = n
		}
		for m, c := range p.ByModel {
			mc := fromPersistedCounter(c)
			agg.byModel[m] = &mc
		}
		for aid, a := range p.ByAccount {
			agg.byAccount[aid] = &accountAgg{
				counter: fromPersistedCounter(a.persistedCounter),
				label:   a.Label,
			}
		}
		agg.recentErrs = append([]ErrorSample(nil), p.RecentErrs...)
		for _, b := range p.Hours {
			if b.Hour < cutoffHour {
				continue
			}
			agg.hours[b.Hour] = &Bucket{
				Minute:       b.Hour,
				Requests:     b.Requests,
				Success:      b.Success,
				Failed:       b.Failed,
				InputTokens:  b.InputTokens,
				OutputTokens: b.OutputTokens,
				CostUSD:      b.CostUSD,
				SumLatencyMs: b.SumLatencyMs,
			}
		}
		s.byProvider[id] = agg
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
