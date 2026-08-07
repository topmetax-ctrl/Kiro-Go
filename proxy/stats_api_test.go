package proxy

// Tests for the detailed stats admin endpoints: provider drill-down, hourly
// history, per-provider reset, and event export.

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/metrics"
)

func initStatsConfig(t *testing.T, providers ...config.UpstreamProvider) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateUpstreamConfig(providers, nil); err != nil {
		t.Fatalf("UpdateUpstreamConfig: %v", err)
	}
}

func getJSON(t *testing.T, h *Handler, target string) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	switch {
	case strings.HasPrefix(target, "/admin/api/provider-stats"):
		h.apiGetProviderDetail(rec, r)
	case strings.HasPrefix(target, "/admin/api/forward-history"):
		h.apiGetForwardHistory(rec, r)
	case strings.HasPrefix(target, "/admin/api/forward-stats"):
		h.apiGetForwardStats(rec, r)
	default:
		t.Fatalf("unrouted target %q", target)
	}
	if rec.Code != 200 {
		t.Fatalf("%s -> %d: %s", target, rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", target, err)
	}
	return out
}

func TestForwardStatsListsConfiguredProviderWithoutTraffic(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t, config.UpstreamProvider{
		ID: "up-idle", Name: "idle-provider", BaseURL: "https://idle.example/v1",
		Enabled: true, PriceInPerM: 3, PriceOutPerM: 15,
	})

	out := getJSON(t, &Handler{}, "/admin/api/forward-stats")
	providers, _ := out["providers"].([]interface{})
	var row map[string]interface{}
	for _, p := range providers {
		m := p.(map[string]interface{})
		if m["providerId"] == "up-idle" {
			row = m
		}
	}
	if row == nil {
		t.Fatalf("configured provider missing from stats: %v", providers)
	}
	// An unused provider must still be listed, with -1 (unknown) success rate
	// rather than a misleading 0%.
	if row["successRate"].(float64) != -1 {
		t.Fatalf("successRate = %v, want -1", row["successRate"])
	}
	if row["baseUrl"] != "https://idle.example/v1" || row["configured"] != true {
		t.Fatalf("config context missing: %+v", row)
	}
	if row["priceInPerM"].(float64) != 3 {
		t.Fatalf("price not exposed: %+v", row)
	}
}

func TestForwardStatsPrefersConfiguredNameOverRecordedName(t *testing.T) {
	metrics.Reset()
	metrics.Record(metrics.Event{
		ProviderID: "up-1", ProviderName: "old-name", Ok: true, Status: 200,
	})
	initStatsConfig(t, config.UpstreamProvider{ID: "up-1", Name: "new-name", Enabled: true})

	out := getJSON(t, &Handler{}, "/admin/api/forward-stats")
	for _, p := range out["providers"].([]interface{}) {
		m := p.(map[string]interface{})
		if m["providerId"] == "up-1" && m["providerName"] != "new-name" {
			t.Fatalf("renamed provider still shows %v", m["providerName"])
		}
	}
}

func TestForwardStatsIncludesKiroPool(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	metrics.Record(metrics.Event{
		ProviderID: metrics.KiroPoolID, ProviderName: metrics.KiroPoolName,
		Ok: true, Status: 200, LatencyMs: 10,
	})

	out := getJSON(t, &Handler{}, "/admin/api/forward-stats")
	var found bool
	for _, p := range out["providers"].([]interface{}) {
		m := p.(map[string]interface{})
		if m["providerId"] == metrics.KiroPoolID {
			found = true
			if m["isPool"] != true {
				t.Fatalf("pool row not flagged: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("kiro pool absent from provider list")
	}
}

func TestProviderDetailRequiresID(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/api/provider-stats", nil)
	(&Handler{}).apiGetProviderDetail(rec, r)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestProviderDetailForUnusedProviderIsEmptyNotError(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t, config.UpstreamProvider{ID: "up-new", Name: "brand-new", Enabled: true})

	out := getJSON(t, &Handler{}, "/admin/api/provider-stats?id=up-new")
	if out["name"] != "brand-new" {
		t.Fatalf("name = %v, want brand-new", out["name"])
	}
	d := out["detail"].(map[string]interface{})
	if d["requests"].(float64) != 0 {
		t.Fatalf("requests = %v, want 0", d["requests"])
	}
}

func TestProviderDetailBreakdown(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t, config.UpstreamProvider{ID: "up-1", Name: "p1", Enabled: true})

	metrics.Record(metrics.Event{
		ProviderID: "up-1", ClientModel: "m1", Ok: true, Status: 200,
		LatencyMs: 100, InputTokens: 10, OutputTokens: 5,
	})
	metrics.Record(metrics.Event{
		ProviderID: "up-1", ClientModel: "m2", Ok: false, Status: 429,
		LatencyMs: 50, ErrorMsg: "rate limited",
	})

	out := getJSON(t, &Handler{}, "/admin/api/provider-stats?id=up-1")
	d := out["detail"].(map[string]interface{})
	if len(d["models"].([]interface{})) != 2 {
		t.Fatalf("models = %v", d["models"])
	}
	if len(d["statuses"].([]interface{})) != 2 {
		t.Fatalf("statuses = %v", d["statuses"])
	}
	if len(d["recentErrors"].([]interface{})) != 1 {
		t.Fatalf("recentErrors = %v", d["recentErrors"])
	}
}

func TestForwardHistoryReturnsRequestedHours(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	metrics.Record(metrics.Event{ProviderID: "up-1", Ok: true, Status: 200, LatencyMs: 10})

	out := getJSON(t, &Handler{}, "/admin/api/forward-history?id=up-1&hours=6")
	buckets := out["buckets"].([]interface{})
	if len(buckets) != 6 {
		t.Fatalf("buckets = %d, want 6", len(buckets))
	}
	last := buckets[len(buckets)-1].(map[string]interface{})
	if last["requests"].(float64) != 1 {
		t.Fatalf("current hour = %v, want 1 request", last["requests"])
	}
	if out["unit"] != "hour" {
		t.Fatalf("unit = %v, want hour", out["unit"])
	}
}

// The 1-hour range must switch to per-minute buckets: an hourly series for one
// hour is a single bar, which is not a trend.
func TestForwardHistoryOneHourUsesMinuteBuckets(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	metrics.Record(metrics.Event{ProviderID: "up-1", Ok: true, Status: 200, LatencyMs: 10})

	out := getJSON(t, &Handler{}, "/admin/api/forward-history?id=up-1&hours=1")
	if out["unit"] != "minute" {
		t.Fatalf("unit = %v, want minute", out["unit"])
	}
	buckets := out["buckets"].([]interface{})
	if len(buckets) != 60 {
		t.Fatalf("buckets = %d, want 60 minute buckets", len(buckets))
	}
	last := buckets[len(buckets)-1].(map[string]interface{})
	if last["requests"].(float64) != 1 {
		t.Fatalf("current minute = %v, want 1 request", last["requests"])
	}
}

// An unknown provider must yield an empty series rather than 60 fabricated
// zero-buckets that would render as a real, flat chart.
func TestForwardHistoryOneHourUnknownProviderIsEmpty(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)

	out := getJSON(t, &Handler{}, "/admin/api/forward-history?id=nope&hours=1")
	if bs, ok := out["buckets"].([]interface{}); ok && len(bs) != 0 {
		t.Fatalf("buckets = %d, want 0", len(bs))
	}
}

// The aggregate (empty id) view must also honour minute granularity.
func TestForwardHistoryOneHourAggregate(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	metrics.Record(metrics.Event{ProviderID: "up-1", Ok: true, Status: 200, LatencyMs: 10})
	metrics.Record(metrics.Event{ProviderID: "up-2", Ok: true, Status: 200, LatencyMs: 20})

	out := getJSON(t, &Handler{}, "/admin/api/forward-history?hours=1")
	if out["unit"] != "minute" {
		t.Fatalf("unit = %v, want minute", out["unit"])
	}
	buckets := out["buckets"].([]interface{})
	last := buckets[len(buckets)-1].(map[string]interface{})
	if last["requests"].(float64) != 2 {
		t.Fatalf("aggregate current minute = %v, want 2", last["requests"])
	}
}

func TestResetProviderStatsEndpoint(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	metrics.Record(metrics.Event{ProviderID: "up-1", Ok: true, Status: 200})
	metrics.Record(metrics.Event{ProviderID: "up-2", Ok: true, Status: 200})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/forward-stats/reset-provider",
		strings.NewReader(`{"id":"up-1"}`))
	(&Handler{}).apiResetProviderStats(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	for _, p := range metrics.ProviderStats() {
		if p.ProviderID == "up-1" && p.Requests != 0 {
			t.Fatalf("up-1 not reset: %d requests", p.Requests)
		}
		if p.ProviderID == "up-2" && p.Requests != 1 {
			t.Fatalf("up-2 collaterally reset: %d requests", p.Requests)
		}
	}
}

func TestResetProviderStatsRejectsMissingID(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/forward-stats/reset-provider",
		strings.NewReader(`{}`))
	(&Handler{}).apiResetProviderStats(rec, r)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestExportForwardEventsCSV(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	metrics.Record(metrics.Event{
		ProviderID: "up-1", ProviderName: "p1", ClientModel: "m1", Endpoint: "claude",
		Ok: true, Status: 200, LatencyMs: 120, TTFBMs: 30,
		InputTokens: 100, OutputTokens: 50, CostUSD: 0.001,
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/api/forward-events/export", nil)
	(&Handler{}).apiExportForwardEvents(rec, r)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "forward-events.csv") {
		t.Fatalf("disposition = %q", cd)
	}

	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want header + 1", len(rows))
	}
	if rows[0][0] != "time" || rows[0][12] != "inputTokens" {
		t.Fatalf("unexpected header: %v", rows[0])
	}
	if rows[1][12] != "100" || rows[1][13] != "50" {
		t.Fatalf("token columns = %v", rows[1])
	}
}

func TestExportForwardEventsJSONRespectsFilters(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t)
	metrics.Record(metrics.Event{ProviderID: "up-1", Ok: true, Status: 200})
	metrics.Record(metrics.Event{ProviderID: "up-2", Ok: false, Status: 500})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		"/admin/api/forward-events/export?format=json&provider=up-2", nil)
	(&Handler{}).apiExportForwardEvents(rec, r)

	var out struct {
		Total    int             `json:"total"`
		Exported int             `json:"exported"`
		Items    []metrics.Event `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 || out.Exported != 1 {
		t.Fatalf("total/exported = %d/%d, want 1/1", out.Total, out.Exported)
	}
	if out.Items[0].ProviderID != "up-2" {
		t.Fatalf("filter ignored: %+v", out.Items)
	}
}

func TestEventFilterFromQueryParsesAllDimensions(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet,
		"/x?provider=p&account=a&status=error&code=429&model=sonnet&since=100&until=200", nil)
	f := eventFilterFromQuery(r.URL.Query(), 5, 25)
	if f.ProviderID != "p" || f.AccountID != "a" || f.Status != "error" {
		t.Fatalf("identity filters = %+v", f)
	}
	if f.StatusCode != 429 || f.Model != "sonnet" {
		t.Fatalf("code/model = %+v", f)
	}
	if f.SinceMs != 100 || f.UntilMs != 200 {
		t.Fatalf("time range = %+v", f)
	}
	if f.Offset != 5 || f.Limit != 25 {
		t.Fatalf("pagination = %+v", f)
	}
}

func TestEventFilterFromQueryIgnoresJunk(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x?code=abc&since=xyz", nil)
	f := eventFilterFromQuery(r.URL.Query(), 0, 50)
	if f.StatusCode != 0 || f.SinceMs != 0 {
		t.Fatalf("junk should parse to zero, got %+v", f)
	}
}

// The Stats tab sends ?hours=N; without server-side scoping the selector looks
// inert because every figure stays all-time.
func TestForwardStatsHoursScopesProviderFigures(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t, config.UpstreamProvider{
		ID: "up-1", Name: "one", BaseURL: "https://one.example/v1", Enabled: true,
	})
	now := time.Now().UnixMilli()
	metrics.Record(metrics.Event{
		TimeMs: now, ProviderID: "up-1", Ok: true, Status: 200, LatencyMs: 10,
	})
	metrics.Record(metrics.Event{
		TimeMs: now - 5*3600000, ProviderID: "up-1", Ok: true, Status: 200, LatencyMs: 10,
	})

	requestsFor := func(target string) float64 {
		out := getJSON(t, &Handler{}, target)
		for _, p := range out["providers"].([]interface{}) {
			m := p.(map[string]interface{})
			if m["providerId"] == "up-1" {
				n, _ := m["requests"].(float64)
				return n
			}
		}
		t.Fatalf("up-1 missing from %s", target)
		return 0
	}

	if got := requestsFor("/admin/api/forward-stats?hours=1"); got != 1 {
		t.Fatalf("hours=1 requests = %v, want 1", got)
	}
	if got := requestsFor("/admin/api/forward-stats?hours=24"); got != 2 {
		t.Fatalf("hours=24 requests = %v, want 2", got)
	}
	// No hours param keeps the existing all-time behaviour.
	if got := requestsFor("/admin/api/forward-stats"); got != 2 {
		t.Fatalf("unscoped requests = %v, want 2", got)
	}
}

// A configured-but-idle provider must stay listed in a scoped response too,
// otherwise the range filter silently hides upstreams from the table.
func TestForwardStatsHoursStillListsIdleProvider(t *testing.T) {
	metrics.Reset()
	initStatsConfig(t, config.UpstreamProvider{
		ID: "up-idle", Name: "idle-provider", BaseURL: "https://idle.example/v1", Enabled: true,
	})
	out := getJSON(t, &Handler{}, "/admin/api/forward-stats?hours=1")
	for _, p := range out["providers"].([]interface{}) {
		if p.(map[string]interface{})["providerId"] == "up-idle" {
			return
		}
	}
	t.Fatalf("idle provider dropped from scoped response: %v", out["providers"])
}
