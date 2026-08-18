package apikey

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

const importStateID = "config_api_keys"

// ImportLegacy copies config.json keys into SQLite. It is idempotent: existing
// IDs are left untouched (live usage wins). Secrets are hashed; plaintext is
// not stored.
func (s *Service) ImportLegacy(entries []LegacyKey) (ImportResult, error) {
	var res ImportResult
	now := s.now().UTC()
	for _, e := range entries {
		secret := strings.TrimSpace(e.Key)
		if secret == "" {
			res.Skipped++
			continue
		}
		id := strings.TrimSpace(e.ID)
		if id == "" {
			id = uuid.NewString()
		}
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id=?`, id).Scan(&exists); err != nil {
			return res, err
		}
		if exists > 0 {
			res.AlreadyPresent++
			continue
		}
		// Digest collision against a different id: skip rather than fail the
		// whole import (duplicate secrets were already rejected by config CRUD).
		digest := Digest(secret, s.pepper)
		var digestHit int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE secret_digest=?`, digest).Scan(&digestHit)
		if digestHit > 0 {
			res.AlreadyPresent++
			continue
		}
		prefix, last4 := SplitDisplay(secret)
		created := e.CreatedAt
		if created <= 0 {
			created = now.Unix()
		}
		var lastUsed interface{}
		if e.LastUsedAt > 0 {
			lastUsed = e.LastUsedAt
		}
		tx, err := s.db.Begin()
		if err != nil {
			return res, err
		}
		_, err = tx.Exec(`INSERT INTO api_keys(id,name,secret_digest,key_prefix,key_last4,enabled,migrated,created_at,updated_at,last_used_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, e.Name, digest, prefix, last4, boolInt(e.Enabled), boolInt(e.Migrated), created, now.Unix(), lastUsed)
		if err != nil {
			_ = tx.Rollback()
			return res, err
		}
		_, err = tx.Exec(`INSERT INTO api_key_quotas(key_id,token_limit,credit_limit,request_limit,reset_policy,period_start,enforcement_mode)
			VALUES(?,?,?,?,?,?,?)`,
			id, e.TokenLimit, e.CreditLimit, 0, ResetLifetime, created, EnforceSoft)
		if err != nil {
			_ = tx.Rollback()
			return res, err
		}
		// Historical RequestsCount was success-only.
		_, err = tx.Exec(`INSERT INTO api_key_usage(key_id,period,requests_total,requests_success,total_tokens,credits,last_used_at)
			VALUES(?,?,?,?,?,?,?)`,
			id, PeriodLifetime, e.RequestsCount, e.RequestsCount, e.TokensUsed, e.CreditsUsed, lastUsed)
		if err != nil {
			_ = tx.Rollback()
			return res, err
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}
		res.Imported++
	}
	_, _ = s.db.Exec(`INSERT INTO import_state(id,done,updated_at) VALUES(?,1,?)
		ON CONFLICT(id) DO UPDATE SET done=1, updated_at=excluded.updated_at`, importStateID, time.Now().Unix())
	return res, nil
}
