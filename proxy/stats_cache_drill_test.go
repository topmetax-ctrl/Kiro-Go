package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kiro-go/metrics"
)

// A hostile hours value must not overflow the window arithmetic (it once made
// the bucket count negative and panicked); it is clamped to the 30-day cap.
func TestForwardCacheSeriesClampsHours(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	for _, hours := range []string{"2562047788016", "2562047789000", "3000000000000", "9000000000000000000"} {
		out := getJSON(t, &Handler{}, "/admin/api/forward-cache-series?hours="+hours)
		buckets, _ := out["buckets"].([]interface{})
		if len(buckets) == 0 || len(buckets) > 800 {
			t.Fatalf("hours=%s: %d buckets, want a 30-day window", hours, len(buckets))
		}
		if from, to := out["from"].(float64), out["to"].(float64); to-from > 30*24*3600*1000+1 {
			t.Fatalf("hours=%s: window %v ms exceeds the cap", hours, to-from)
		}
	}
}

func TestForwardEventsCacheDrillFilters(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	now := time.Now().UnixMilli()
	metrics.Record(cacheUsageEvent("up-1", now-3000, 1000, 900, 0))
	metrics.Record(cacheUsageEvent("up-2", now-2000, 1000, 0, 500))
	metrics.Record(cacheUsageEvent("up-3", now-1000, 1000, 100, 0))
	metrics.Record(metrics.Event{TimeMs: now, ProviderID: "up-1", Ok: true, Status: 200})
	total := func(query string) int {
		rec := httptest.NewRecorder()
		(&Handler{}).apiGetForwardEvents(rec, httptest.NewRequest(http.MethodGet, "/admin/api/forward-events?"+query, nil))
		var out struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return out.Total
	}
	if got := total("cacheEligible=1"); got != 3 {
		t.Fatalf("cacheEligible=1: %d, want 3", got)
	}
	if got := total("cacheEligible=1&provider=up-1,up-2"); got != 2 {
		t.Fatalf("two providers: %d, want 2", got)
	}
	if got := total("cacheEligible=1&provider=up-2"); got != 1 {
		t.Fatalf("one provider: %d, want 1", got)
	}
	if got := total("provider=up-1,up-2"); got != 3 {
		t.Fatalf("provider list without the cache filter: %d, want 3 (two cache events + the plain one)", got)
	}
}
