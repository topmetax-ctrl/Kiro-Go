package apikey

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *Service) Create(in CreateInput) (Record, string, error) {
	secret := strings.TrimSpace(in.Key)
	if secret == "" {
		secret = GenerateSecret()
	}
	if secret == "" {
		return Record{}, "", ErrEmptySecret
	}
	now := s.now().UTC()
	id := uuid.NewString()
	prefix, last4 := SplitDisplay(secret)
	digest := Digest(secret, s.pepper)
	policy := normalizePolicy(in.ResetPolicy)
	enforce := normalizeEnforcement(in.EnforcementMode)
	pStart, pEnd := periodBounds(policy, now)
	if policy == ResetLifetime {
		pStart = now
		pEnd = nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Record{}, "", err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`INSERT INTO api_keys(id,name,secret_digest,key_prefix,key_last4,enabled,migrated,created_at,updated_at,expires_at)
		VALUES(?,?,?,?,?,?,0,?,?,?)`,
		id, strings.TrimSpace(in.Name), digest, prefix, last4, boolInt(in.Enabled), now.Unix(), now.Unix(), unixOrZero(in.ExpiresAt))
	if err != nil {
		if isUniqueErr(err) {
			return Record{}, "", ErrDuplicate
		}
		return Record{}, "", err
	}
	_, err = tx.Exec(`INSERT INTO api_key_quotas(key_id,token_limit,credit_limit,request_limit,reset_policy,period_start,period_end,enforcement_mode)
		VALUES(?,?,?,?,?,?,?,?)`,
		id, in.TokenLimit, in.CreditLimit, in.RequestLimit, policy, pStart.Unix(), unixOrZero(pEnd), enforce)
	if err != nil {
		return Record{}, "", err
	}
	if err := ensureUsageTx(tx, id, PeriodLifetime); err != nil {
		return Record{}, "", err
	}
	if policy != ResetLifetime {
		if err := ensureUsageTx(tx, id, periodKey(policy, now)); err != nil {
			return Record{}, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return Record{}, "", err
	}
	rec, err := s.Get(id)
	return rec, secret, err
}

func (s *Service) Get(id string) (Record, error) {
	if id == "" {
		return Record{}, ErrNotFound
	}
	return s.loadRecord(id)
}

func (s *Service) Update(id string, in UpdateInput) (Record, error) {
	cur, err := s.Get(id)
	if err != nil {
		return Record{}, err
	}
	now := s.now().UTC()
	name := cur.Key.Name
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	enabled := cur.Key.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	expires := cur.Key.ExpiresAt
	if in.ClearExpires {
		expires = nil
	} else if in.ExpiresAt != nil {
		expires = in.ExpiresAt
	}
	q := cur.Quota
	if in.TokenLimit != nil {
		q.TokenLimit = *in.TokenLimit
	}
	if in.CreditLimit != nil {
		q.CreditLimit = *in.CreditLimit
	}
	if in.RequestLimit != nil {
		q.RequestLimit = *in.RequestLimit
	}
	if in.ResetPolicy != nil {
		q.ResetPolicy = normalizePolicy(*in.ResetPolicy)
		start, end := periodBounds(q.ResetPolicy, now)
		if q.ResetPolicy == ResetLifetime {
			q.PeriodStart = now
			q.PeriodEnd = nil
		} else {
			q.PeriodStart = start
			q.PeriodEnd = end
		}
	}
	if in.EnforcementMode != nil {
		q.EnforcementMode = normalizeEnforcement(*in.EnforcementMode)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE api_keys SET name=?, enabled=?, updated_at=?, expires_at=? WHERE id=?`,
		name, boolInt(enabled), now.Unix(), unixOrZero(expires), id)
	if err != nil {
		return Record{}, err
	}
	_, err = tx.Exec(`UPDATE api_key_quotas SET token_limit=?, credit_limit=?, request_limit=?, reset_policy=?, period_start=?, period_end=?, enforcement_mode=? WHERE key_id=?`,
		q.TokenLimit, q.CreditLimit, q.RequestLimit, q.ResetPolicy, q.PeriodStart.Unix(), unixOrZero(q.PeriodEnd), q.EnforcementMode, id)
	if err != nil {
		return Record{}, err
	}
	if q.ResetPolicy != ResetLifetime {
		if err := ensureUsageTx(tx, id, periodKey(q.ResetPolicy, now)); err != nil {
			return Record{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return s.Get(id)
}

func (s *Service) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM api_keys WHERE id=?`, id)
	return err
}

func (s *Service) Rotate(id string) (Record, string, error) {
	if _, err := s.Get(id); err != nil {
		return Record{}, "", err
	}
	secret := GenerateSecret()
	if secret == "" {
		return Record{}, "", ErrEmptySecret
	}
	prefix, last4 := SplitDisplay(secret)
	digest := Digest(secret, s.pepper)
	now := s.now().UTC().Unix()
	_, err := s.db.Exec(`UPDATE api_keys SET secret_digest=?, key_prefix=?, key_last4=?, updated_at=? WHERE id=?`,
		digest, prefix, last4, now, id)
	if err != nil {
		if isUniqueErr(err) {
			return Record{}, "", ErrDuplicate
		}
		return Record{}, "", err
	}
	rec, err := s.Get(id)
	return rec, secret, err
}

func (s *Service) ResetUsage(id string) (Record, error) {
	if _, err := s.Get(id); err != nil {
		return Record{}, err
	}
	_, err := s.db.Exec(`UPDATE api_key_usage SET
		requests_total=0, requests_success=0, requests_failed=0, requests_cancelled=0,
		requests_rejected=0, requests_reserved=0, input_tokens=0, output_tokens=0,
		total_tokens=0, credits=0 WHERE key_id=?`, id)
	if err != nil {
		return Record{}, err
	}
	return s.Get(id)
}

func (s *Service) List(q ListQuery) ([]Record, int, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	now := s.now().UTC()
	where, args := listWhere(q, now)
	order := listOrder(q.Sort)

	var total int
	countSQL := `SELECT COUNT(*) FROM api_keys k
		JOIN api_key_quotas q ON q.key_id = k.id
		JOIN api_key_usage u ON u.key_id = k.id AND u.period = CASE
			WHEN q.reset_policy IN ('daily','weekly','monthly') THEN
				CASE q.reset_policy
					WHEN 'daily' THEN strftime('%Y-%m-%d', 'now')
					WHEN 'monthly' THEN strftime('%Y-%m', 'now')
					WHEN 'weekly' THEN printf('%04d-W%02d',
						CAST(strftime('%Y', 'now', 'weekday 0', '-6 days') AS INTEGER),
						CAST(strftime('%W', 'now', 'weekday 0', '-6 days') AS INTEGER) + 1)
				END
			ELSE 'lifetime'
		END
		WHERE ` + where
	// Weekly ISO in SQLite is awkward; filter status/usage in Go when needed
	// after a broader SQL filter. For total correctness we load matching ids
	// with a simpler SQL predicate and apply computed filters in memory when
	// status/usage/weekly period is involved.
	if q.Status != "" || q.Usage != "" || q.Quota != "" {
		return s.listInMemory(q, now)
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys k
		JOIN api_key_quotas q ON q.key_id=k.id
		WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(`SELECT k.id FROM api_keys k
		JOIN api_key_quotas q ON q.key_id=k.id
		LEFT JOIN api_key_usage u ON u.key_id=k.id AND u.period='lifetime'
		WHERE `+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		rec, err := s.Get(id)
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	_ = countSQL
	return out, total, nil
}

func (s *Service) listInMemory(q ListQuery, now time.Time) ([]Record, int, error) {
	rows, err := s.db.Query(`SELECT id FROM api_keys`)
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var all []Record
	for _, id := range ids {
		rec, err := s.Get(id)
		if err != nil {
			continue
		}
		if !matchList(rec, q, now) {
			continue
		}
		all = append(all, rec)
	}
	sortRecords(all, q.Sort)
	total := len(all)
	end := q.Offset + q.Limit
	if q.Offset > total {
		return nil, total, nil
	}
	if end > total {
		end = total
	}
	return all[q.Offset:end], total, nil
}

func matchList(rec Record, q ListQuery, now time.Time) bool {
	if q.Q != "" {
		needle := strings.ToLower(q.Q)
		if !strings.Contains(strings.ToLower(rec.Key.Name), needle) &&
			!strings.Contains(strings.ToLower(rec.Key.ID), needle) &&
			!strings.Contains(strings.ToLower(rec.Key.KeyPrefix), needle) &&
			!strings.Contains(strings.ToLower(rec.Key.KeyLast4), needle) &&
			!strings.Contains(strings.ToLower(rec.Masked()), needle) {
			return false
		}
	}
	if q.Status != "" && rec.Status(now) != q.Status {
		return false
	}
	switch q.Quota {
	case "unlimited":
		if rec.Quota.TokenLimit > 0 || rec.Quota.CreditLimit > 0 || rec.Quota.RequestLimit > 0 {
			return false
		}
	case "tokens":
		if rec.Quota.TokenLimit <= 0 {
			return false
		}
	case "credits":
		if rec.Quota.CreditLimit <= 0 {
			return false
		}
	case "requests":
		if rec.Quota.RequestLimit <= 0 {
			return false
		}
	}
	switch q.Usage {
	case "never":
		if rec.Key.LastUsedAt != nil {
			return false
		}
	case "recent":
		if rec.Key.LastUsedAt == nil || now.Sub(*rec.Key.LastUsedAt) > 24*time.Hour {
			return false
		}
	case "high":
		if !highUsage(rec) {
			return false
		}
	}
	return true
}

func highUsage(rec Record) bool {
	if rec.Quota.TokenLimit > 0 && float64(rec.Usage.TotalTokens) >= 0.8*float64(rec.Quota.TokenLimit) {
		return true
	}
	if rec.Quota.CreditLimit > 0 && rec.Usage.Credits >= 0.8*rec.Quota.CreditLimit {
		return true
	}
	if rec.Quota.RequestLimit > 0 && float64(rec.Usage.RequestsTotal) >= 0.8*float64(rec.Quota.RequestLimit) {
		return true
	}
	return false
}

func listWhere(q ListQuery, now time.Time) (string, []interface{}) {
	var parts []string
	var args []interface{}
	parts = append(parts, "1=1")
	if q.Q != "" {
		like := "%" + q.Q + "%"
		parts = append(parts, `(k.name LIKE ? OR k.id LIKE ? OR k.key_prefix LIKE ? OR k.key_last4 LIKE ?)`)
		args = append(args, like, like, like, like)
	}
	_ = now
	return strings.Join(parts, " AND "), args
}

func listOrder(sort string) string {
	switch sort {
	case "name":
		return "k.name COLLATE NOCASE ASC, k.created_at DESC"
	case "last_used":
		return "k.last_used_at IS NULL, k.last_used_at DESC"
	case "usage":
		return "u.total_tokens DESC, u.credits DESC"
	default:
		return "k.created_at DESC"
	}
}

func sortRecords(all []Record, sort string) {
	// small-N insertion; List is already paginated from full scan only when filtered
	less := func(i, j int) bool {
		a, b := all[i], all[j]
		switch sort {
		case "name":
			if strings.EqualFold(a.Key.Name, b.Key.Name) {
				return a.Key.CreatedAt.After(b.Key.CreatedAt)
			}
			return strings.ToLower(a.Key.Name) < strings.ToLower(b.Key.Name)
		case "last_used":
			if a.Key.LastUsedAt == nil {
				return false
			}
			if b.Key.LastUsedAt == nil {
				return true
			}
			return a.Key.LastUsedAt.After(*b.Key.LastUsedAt)
		case "usage":
			if a.Usage.TotalTokens != b.Usage.TotalTokens {
				return a.Usage.TotalTokens > b.Usage.TotalTokens
			}
			return a.Usage.Credits > b.Usage.Credits
		default:
			return a.Key.CreatedAt.After(b.Key.CreatedAt)
		}
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
}

func (s *Service) loadRecord(id string) (Record, error) {
	var rec Record
	var lastUsed, expires sql.NullInt64
	var enabled, migrated int
	var created, updated int64
	err := s.db.QueryRow(`SELECT id,name,key_prefix,key_last4,enabled,migrated,created_at,updated_at,last_used_at,expires_at
		FROM api_keys WHERE id=?`, id).Scan(
		&rec.Key.ID, &rec.Key.Name, &rec.Key.KeyPrefix, &rec.Key.KeyLast4,
		&enabled, &migrated, &created, &updated, &lastUsed, &expires)
	if err == sql.ErrNoRows {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	rec.Key.Enabled = enabled == 1
	rec.Key.Migrated = migrated == 1
	rec.Key.CreatedAt = time.Unix(created, 0).UTC()
	rec.Key.UpdatedAt = time.Unix(updated, 0).UTC()
	rec.Key.LastUsedAt = scanNullInt64(lastUsed)
	rec.Key.ExpiresAt = scanNullInt64(expires)

	var pStart int64
	var pEnd sql.NullInt64
	err = s.db.QueryRow(`SELECT token_limit,credit_limit,request_limit,reset_policy,period_start,period_end,enforcement_mode
		FROM api_key_quotas WHERE key_id=?`, id).Scan(
		&rec.Quota.TokenLimit, &rec.Quota.CreditLimit, &rec.Quota.RequestLimit,
		&rec.Quota.ResetPolicy, &pStart, &pEnd, &rec.Quota.EnforcementMode)
	if err != nil {
		return Record{}, fmt.Errorf("quota: %w", err)
	}
	rec.Quota.PeriodStart = time.Unix(pStart, 0).UTC()
	rec.Quota.PeriodEnd = scanNullInt64(pEnd)

	period := usagePeriodFor(rec.Quota.ResetPolicy, s.now())
	if err := s.maybeRollPeriod(id, &rec); err != nil {
		return Record{}, err
	}
	period = usagePeriodFor(rec.Quota.ResetPolicy, s.now())
	u, err := s.loadUsage(id, period)
	if err != nil {
		if period != PeriodLifetime {
			u, err = s.loadUsage(id, PeriodLifetime)
		}
		if err != nil {
			return Record{}, err
		}
	}
	rec.Usage = u
	return rec, nil
}

func (s *Service) loadUsage(id, period string) (Usage, error) {
	var u Usage
	var last sql.NullInt64
	err := s.db.QueryRow(`SELECT period,requests_total,requests_success,requests_failed,requests_cancelled,requests_rejected,requests_reserved,
		input_tokens,output_tokens,total_tokens,credits,last_used_at
		FROM api_key_usage WHERE key_id=? AND period=?`, id, period).Scan(
		&u.Period, &u.RequestsTotal, &u.RequestsSuccess, &u.RequestsFailed, &u.RequestsCancelled,
		&u.RequestsRejected, &u.RequestsReserved, &u.InputTokens, &u.OutputTokens, &u.TotalTokens,
		&u.Credits, &last)
	if err == sql.ErrNoRows {
		return Usage{Period: period}, fmt.Errorf("usage %s: %w", period, err)
	}
	if err != nil {
		return Usage{}, err
	}
	u.LastUsedAt = scanNullInt64(last)
	return u, nil
}

func (s *Service) maybeRollPeriod(id string, rec *Record) error {
	if rec.Quota.ResetPolicy == ResetLifetime || rec.Quota.ResetPolicy == "" {
		return nil
	}
	now := s.now().UTC()
	if rec.Quota.PeriodEnd != nil && !now.Before(*rec.Quota.PeriodEnd) {
		start, end := periodBounds(rec.Quota.ResetPolicy, now)
		_, err := s.db.Exec(`UPDATE api_key_quotas SET period_start=?, period_end=? WHERE key_id=?`,
			start.Unix(), unixOrZero(end), id)
		if err != nil {
			return err
		}
		if err := ensureUsage(s.db, id, periodKey(rec.Quota.ResetPolicy, now)); err != nil {
			return err
		}
		rec.Quota.PeriodStart = start
		rec.Quota.PeriodEnd = end
	}
	return nil
}

func ensureUsage(db *sql.DB, id, period string) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO api_key_usage(key_id, period) VALUES(?, ?)`, id, period)
	return err
}

func ensureUsageTx(tx *sql.Tx, id, period string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO api_key_usage(key_id, period) VALUES(?, ?)`, id, period)
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint")
}
