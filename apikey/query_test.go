package apikey

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func frozenService(t *testing.T, now time.Time, retain time.Duration) *Service {
	t.Helper()
	if retain <= 0 {
		retain = 30 * 24 * time.Hour
	}
	s, err := Open(filepath.Join(t.TempDir(), "apikeys.db"), []byte("test-pepper-32-bytes-long!!!!!!"), Options{
		Now:       func() time.Time { return now },
		Retention: retain,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func commitN(t *testing.T, s *Service, keyID string, n int, tweak func(i int, in *CommitInput)) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		in := CommitInput{
			RequestID: fmt.Sprintf("r-%d", i), Outcome: OutcomeSuccess, Endpoint: "openai",
			ClientModel: "claude-sonnet-4.5", EffectiveModel: "claude-sonnet-4.5",
			InputTokens: 1, StatusCode: 200,
		}
		if tweak != nil {
			tweak(i, &in)
		}
		if err := s.Commit(keyID, in); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	page, err := s.ListEvents(keyID, EventQuery{Limit: n + 10})
	if err != nil {
		t.Fatal(err)
	}
	// Newest first.
	for i := len(page.Items) - 1; i >= 0; i-- {
		ids = append(ids, page.Items[i].EventID)
	}
	return ids
}

func TestParsePortalRange(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	from, to, err := ParsePortalRange("24H", "", "", now)
	if err != nil || to.Sub(from) != 24*time.Hour {
		t.Fatalf("24H: %v %s %s", err, from, to)
	}
	_, _, err = ParsePortalRange("CUSTOM", "", "", now)
	if err == nil {
		t.Fatal("custom without bounds must fail")
	}
	_, _, err = ParsePortalRange("CUSTOM", "100", "50", now)
	if err == nil {
		t.Fatal("from>=to must fail")
	}
	old := now.Add(-400 * 24 * time.Hour).Unix()
	_, _, err = ParsePortalRange("CUSTOM", fmt.Sprintf("%d", old), fmt.Sprintf("%d", now.Unix()), now)
	if err == nil {
		t.Fatal("400d must fail")
	}
	from2, to2, err := ParsePortalRange("CUSTOM", fmt.Sprintf("%d", now.Add(-2*time.Hour).Unix()), fmt.Sprintf("%d", now.Unix()), now)
	if err != nil || to2.Sub(from2) != 2*time.Hour {
		t.Fatalf("custom 2h: %v %s %s", err, from2, to2)
	}
}

func TestParseEventQueryIgnoresKeyID(t *testing.T) {
	now := time.Now().UTC()
	q, err := ParseEventQuery(url.Values{
		"keyId":    []string{"other-key"},
		"apiKeyId": []string{"other-key"},
		"range":    []string{"1H"},
		"model":    []string{"claude-sonnet-4.5"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Model != "claude-sonnet-4.5" {
		t.Fatalf("model: %+v", q)
	}
}

func TestParseEventQueryRejectsBadFilters(t *testing.T) {
	now := time.Now().UTC()
	if _, err := ParseEventQuery(url.Values{"status": []string{"nope"}}, now); err == nil {
		t.Fatal("bad status")
	}
	if _, err := ParseEventQuery(url.Values{"error_code": []string{"not_a_code"}}, now); err == nil {
		t.Fatal("bad error")
	}
	if _, err := ParseEventQuery(url.Values{"metric": []string{"drop-table"}}, now); err == nil {
		t.Fatal("bad metric")
	}
	if _, err := ParseEventQuery(url.Values{"cursor": []string{"%%%"}}, now); err == nil {
		t.Fatal("bad cursor")
	}
}

func TestHistoryFiltersAndRetention(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := frozenService(t, now, time.Hour)
	a, _, err := s.Create(CreateInput{Name: "a", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Commit(a.Key.ID, CommitInput{
		RequestID: "ok", Outcome: OutcomeSuccess, Endpoint: "openai",
		ClientModel: "m1", EffectiveModel: "m1", StatusCode: 200, Stream: true,
		TTFBMs: 12, TTFBKnown: true,
	})
	_ = s.Commit(a.Key.ID, CommitInput{
		RequestID: "fail", Outcome: OutcomeFailed, Endpoint: "claude",
		ClientModel: "m2", EffectiveModel: "m2", StatusCode: 500,
		ErrorCode: ErrorProviderError, Stream: false,
	})
	// Older than raw retention.
	_, err = s.db.Exec(`INSERT INTO request_events(request_id,key_id,ts,status,endpoint,client_model,effective_model)
		VALUES('old',?,?, 'success','openai','m1','m1')`, a.Key.ID, now.Add(-2*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}

	st := OutcomeFailed
	page, err := s.ListEvents(a.Key.ID, EventQuery{Status: st, Limit: 20})
	if err != nil || len(page.Items) != 1 || page.Items[0].RequestID != "fail" {
		t.Fatalf("status filter: %+v err=%v", page.Items, err)
	}
	page, err = s.ListEvents(a.Key.ID, EventQuery{Model: "m1", Limit: 20})
	if err != nil || len(page.Items) != 1 || page.Items[0].RequestID != "ok" {
		t.Fatalf("model filter: %+v err=%v", page.Items, err)
	}
	page, err = s.ListEvents(a.Key.ID, EventQuery{Endpoint: "claude", Limit: 20})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("endpoint: %+v err=%v", page.Items, err)
	}
	tru := true
	page, err = s.ListEvents(a.Key.ID, EventQuery{Stream: &tru, Limit: 20})
	if err != nil || len(page.Items) != 1 || page.Items[0].RequestID != "ok" {
		t.Fatalf("stream: %+v err=%v", page.Items, err)
	}
	page, err = s.ListEvents(a.Key.ID, EventQuery{ErrorCode: ErrorProviderError, Limit: 20})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("error: %+v err=%v", page.Items, err)
	}
	from := now.Add(-24 * time.Hour)
	to := now
	page, err = s.ListEvents(a.Key.ID, EventQuery{From: &from, To: &to, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated {
		t.Fatal("expected retention clip")
	}
	for _, ev := range page.Items {
		if ev.RequestID == "old" {
			t.Fatal("history leaked an event outside raw retention")
		}
		if ev.TTFBMs != nil && ev.RequestID == "fail" {
			t.Fatal("unknown TTFB must be null, not 0")
		}
		if ev.RequestID == "ok" && (ev.TTFBMs == nil || *ev.TTFBMs != 12) {
			t.Fatalf("known TTFB: %+v", ev.TTFBMs)
		}
	}
}

func TestCursorPaginationNoDupGapSameTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := frozenService(t, now, 0)
	rec, _, err := s.Create(CreateInput{Name: "p", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := commitN(t, s, rec.Key.ID, 17, nil)
	if len(ids) != 17 {
		t.Fatalf("ids %d", len(ids))
	}
	seen := map[int64]struct{}{}
	var cursor string
	var got []int64
	for {
		page, err := s.ListEvents(rec.Key.ID, EventQuery{Limit: 5, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range page.Items {
			if _, dup := seen[ev.EventID]; dup {
				t.Fatalf("duplicate %d", ev.EventID)
			}
			seen[ev.EventID] = struct{}{}
			got = append(got, ev.EventID)
		}
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Fatal("nextCursor without hasMore")
			}
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != 17 {
		t.Fatalf("paged %d want 17: %v", len(got), got)
	}
}

func TestCursorStableUnderConcurrentInserts(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := frozenService(t, now, 0)
	rec, _, err := s.Create(CreateInput{Name: "c", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = commitN(t, s, rec.Key.ID, 10, nil)
	page1, err := s.ListEvents(rec.Key.ID, EventQuery{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Commit(rec.Key.ID, CommitInput{RequestID: "late", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	page2, err := s.ListEvents(rec.Key.ID, EventQuery{Limit: 20, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]struct{}{}
	for _, ev := range page1.Items {
		seen[ev.EventID] = struct{}{}
	}
	for _, ev := range page2.Items {
		if _, dup := seen[ev.EventID]; dup {
			t.Fatalf("page2 duplicated %d", ev.EventID)
		}
	}
}

func TestIsolationListAndSeries(t *testing.T) {
	s := testService(t)
	a, _, _ := s.Create(CreateInput{Name: "a", Enabled: true})
	b, _, _ := s.Create(CreateInput{Name: "b", Enabled: true})
	_ = s.Commit(a.Key.ID, CommitInput{RequestID: "a1", Outcome: OutcomeSuccess, InputTokens: 3, Endpoint: "openai", StatusCode: 200})
	_ = s.Commit(b.Key.ID, CommitInput{RequestID: "b1", Outcome: OutcomeSuccess, InputTokens: 99, Endpoint: "openai", StatusCode: 200})
	page, err := s.ListEvents(a.Key.ID, EventQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range page.Items {
		if ev.RequestID == "b1" || ev.InputTokens == 99 {
			t.Fatal("leaked B")
		}
	}
	series, err := s.UsageSeries(a.Key.ID, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var tokens int64
	for _, p := range series.Points {
		tokens += p.TotalTokens
	}
	if tokens != 3 {
		t.Fatalf("series tokens %d", tokens)
	}
}

func TestSeriesPlannerAndAllowlist(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := frozenService(t, now, 0)
	rec, _, _ := s.Create(CreateInput{Name: "s", Enabled: true})
	_ = s.Commit(rec.Key.ID, CommitInput{
		Outcome: OutcomeSuccess, Endpoint: "openai", ClientModel: "m", EffectiveModel: "m",
		InputTokens: 2, OutputTokens: 3, StatusCode: 200, LatencyMs: 40, TTFBMs: 10, TTFBKnown: true,
	})
	from := now.Add(-2 * time.Hour)
	to := now
	res, err := s.UsageSeries(rec.Key.ID, EventQuery{From: &from, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolution != Resolution1m || res.Source != SourceEvents {
		t.Fatalf("plan %+v", res.QueryMeta)
	}
	var hit bool
	for _, p := range res.Points {
		if p.TotalTokens == 5 && p.AvgTtfbMs != nil && p.SuccessRate != nil {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("missing populated point: %+v", res.Points)
	}

	from7 := now.Add(-10 * 24 * time.Hour)
	res, err = s.UsageSeries(rec.Key.ID, EventQuery{From: &from7, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceHourly || res.Resolution != Resolution1h {
		t.Fatalf("long unfiltered should be hourly: %+v", res.QueryMeta)
	}

	from90 := now.Add(-90 * 24 * time.Hour)
	res, err = s.UsageSeries(rec.Key.ID, EventQuery{From: &from90, To: &to, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceEvents || !res.Truncated {
		t.Fatalf("filtered long range must stay on raw and clip: %+v", res.QueryMeta)
	}
}

func TestExplainUsesKeyTsIndex(t *testing.T) {
	s := testService(t)
	rec, _, _ := s.Create(CreateInput{Name: "x", Enabled: true})
	_ = s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	plans, err := s.ExplainQueryPlan(
		`SELECT id FROM request_events WHERE key_id=? AND (ts < ? OR (ts = ? AND id < ?)) ORDER BY ts DESC, id DESC LIMIT 6`,
		rec.Key.ID, time.Now().Unix(), time.Now().Unix(), int64(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plans, " | ")
	if !strings.Contains(joined, "idx_events_key_ts") && !strings.Contains(strings.ToLower(joined), "using index") {
		t.Fatalf("expected index use, got %s", joined)
	}
}

func TestUnknownTTFBNotZero(t *testing.T) {
	s := testService(t)
	rec, _, _ := s.Create(CreateInput{Name: "t", Enabled: true})
	_ = s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	page, _ := s.ListEvents(rec.Key.ID, EventQuery{Limit: 1})
	if page.Items[0].TTFBMs != nil {
		t.Fatalf("unknown ttfb should be nil, got %v", *page.Items[0].TTFBMs)
	}
	_ = s.Commit(rec.Key.ID, CommitInput{Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200, TTFBMs: 0, TTFBKnown: true, RequestID: "zero"})
	page, _ = s.ListEvents(rec.Key.ID, EventQuery{Limit: 5})
	var found bool
	for _, ev := range page.Items {
		if ev.RequestID == "zero" {
			found = true
			if ev.TTFBMs == nil || *ev.TTFBMs != 0 {
				t.Fatalf("known 0ms must stay 0: %+v", ev.TTFBMs)
			}
		}
	}
	if !found {
		t.Fatal("missing zero ttfb event")
	}
	sum, err := s.Summary(rec.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.AvgTTFBMs == nil || *sum.AvgTTFBMs != 0 {
		t.Fatalf("summary avg TTFB must use known zeros, got %v", sum.AvgTTFBMs)
	}
}
