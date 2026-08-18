package apikey

import (
	"database/sql"
	"strings"
	"time"
)

const defaultSessionTTL = 12 * time.Hour

func (s *Service) CreatePortalToken(keyID string, ttl time.Duration) (string, PortalTokenInfo, error) {
	if _, err := s.Get(keyID); err != nil {
		return "", PortalTokenInfo{}, err
	}
	plain := GeneratePortalToken()
	if plain == "" {
		return "", PortalTokenInfo{}, ErrEmptySecret
	}
	prefix, _ := SplitDisplay(plain)
	digest := Digest(plain, s.pepper)
	now := s.now().UTC()
	var exp interface{}
	info := PortalTokenInfo{Prefix: prefix, CreatedAt: now}
	if ttl > 0 {
		e := now.Add(ttl)
		info.ExpiresAt = &e
		exp = e.Unix()
	}
	_, err := s.db.Exec(`INSERT INTO portal_tokens(key_id,token_digest,prefix,created_at,expires_at,revoked_at)
		VALUES(?,?,?,?,?,NULL)
		ON CONFLICT(key_id) DO UPDATE SET token_digest=excluded.token_digest, prefix=excluded.prefix,
			created_at=excluded.created_at, expires_at=excluded.expires_at, revoked_at=NULL`,
		keyID, digest, prefix, now.Unix(), exp)
	if err != nil {
		return "", PortalTokenInfo{}, err
	}
	return plain, info, nil
}

func (s *Service) RevokePortalToken(keyID string) error {
	_, err := s.db.Exec(`UPDATE portal_tokens SET revoked_at=? WHERE key_id=?`, s.now().UTC().Unix(), keyID)
	if err != nil {
		return err
	}
	return s.CloseSessionsForKey(keyID)
}

func (s *Service) CloseSessionsForKey(keyID string) error {
	if keyID == "" {
		return nil
	}
	res, err := s.db.Exec(`DELETE FROM portal_sessions WHERE key_id=?`, keyID)
	if err != nil {
		ObserveSQL(err, true)
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		DecPortalSession(n)
	}
	return nil
}

func (s *Service) HasPortalToken(keyID string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM portal_tokens WHERE key_id=? AND revoked_at IS NULL`, keyID).Scan(&n)
	return n > 0
}

func (s *Service) OpenSessionByKey(secret string) (string, Record, error) {
	rec, err := s.Lookup(secret)
	if err != nil {
		return "", Record{}, err
	}
	if !rec.Key.Enabled {
		return "", Record{}, errDisabled()
	}
	if rec.Key.ExpiresAt != nil && !rec.Key.ExpiresAt.After(s.now().UTC()) {
		return "", Record{}, errExpired()
	}
	sid, err := s.createSession(rec.Key.ID)
	return sid, rec, err
}

func (s *Service) OpenSessionByPortalToken(token string) (string, Record, error) {
	token = strings.TrimSpace(token)
	if token == "" || !strings.HasPrefix(token, "pt-") {
		return "", Record{}, &AuthError{Status: 401, Code: "authentication_error", Machine: "invalid_portal_token", Message: "invalid portal token"}
	}
	digest := Digest(token, s.pepper)
	var keyID string
	var expires, revoked sql.NullInt64
	err := s.db.QueryRow(`SELECT key_id, expires_at, revoked_at FROM portal_tokens WHERE token_digest=?`, digest).Scan(&keyID, &expires, &revoked)
	if err == sql.ErrNoRows {
		return "", Record{}, &AuthError{Status: 401, Code: "authentication_error", Machine: "invalid_portal_token", Message: "invalid portal token"}
	}
	if err != nil {
		return "", Record{}, err
	}
	now := s.now().UTC().Unix()
	if revoked.Valid && revoked.Int64 > 0 {
		return "", Record{}, &AuthError{Status: 401, Code: "authentication_error", Machine: "invalid_portal_token", Message: "invalid portal token"}
	}
	if expires.Valid && expires.Int64 > 0 && expires.Int64 < now {
		return "", Record{}, &AuthError{Status: 401, Code: "authentication_error", Machine: "portal_token_expired", Message: "portal token expired"}
	}
	rec, err := s.Get(keyID)
	if err != nil {
		return "", Record{}, err
	}
	sid, err := s.createSession(keyID)
	return sid, rec, err
}

func (s *Service) createSession(keyID string) (string, error) {
	plain := GenerateSessionID()
	if plain == "" {
		return "", ErrEmptySecret
	}
	now := s.now().UTC()
	exp := now.Add(defaultSessionTTL)
	_, err := s.db.Exec(`INSERT INTO portal_sessions(id_digest,key_id,created_at,expires_at) VALUES(?,?,?,?)`,
		Digest(plain, s.pepper), keyID, now.Unix(), exp.Unix())
	if err != nil {
		ObserveSQL(err, true)
		return "", err
	}
	IncPortalSession()
	return plain, nil
}

func (s *Service) SessionRecord(sessionPlain string) (Record, error) {
	if sessionPlain == "" {
		return Record{}, ErrSession
	}
	digest := Digest(sessionPlain, s.pepper)
	var keyID string
	var exp int64
	err := s.db.QueryRow(`SELECT key_id, expires_at FROM portal_sessions WHERE id_digest=?`, digest).Scan(&keyID, &exp)
	if err == sql.ErrNoRows {
		return Record{}, ErrSession
	}
	if err != nil {
		return Record{}, err
	}
	if exp < s.now().UTC().Unix() {
		return Record{}, ErrSession
	}
	rec, err := s.Get(keyID)
	if err != nil {
		return Record{}, err
	}
	if !rec.Key.Enabled {
		return Record{}, ErrSession
	}
	return rec, nil
}

// SessionExpiry returns when the portal cookie must stop being accepted.
func (s *Service) SessionExpiry(sessionPlain string) (time.Time, error) {
	if sessionPlain == "" {
		return time.Time{}, ErrSession
	}
	var exp int64
	err := s.db.QueryRow(`SELECT expires_at FROM portal_sessions WHERE id_digest=?`, Digest(sessionPlain, s.pepper)).Scan(&exp)
	if err == sql.ErrNoRows {
		return time.Time{}, ErrSession
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(exp, 0).UTC(), nil
}

func (s *Service) CloseSession(sessionPlain string) error {
	if sessionPlain == "" {
		return nil
	}
	res, err := s.db.Exec(`DELETE FROM portal_sessions WHERE id_digest=?`, Digest(sessionPlain, s.pepper))
	if err != nil {
		ObserveSQL(err, true)
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		DecPortalSession(n)
	}
	return nil
}
