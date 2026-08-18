package apikey

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Lookup finds a key by plaintext secret without reserving quota.
func (s *Service) Lookup(secret string) (Record, error) {
	if s == nil {
		return Record{}, ErrUnavailable
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return Record{}, errInvalidKey()
	}
	digest := Digest(secret, s.pepper)
	var id string
	err := s.db.QueryRow(`SELECT id FROM api_keys WHERE secret_digest=?`, digest).Scan(&id)
	if err == sql.ErrNoRows {
		return Record{}, errInvalidKey()
	}
	if err != nil {
		return Record{}, err
	}
	return s.Get(id)
}

// Authenticate verifies the secret, key status, and quota. When reserve is
// true (inference paths), a request slot is taken atomically if a request
// limit is configured.
func (s *Service) Authenticate(secret string, reserve bool) (Record, error) {
	if s == nil {
		return Record{}, ErrUnavailable
	}
	if !s.HasKeys() {
		return Record{}, errNoKeysConfigured()
	}
	rec, err := s.Lookup(secret)
	if err != nil {
		return Record{}, err
	}
	now := s.now().UTC()
	if !rec.Key.Enabled {
		return Record{}, errDisabled()
	}
	if rec.Key.ExpiresAt != nil && !rec.Key.ExpiresAt.After(now) {
		return Record{}, errExpired()
	}
	overToken, overCredit, overRequest := rec.OverLimit()
	if overToken {
		_ = s.addRejected(rec.Key.ID)
		return Record{}, errTokenQuota()
	}
	if overCredit {
		_ = s.addRejected(rec.Key.ID)
		return Record{}, errCreditQuota()
	}
	if overRequest {
		_ = s.addRejected(rec.Key.ID)
		return Record{}, errRequestQuota()
	}
	if reserve {
		if err := s.reserve(rec.Key.ID, rec.Quota); err != nil {
			if ae, ok := err.(*AuthError); ok {
				_ = s.addRejected(rec.Key.ID)
				return Record{}, ae
			}
			return Record{}, err
		}
		rec, err = s.Get(rec.Key.ID)
		if err != nil {
			return Record{}, err
		}
	}
	return rec, nil
}

func (s *Service) reserve(id string, q Quota) error {
	now := s.now().UTC()
	period := usagePeriodFor(q.ResetPolicy, now)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureUsageTx(tx, id, period); err != nil {
		return err
	}
	if err := ensureUsageTx(tx, id, PeriodLifetime); err != nil {
		return err
	}
	if q.RequestLimit <= 0 {
		// Still bump reserved on the period row so Commit can decrement
		// uniformly? No — Commit always decrements reserved. If we did not
		// increment, Commit would drive reserved negative. Only reserve
		// when a request limit is set; Commit checks rowsAffected-style
		// via decrementing MAX(0, reserved-1).
		return tx.Commit()
	}
	res, err := tx.Exec(`UPDATE api_key_usage SET requests_reserved = requests_reserved + 1
		WHERE key_id=? AND period=? AND requests_total + requests_reserved < ?`,
		id, period, q.RequestLimit)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errRequestQuota()
	}
	// Mirror reserved on lifetime so Get(lifetime-policy) stays consistent.
	if period != PeriodLifetime {
		_, _ = tx.Exec(`UPDATE api_key_usage SET requests_reserved = requests_reserved + 1
			WHERE key_id=? AND period=?`, id, PeriodLifetime)
	}
	return tx.Commit()
}

func (s *Service) addRejected(id string) error {
	now := s.now().UTC()
	rec, err := s.Get(id)
	if err != nil {
		return err
	}
	period := usagePeriodFor(rec.Quota.ResetPolicy, now)
	_, err = s.db.Exec(`UPDATE api_key_usage SET requests_rejected = requests_rejected + 1 WHERE key_id=? AND period=?`, id, period)
	if period != PeriodLifetime {
		_, _ = s.db.Exec(`UPDATE api_key_usage SET requests_rejected = requests_rejected + 1 WHERE key_id=? AND period=?`, id, PeriodLifetime)
	}
	return err
}

// Commit reconciles a reserved inference (or an unreserved lookup-only path)
// and persists a metadata event plus hourly bucket.
func (s *Service) Commit(keyID string, in CommitInput) error {
	if s == nil || keyID == "" {
		return nil
	}
	rec, err := s.Get(keyID)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	period := usagePeriodFor(rec.Quota.ResetPolicy, now)
	if err := ensureUsage(s.db, keyID, period); err != nil {
		return err
	}

	outcome := in.Outcome
	if outcome == "" {
		outcome = OutcomeSuccess
	}
	if in.RequestID == "" {
		in.RequestID = uuid.NewString()
	}
	// Resource consumption is independent of business outcome. Rejected
	// admissions never executed, so they never charge tokens/credits. Success,
	// failed, and cancelled all persist and increment any known usage — a
	// mid-stream cancel after 8k output tokens must not bypass quota.
	addTokens := int64(0)
	addIn, addOut := int64(0), int64(0)
	addCredits := 0.0
	if outcome != OutcomeRejected {
		if in.InputTokens > 0 {
			addIn = in.InputTokens
		}
		if in.OutputTokens > 0 {
			addOut = in.OutputTokens
		}
		addTokens = addIn + addOut
		if in.Credits > 0 {
			addCredits = in.Credits
		}
	}

	var succ, fail, cancel, rej, totalInc int64
	switch outcome {
	case OutcomeSuccess:
		succ = 1
		totalInc = 1
	case OutcomeFailed:
		fail = 1
		totalInc = 1
	case OutcomeCancelled:
		cancel = 1
		totalInc = 1
	case OutcomeRejected:
		// Release the reservation without consuming request quota. Used for
		// never-executed admissions (no-account 503, 4xx validation).
		rej = 1
	default:
		totalInc = 1
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	bump := func(p string) error {
		_, err := tx.Exec(`UPDATE api_key_usage SET
			requests_reserved = MAX(0, requests_reserved - 1),
			requests_total = requests_total + ?,
			requests_success = requests_success + ?,
			requests_failed = requests_failed + ?,
			requests_cancelled = requests_cancelled + ?,
			requests_rejected = requests_rejected + ?,
			input_tokens = input_tokens + ?,
			output_tokens = output_tokens + ?,
			total_tokens = total_tokens + ?,
			credits = credits + ?,
			last_used_at = ?
			WHERE key_id=? AND period=?`,
			totalInc, succ, fail, cancel, rej, addIn, addOut, addTokens, addCredits, now.Unix(), keyID, p)
		return err
	}
	if err := bump(period); err != nil {
		return err
	}
	if period != PeriodLifetime {
		if err := ensureUsageTx(tx, keyID, PeriodLifetime); err != nil {
			return err
		}
		if err := bump(PeriodLifetime); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE api_keys SET last_used_at=?, updated_at=? WHERE id=?`, now.Unix(), now.Unix(), keyID)
	if err != nil {
		return err
	}

	source := in.UsageSource
	if source == "" {
		if addTokens == 0 && addCredits == 0 {
			source = UsageSourceNone
		} else {
			source = UsageSourceEstimator
		}
	}
	ttfbKnown := 0
	ttfbVal := in.TTFBMs
	if in.TTFBKnown {
		ttfbKnown = 1
	} else if in.TTFBMs > 0 {
		ttfbKnown = 1
	} else {
		ttfbVal = 0
	}
	res, err := tx.Exec(`INSERT INTO request_events(
		request_id,key_id,ts,endpoint,client_model,effective_model,status_code,status,
		input_tokens,output_tokens,total_tokens,credits,latency_ms,ttfb_ms,ttfb_known,stream,cancelled,error_code,sanitized_error,
		usage_source,usage_estimated)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.RequestID, keyID, now.Unix(), in.Endpoint, in.ClientModel, in.EffectiveModel, in.StatusCode, outcome,
		addIn, addOut, addTokens, addCredits, in.LatencyMs, ttfbVal, ttfbKnown, boolInt(in.Stream), boolInt(outcome == OutcomeCancelled),
		in.ErrorCode, sanitizeError(in.SanitizedError), source, boolInt(in.UsageEstimated))
	if err != nil {
		IncEventPersistError()
		return err
	}
	eventID, _ := res.LastInsertId()

	hour := now.Truncate(time.Hour).Unix()
	var hs, hf, hc, hr, hourReq int64
	switch outcome {
	case OutcomeSuccess:
		hs = 1
		hourReq = 1
	case OutcomeFailed:
		hf = 1
		hourReq = 1
	case OutcomeCancelled:
		hc = 1
		hourReq = 1
	case OutcomeRejected:
		hr = 1
	default:
		hourReq = 1
	}
	ttfbInc, ttfbN := int64(0), int64(0)
	if ttfbKnown == 1 {
		ttfbInc = ttfbVal
		ttfbN = 1
	}
	_, err = tx.Exec(`INSERT INTO usage_hourly(key_id,hour_utc,requests,requests_success,requests_failed,requests_cancelled,requests_rejected,
		input_tokens,output_tokens,total_tokens,credits,latency_ms_sum,ttfb_ms_sum,ttfb_count)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key_id,hour_utc) DO UPDATE SET
			requests = requests + excluded.requests,
			requests_success = requests_success + excluded.requests_success,
			requests_failed = requests_failed + excluded.requests_failed,
			requests_cancelled = requests_cancelled + excluded.requests_cancelled,
			requests_rejected = requests_rejected + excluded.requests_rejected,
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			total_tokens = total_tokens + excluded.total_tokens,
			credits = credits + excluded.credits,
			latency_ms_sum = latency_ms_sum + excluded.latency_ms_sum,
			ttfb_ms_sum = ttfb_ms_sum + excluded.ttfb_ms_sum,
			ttfb_count = ttfb_count + excluded.ttfb_count`,
		keyID, hour, hourReq, hs, hf, hc, hr, addIn, addOut, addTokens, addCredits, in.LatencyMs, ttfbInc, ttfbN)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		IncEventPersistError()
		return err
	}
	IncSettlement(outcome)
	if eventID > 0 {
		if ev, loadErr := s.eventByID(eventID); loadErr == nil {
			s.publishPortal(ev)
		}
	}
	return nil
}
