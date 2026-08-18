package apikey

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ParsePortalRange resolves a named window or a custom UTC unix range.
// Named ranges ignore from/to. CUSTOM requires both, with from < to.
func ParsePortalRange(named, fromS, toS string, now time.Time) (from, to time.Time, err error) {
	now = now.UTC()
	to = now
	from = now.Add(-24 * time.Hour)
	named = strings.ToUpper(strings.TrimSpace(named))
	if named == "" && fromS != "" && toS != "" {
		named = "CUSTOM"
	}
	switch named {
	case "LIVE", "1H":
		from = now.Add(-1 * time.Hour)
	case "6H":
		from = now.Add(-6 * time.Hour)
	case "24H", "":
		from = now.Add(-24 * time.Hour)
	case "7D":
		from = now.Add(-7 * 24 * time.Hour)
	case "30D":
		from = now.Add(-30 * 24 * time.Hour)
	case "CUSTOM":
		fromN, ferr := strconv.ParseInt(strings.TrimSpace(fromS), 10, 64)
		toN, terr := strconv.ParseInt(strings.TrimSpace(toS), 10, 64)
		if ferr != nil || terr != nil || fromN <= 0 || toN <= 0 {
			return time.Time{}, time.Time{}, errInvalidRange("custom range requires from and to as unix seconds")
		}
		from = time.Unix(fromN, 0).UTC()
		to = time.Unix(toN, 0).UTC()
	default:
		return time.Time{}, time.Time{}, errInvalidRange("range must be LIVE, 1H, 6H, 24H, 7D, 30D, or CUSTOM")
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errInvalidRange("from must be before to")
	}
	if to.Sub(from) > MaxPortalRange {
		return time.Time{}, time.Time{}, errRangeTooLarge("maximum supported range is 365 days")
	}
	skew := now.Add(5 * time.Minute)
	if to.After(skew) {
		to = now
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errInvalidRange("from must be before to")
	}
	return from, to, nil
}

// ParseEventQuery builds the unified portal filter from URL values.
// keyId / apiKeyId / key_id are ignored: the principal supplies the key.
func ParseEventQuery(values url.Values, now time.Time) (EventQuery, error) {
	return parseEventQuery(values, now, true)
}

// ParseEventStreamQuery is the same filter model without a default time
// window. Reconnect replay is bounded by Last-Event-ID, not by 24H.
func ParseEventStreamQuery(values url.Values, now time.Time) (EventQuery, error) {
	if strings.TrimSpace(values.Get("range")) == "" &&
		strings.TrimSpace(values.Get("from")) == "" &&
		strings.TrimSpace(values.Get("to")) == "" {
		return parseEventQuery(values, now, false)
	}
	return parseEventQuery(values, now, true)
}

func parseEventQuery(values url.Values, now time.Time, requireRange bool) (EventQuery, error) {
	q := EventQuery{
		Range:     strings.ToUpper(strings.TrimSpace(values.Get("range"))),
		Model:     strings.TrimSpace(values.Get("model")),
		Endpoint:  strings.TrimSpace(values.Get("endpoint")),
		Status:    strings.TrimSpace(values.Get("status")),
		ErrorCode: firstQuery(values, "error_code", "error"),
		Cursor:    strings.TrimSpace(values.Get("cursor")),
		Metric:    strings.TrimSpace(values.Get("metric")),
	}
	if requireRange {
		from, to, err := ParsePortalRange(values.Get("range"), values.Get("from"), values.Get("to"), now)
		if err != nil {
			return EventQuery{}, err
		}
		q.From, q.To = &from, &to
	}
	if v := firstQuery(values, "limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return EventQuery{}, errInvalidFilter("limit must be a non-negative integer")
		}
		q.Limit = n
	}
	if v := firstQuery(values, "streaming", "stream"); v != "" {
		switch v {
		case "1", "true", "yes":
			t := true
			q.Stream = &t
		case "0", "false", "no":
			f := false
			q.Stream = &f
		default:
			return EventQuery{}, errInvalidFilter("streaming must be true or false")
		}
	}
	if err := validateEventQuery(q); err != nil {
		return EventQuery{}, err
	}
	return q, nil
}

func firstQuery(values url.Values, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(values.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

func validateEventQuery(q EventQuery) error {
	if q.Status != "" {
		switch q.Status {
		case OutcomeSuccess, OutcomeFailed, OutcomeCancelled, OutcomeRejected:
		default:
			return errInvalidFilter("status must be success, failed, cancelled, or rejected")
		}
	}
	if q.ErrorCode != "" && !KnownErrorCode(q.ErrorCode) {
		return errInvalidFilter("unknown error_code")
	}
	if q.Metric != "" && !knownSeriesMetric(q.Metric) {
		return errInvalidMetric("metric is not in the allowlist")
	}
	if q.Cursor != "" {
		if _, _, err := DecodeCursor(q.Cursor); err != nil {
			return err
		}
	}
	return nil
}

func knownSeriesMetric(m string) bool {
	for _, a := range AllowedSeriesMetrics {
		if a == m {
			return true
		}
	}
	return false
}

func (q EventQuery) hasDimensionFilter() bool {
	return q.Model != "" || q.Endpoint != "" || q.Status != "" || q.Stream != nil || q.ErrorCode != ""
}

// Match reports whether a persisted event satisfies the unified filter.
func (q EventQuery) Match(ev PublicEvent) bool {
	if q.Model != "" && ev.ClientModel != q.Model && ev.EffectiveModel != q.Model && ev.Model != q.Model {
		return false
	}
	if q.Endpoint != "" && ev.Endpoint != q.Endpoint {
		return false
	}
	if q.Status != "" && ev.Status != q.Status {
		return false
	}
	if q.Stream != nil && ev.Streaming != *q.Stream {
		return false
	}
	if q.ErrorCode != "" && ev.ErrorCode != q.ErrorCode {
		return false
	}
	return true
}

func (s *Service) retentionMeta() RetentionMeta {
	return RetentionMeta{
		RawEventsDays: int(s.retain / (24 * time.Hour)),
		HourlyDays:    int(s.hourlyRetain / (24 * time.Hour)),
	}
}

func (s *Service) rawAvailableFrom(now time.Time) time.Time {
	return now.UTC().Add(-s.retain)
}

func (s *Service) hourlyAvailableFrom(now time.Time) time.Time {
	return now.UTC().Add(-s.hourlyRetain)
}

func (s *Service) queryMeta(from, to time.Time, resolution string, bucket time.Duration, source string, truncated bool) QueryMeta {
	now := s.now().UTC()
	var bucketSec int64
	if bucket > 0 {
		bucketSec = int64(bucket / time.Second)
	}
	return QueryMeta{
		From:             from.Unix(),
		To:               to.Unix(),
		Resolution:       resolution,
		BucketSeconds:    bucketSec,
		Source:           source,
		Truncated:        truncated,
		RawAvailableFrom: s.rawAvailableFrom(now).Unix(),
		DataRetention:    s.retentionMeta(),
	}
}

func (s *Service) clipHistoryRange(from, to time.Time) (time.Time, time.Time, bool) {
	rawFrom := s.rawAvailableFrom(s.now())
	truncated := false
	if from.Before(rawFrom) {
		from = rawFrom
		truncated = true
	}
	if !from.Before(to) {
		from = to.Add(-time.Second)
		truncated = true
	}
	return from, to, truncated
}

func planSeries(span time.Duration, filtered, fromInRaw bool) (resolution string, bucket time.Duration, source string) {
	if filtered {
		switch {
		case span <= 6*time.Hour:
			return Resolution1m, time.Minute, SourceEvents
		case span <= 24*time.Hour:
			return Resolution5m, 5 * time.Minute, SourceEvents
		case span <= 7*24*time.Hour:
			return Resolution15m, 15 * time.Minute, SourceEvents
		default:
			return Resolution1h, time.Hour, SourceEvents
		}
	}
	switch {
	case span <= 6*time.Hour && fromInRaw:
		return Resolution1m, time.Minute, SourceEvents
	case span <= 24*time.Hour && fromInRaw:
		return Resolution5m, 5 * time.Minute, SourceEvents
	default:
		return Resolution1h, time.Hour, SourceHourly
	}
}

func EncodeCursor(ts, id int64) string {
	raw := fmt.Sprintf("v1:%d:%d", ts, id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func DecodeCursor(s string) (ts, id int64, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, errInvalidCursor("cursor is empty")
	}
	b, decErr := base64.RawURLEncoding.DecodeString(s)
	if decErr != nil {
		return 0, 0, errInvalidCursor("cursor is not a valid token")
	}
	parts := strings.Split(string(b), ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, 0, errInvalidCursor("cursor is not a valid token")
	}
	ts, err1 := strconv.ParseInt(parts[1], 10, 64)
	id, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || id <= 0 {
		return 0, 0, errInvalidCursor("cursor is not a valid token")
	}
	return ts, id, nil
}

func ttfbPointer(ms int64, known bool) *int64 {
	if !known {
		if ms > 0 {
			// Legacy rows stored unknown as 0 and measured values as > 0.
			v := ms
			return &v
		}
		return nil
	}
	v := ms
	return &v
}
