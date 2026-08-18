package apikey

import (
	"strings"
	"time"
)

func sanitizeError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	// Drop anything that looks like a secret or credential blob.
	if strings.Contains(lower, "authorization") || strings.Contains(lower, "api-key") ||
		strings.Contains(lower, "api_key") || strings.Contains(lower, "sk-") ||
		strings.Contains(lower, "bearer ") {
		return "upstream error"
	}
	if len(s) > 240 {
		s = s[:240]
	}
	return s
}

func (s *Service) ListEvents(keyID string, q EventQuery) ([]PublicEvent, int, error) {
	if keyID == "" {
		return nil, 0, ErrNotFound
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	var parts []string
	var args []interface{}
	parts = append(parts, "key_id=?")
	args = append(args, keyID)
	if q.From != nil {
		parts = append(parts, "ts>=?")
		args = append(args, q.From.Unix())
	}
	if q.To != nil {
		parts = append(parts, "ts<=?")
		args = append(args, q.To.Unix())
	}
	if q.Model != "" {
		parts = append(parts, "(client_model LIKE ? OR effective_model LIKE ?)")
		like := "%" + q.Model + "%"
		args = append(args, like, like)
	}
	if q.Endpoint != "" {
		parts = append(parts, "endpoint=?")
		args = append(args, q.Endpoint)
	}
	if q.Status != "" {
		parts = append(parts, "status=?")
		args = append(args, q.Status)
	}
	if q.Stream != nil {
		parts = append(parts, "stream=?")
		args = append(args, boolInt(*q.Stream))
	}
	if q.ErrorCode != "" {
		parts = append(parts, "error_code=?")
		args = append(args, q.ErrorCode)
	}
	where := strings.Join(parts, " AND ")
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_events WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "ts DESC"
	if q.SortAsc {
		order = "ts ASC"
	}
	rows, err := s.db.Query(`SELECT id,request_id,ts,endpoint,client_model,effective_model,status_code,status,
		input_tokens,output_tokens,total_tokens,credits,latency_ms,ttfb_ms,stream,error_code,usage_source,usage_estimated
		FROM request_events WHERE `+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []PublicEvent
	for rows.Next() {
		var ev PublicEvent
		var ts int64
		var stream, estimated int
		if err := rows.Scan(&ev.EventID, &ev.RequestID, &ts, &ev.Endpoint, &ev.ClientModel, &ev.EffectiveModel, &ev.StatusCode, &ev.Status,
			&ev.InputTokens, &ev.OutputTokens, &ev.TotalTokens, &ev.Credits, &ev.LatencyMs, &ev.TTFBMs, &stream, &ev.ErrorCode,
			&ev.UsageSource, &estimated); err != nil {
			return nil, 0, err
		}
		ev.Timestamp = time.Unix(ts, 0).UTC()
		ev.Streaming = stream == 1
		ev.UsageEstimated = estimated == 1
		ev.ApiKeyID = keyID
		ev.Model = ev.EffectiveModel
		if ev.Model == "" {
			ev.Model = ev.ClientModel
		}
		out = append(out, ev)
	}
	return out, total, nil
}

func (s *Service) UsageSeries(keyID string, from, to time.Time) ([]HourBucket, error) {
	if keyID == "" {
		return nil, ErrNotFound
	}
	if to.IsZero() {
		to = s.now().UTC()
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}
	rows, err := s.db.Query(`SELECT hour_utc,requests,requests_success,requests_failed,requests_cancelled,
		input_tokens,output_tokens,total_tokens,credits,latency_ms_sum,ttfb_ms_sum,ttfb_count
		FROM usage_hourly WHERE key_id=? AND hour_utc>=? AND hour_utc<=? ORDER BY hour_utc ASC`,
		keyID, from.Truncate(time.Hour).Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourBucket
	for rows.Next() {
		var b HourBucket
		var hour, latSum, ttfbSum, ttfbN int64
		if err := rows.Scan(&hour, &b.Requests, &b.RequestsSuccess, &b.RequestsFailed, &b.RequestsCancelled,
			&b.InputTokens, &b.OutputTokens, &b.TotalTokens, &b.Credits, &latSum, &ttfbSum, &ttfbN); err != nil {
			return nil, err
		}
		b.Hour = time.Unix(hour, 0).UTC()
		if b.Requests > 0 {
			b.LatencyMsAvg = float64(latSum) / float64(b.Requests)
		}
		if ttfbN > 0 {
			b.TTFBMsAvg = float64(ttfbSum) / float64(ttfbN)
		}
		out = append(out, b)
	}
	return out, nil
}

func (s *Service) Summary(keyID string) (Summary, error) {
	rec, err := s.Get(keyID)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{Record: rec, Status: rec.Status(s.now()), NextReset: rec.Quota.PeriodEnd}
	if rec.Usage.RequestsTotal > 0 {
		sum.SuccessRate = float64(rec.Usage.RequestsSuccess) / float64(rec.Usage.RequestsTotal)
	}
	var latSum, n int64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(latency_ms),0), COUNT(*) FROM request_events WHERE key_id=? AND latency_ms>0`, keyID).Scan(&latSum, &n)
	if n > 0 {
		sum.AvgLatencyMs = float64(latSum) / float64(n)
	}
	var tok int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM portal_tokens WHERE key_id=? AND revoked_at IS NULL`, keyID).Scan(&tok)
	sum.HasPortalToken = tok > 0
	return sum, nil
}

// Cleanup deletes expired raw events and old hourly buckets in small batches.
func (s *Service) Cleanup() (events int, hours int, err error) {
	now := s.now().UTC()
	eventCut := now.Add(-s.retain).Unix()
	hourCut := now.Add(-s.hourlyRetain).Unix()
	for {
		res, e := s.db.Exec(`DELETE FROM request_events WHERE id IN (SELECT id FROM request_events WHERE ts < ? LIMIT 500)`, eventCut)
		if e != nil {
			return events, hours, e
		}
		n, _ := res.RowsAffected()
		events += int(n)
		if n < 500 {
			break
		}
	}
	for {
		res, e := s.db.Exec(`DELETE FROM usage_hourly WHERE rowid IN (SELECT rowid FROM usage_hourly WHERE hour_utc < ? LIMIT 500)`, hourCut)
		if e != nil {
			return events, hours, e
		}
		n, _ := res.RowsAffected()
		hours += int(n)
		if n < 500 {
			break
		}
	}
	_, _ = s.db.Exec(`DELETE FROM portal_sessions WHERE expires_at < ?`, now.Unix())
	return events, hours, nil
}

// ToPublic maps a live metrics-like event into the portal DTO. Internal fields
// must already have been stripped by the caller; this only shapes the payload.
func ToPublic(requestID string, ts time.Time, endpoint, model string, statusCode int, status string, inTok, outTok int64, credits float64, latency, ttfb int64, stream bool, errorCode string) PublicEvent {
	return PublicEvent{
		RequestID:      requestID,
		Timestamp:      ts.UTC(),
		Endpoint:       endpoint,
		ClientModel:    model,
		EffectiveModel: model,
		Model:          model,
		StatusCode:     statusCode,
		Status:         status,
		InputTokens:    inTok,
		OutputTokens:   outTok,
		TotalTokens:    inTok + outTok,
		Credits:        credits,
		LatencyMs:      latency,
		TTFBMs:         ttfb,
		Streaming:      stream,
		ErrorCode:      errorCode,
	}
}
