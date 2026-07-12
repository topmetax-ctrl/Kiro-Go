package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	accountpool "kiro-go/pool"
)

func TestStatsExposesPromptCacheMetricsBackwardCompatible(t *testing.T) {
	mustInitConfig(t)
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 100)
	// Drive some traffic so counters are non-zero.
	prof := profileWithFingerprint("claude-sonnet-4.5", fp("m"), 4000, 5000, time.Hour)
	tr.Compute("acct", prof) // miss
	tr.Update("acct", prof)  // creation
	tr.Compute("acct", prof) // hit

	h := &Handler{promptCache: tr, pool: accountpool.GetPool()}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	h.handleStats(rec, r)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Existing fields still present (backward compatible).
	for _, k := range []string{"status", "totalRequests", "totalTokens", "uptime"} {
		if _, ok := body[k]; !ok {
			t.Errorf("existing stats field %q missing", k)
		}
	}
	pc, ok := body["promptCache"].(map[string]interface{})
	if !ok {
		t.Fatalf("promptCache object missing")
	}
	for _, k := range []string{"hits", "misses", "creations", "evictions", "expiredEvicted", "currentEntries", "capacity"} {
		if _, ok := pc[k]; !ok {
			t.Errorf("promptCache field %q missing", k)
		}
	}
	// No prompt text / fingerprint / token content leaked in the metrics blob.
	raw := rec.Body.String()
	if strings.Contains(raw, "fingerprint") || strings.Contains(strings.ToLower(raw), "prefix") {
		t.Errorf("metrics must not leak prompt/fingerprint content: %s", raw)
	}
}
