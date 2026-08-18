package apikey

import (
	"database/sql"
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

const eventSelectCols = `id,request_id,key_id,ts,endpoint,client_model,effective_model,status_code,status,
		input_tokens,output_tokens,total_tokens,credits,latency_ms,ttfb_ms,ttfb_known,stream,error_code,usage_source,usage_estimated`

func (q EventQuery) where(keyID string) (string, []interface{}) {
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
		parts = append(parts, "(client_model=? OR effective_model=?)")
		args = append(args, q.Model, q.Model)
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
	return strings.Join(parts, " AND "), args
}

func (s *Service) ListEvents(keyID string, q EventQuery) (EventPage, error) {
	if keyID == "" {
		return EventPage{}, ErrNotFound
	}
	if q.Limit <= 0 {
		q.Limit = DefaultEventPage
	}
	if q.Limit > MaxEventPage {
		q.Limit = MaxEventPage
	}

	now := s.now().UTC()
	from, to := now.Add(-24*time.Hour), now
	if q.From != nil {
		from = q.From.UTC()
	}
	if q.To != nil {
		to = q.To.UTC()
	}
	var truncated bool
	from, to, truncated = s.clipHistoryRange(from, to)
	q.From, q.To = &from, &to

	where, args := q.where(keyID)
	if q.Cursor != "" {
		ts, id, err := DecodeCursor(q.Cursor)
		if err != nil {
			return EventPage{}, err
		}
		where += " AND (ts < ? OR (ts = ? AND id < ?))"
		args = append(args, ts, ts, id)
	}

	rows, err := s.db.Query(`SELECT `+eventSelectCols+` FROM request_events WHERE `+where+
		` ORDER BY ts DESC, id DESC LIMIT ?`, append(args, q.Limit+1)...)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	items, err := scanEvents(rows, keyID)
	if err != nil {
		return EventPage{}, err
	}
	page := EventPage{
		QueryMeta: s.queryMeta(from, to, ResolutionRaw, 0, SourceEvents, truncated),
	}
	if len(items) > q.Limit {
		page.HasMore = true
		items = items[:q.Limit]
		last := items[len(items)-1]
		page.NextCursor = EncodeCursor(last.Timestamp.Unix(), last.EventID)
	}
	page.Items = items
	return page, nil
}

func (s *Service) eventByID(id int64) (PublicEvent, error) {
	row := s.db.QueryRow(`SELECT `+eventSelectCols+` FROM request_events WHERE id=?`, id)
	return scanEvent(row)
}

func scanEvents(rows *sql.Rows, keyID string) ([]PublicEvent, error) {
	var out []PublicEvent
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		if keyID != "" {
			ev.ApiKeyID = keyID
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

type eventScanner interface {
	Scan(dest ...interface{}) error
}

func scanEvent(sc eventScanner) (PublicEvent, error) {
	var ev PublicEvent
	var ts, ttfb int64
	var ttfbKnown, stream, estimated int
	var keyID string
	if err := sc.Scan(&ev.EventID, &ev.RequestID, &keyID, &ts, &ev.Endpoint, &ev.ClientModel, &ev.EffectiveModel, &ev.StatusCode, &ev.Status,
		&ev.InputTokens, &ev.OutputTokens, &ev.TotalTokens, &ev.Credits, &ev.LatencyMs, &ttfb, &ttfbKnown, &stream, &ev.ErrorCode,
		&ev.UsageSource, &estimated); err != nil {
		return PublicEvent{}, err
	}
	ev.ApiKeyID = keyID
	ev.Timestamp = time.Unix(ts, 0).UTC()
	ev.Streaming = stream == 1
	ev.UsageEstimated = estimated == 1
	ev.TTFBMs = ttfbPointer(ttfb, ttfbKnown == 1)
	ev.Model = ev.EffectiveModel
	if ev.Model == "" {
		ev.Model = ev.ClientModel
	}
	return ev, nil
}

func (s *Service) UsageSeries(keyID string, q EventQuery) (SeriesResult, error) {
	if keyID == "" {
		return SeriesResult{}, ErrNotFound
	}
	now := s.now().UTC()
	from, to := now.Add(-24*time.Hour), now
	if q.From != nil {
		from = q.From.UTC()
	}
	if q.To != nil {
		to = q.To.UTC()
	}
	if !from.Before(to) {
		return SeriesResult{}, errInvalidRange("from must be before to")
	}

	truncated := false
	filtered := q.hasDimensionFilter()
	rawFrom := s.rawAvailableFrom(now)
	hourFrom := s.hourlyAvailableFrom(now)
	fromInRaw := !from.Before(rawFrom)
	span := to.Sub(from)
	resolution, bucket, source := planSeries(span, filtered, fromInRaw)

	if source == SourceEvents {
		if from.Before(rawFrom) {
			from = rawFrom
			truncated = true
		}
	} else if from.Before(hourFrom) {
		from = hourFrom
		truncated = true
	}
	if !from.Before(to) {
		return SeriesResult{
			Points:    nil,
			Metrics:   AllowedSeriesMetrics,
			QueryMeta: s.queryMeta(from, to, resolution, bucket, source, true),
		}, nil
	}

	var points []SeriesPoint
	var err error
	if source == SourceHourly {
		points, err = s.seriesFromHourly(keyID, from, to)
	} else {
		points, err = s.seriesFromEvents(keyID, q, from, to, bucket)
	}
	if err != nil {
		return SeriesResult{}, err
	}
	points = fillSeriesGaps(points, from, to, bucket)
	return SeriesResult{
		Points:    points,
		Metrics:   AllowedSeriesMetrics,
		QueryMeta: s.queryMeta(from, to, resolution, bucket, source, truncated),
	}, nil
}

func (s *Service) seriesFromHourly(keyID string, from, to time.Time) ([]SeriesPoint, error) {
	rows, err := s.db.Query(`SELECT hour_utc,requests,requests_success,requests_failed,requests_cancelled,requests_rejected,
		input_tokens,output_tokens,total_tokens,credits,latency_ms_sum,ttfb_ms_sum,ttfb_count
		FROM usage_hourly WHERE key_id=? AND hour_utc>=? AND hour_utc<=? ORDER BY hour_utc ASC`,
		keyID, from.Truncate(time.Hour).Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var hour, req, succ, fail, cancel, rej, in, outTok, tot, latSum, ttfbSum, ttfbN int64
		var credits float64
		if err := rows.Scan(&hour, &req, &succ, &fail, &cancel, &rej, &in, &outTok, &tot, &credits, &latSum, &ttfbSum, &ttfbN); err != nil {
			return nil, err
		}
		out = append(out, packPoint(hour, req, succ, fail, cancel, rej, in, outTok, tot, credits, latSum, ttfbSum, ttfbN))
	}
	return out, rows.Err()
}

func (s *Service) seriesFromEvents(keyID string, q EventQuery, from, to time.Time, bucket time.Duration) ([]SeriesPoint, error) {
	sec := int64(bucket / time.Second)
	if sec <= 0 {
		sec = 60
	}
	q.From, q.To = &from, &to
	where, args := q.where(keyID)
	rows, err := s.db.Query(`SELECT (ts / ?) * ? AS bucket,
		COUNT(*),
		SUM(CASE WHEN status IN ('success','failed','cancelled') THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='cancelled' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='rejected' THEN 1 ELSE 0 END),
		SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), SUM(credits),
		SUM(latency_ms),
		SUM(CASE WHEN ttfb_known=1 OR ttfb_ms>0 THEN ttfb_ms ELSE 0 END),
		SUM(CASE WHEN ttfb_known=1 OR ttfb_ms>0 THEN 1 ELSE 0 END)
		FROM request_events WHERE `+where+` GROUP BY bucket ORDER BY bucket ASC`,
		append([]interface{}{sec, sec}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var bucketT, attempted, quota, succ, fail, cancel, rej, in, outTok, tot, latSum, ttfbSum, ttfbN int64
		var credits float64
		if err := rows.Scan(&bucketT, &attempted, &quota, &succ, &fail, &cancel, &rej, &in, &outTok, &tot, &credits, &latSum, &ttfbSum, &ttfbN); err != nil {
			return nil, err
		}
		_ = attempted
		out = append(out, packPoint(bucketT, quota, succ, fail, cancel, rej, in, outTok, tot, credits, latSum, ttfbSum, ttfbN))
	}
	return out, rows.Err()
}

func packPoint(t, quota, succ, fail, cancel, rej, in, out, tot int64, credits float64, latSum, ttfbSum, ttfbN int64) SeriesPoint {
	p := SeriesPoint{
		T:                     t,
		RequestsQuotaConsumed: quota,
		RequestsSuccess:       succ,
		RequestsFailed:        fail,
		RequestsCancelled:     cancel,
		RequestsRejected:      rej,
		RequestsAttempted:     quota + rej,
		InputTokens:           in,
		OutputTokens:          out,
		TotalTokens:           tot,
		Credits:               credits,
	}
	if p.RequestsAttempted > 0 {
		r := float64(succ) / float64(p.RequestsAttempted)
		p.SuccessRate = &r
	}
	if quota > 0 {
		avg := float64(latSum) / float64(quota)
		p.AvgLatencyMs = &avg
	}
	if ttfbN > 0 {
		avg := float64(ttfbSum) / float64(ttfbN)
		p.AvgTtfbMs = &avg
	}
	return p
}

func fillSeriesGaps(points []SeriesPoint, from, to time.Time, bucket time.Duration) []SeriesPoint {
	if bucket <= 0 {
		return points
	}
	start := from.UTC().Truncate(bucket).Unix()
	end := to.UTC().Unix()
	step := int64(bucket / time.Second)
	if step <= 0 {
		return points
	}
	byT := make(map[int64]SeriesPoint, len(points))
	for _, p := range points {
		byT[p.T] = p
	}
	var out []SeriesPoint
	for t := start; t <= end; t += step {
		if p, ok := byT[t]; ok {
			out = append(out, p)
		} else {
			out = append(out, SeriesPoint{T: t})
		}
	}
	return out
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
	var ttfbSum, ttfbN int64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(ttfb_ms),0), COUNT(*) FROM request_events WHERE key_id=? AND ttfb_known=1`, keyID).Scan(&ttfbSum, &ttfbN)
	if ttfbN > 0 {
		avg := float64(ttfbSum) / float64(ttfbN)
		sum.AvgTTFBMs = &avg
	}
	var tok int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM portal_tokens WHERE key_id=? AND revoked_at IS NULL`, keyID).Scan(&tok)
	sum.HasPortalToken = tok > 0
	return sum, nil
}

// Cleanup deletes expired raw events and old hourly buckets in small batches.
func (s *Service) Cleanup() (events int, hours int, err error) {
	start := time.Now()
	defer func() {
		NoteCleanup(time.Since(start), events+hours)
		ObserveSQL(err, err != nil)
	}()
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
	if res, e := s.db.Exec(`DELETE FROM portal_sessions WHERE expires_at < ?`, now.Unix()); e == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			DecPortalSession(n)
		}
	}
	return events, hours, nil
}

// ToPublic maps a live metrics-like event into the portal DTO. Internal fields
// must already have been stripped by the caller; this only shapes the payload.
func ToPublic(requestID string, ts time.Time, endpoint, model string, statusCode int, status string, inTok, outTok int64, credits float64, latency, ttfb int64, ttfbKnown, stream bool, errorCode string) PublicEvent {
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
		TTFBMs:         ttfbPointer(ttfb, ttfbKnown),
		Streaming:      stream,
		ErrorCode:      errorCode,
	}
}

// ExplainQueryPlan is for tests and ops evidence. It does not change data.
func (s *Service) ExplainQueryPlan(query string, args ...interface{}) ([]string, error) {
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sel, ord, from int
		var detail string
		if err := rows.Scan(&sel, &ord, &from, &detail); err != nil {
			return nil, err
		}
		out = append(out, detail)
	}
	return out, rows.Err()
}
