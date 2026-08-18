package apikey

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 2

const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS import_state (
  id TEXT PRIMARY KEY,
  done INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  secret_digest TEXT NOT NULL UNIQUE,
  key_prefix TEXT NOT NULL DEFAULT '',
  key_last4 TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  migrated INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  last_used_at INTEGER,
  expires_at INTEGER,
  metadata TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS api_key_quotas (
  key_id TEXT PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
  token_limit INTEGER NOT NULL DEFAULT 0,
  credit_limit REAL NOT NULL DEFAULT 0,
  request_limit INTEGER NOT NULL DEFAULT 0,
  reset_policy TEXT NOT NULL DEFAULT 'lifetime',
  period_start INTEGER NOT NULL DEFAULT 0,
  period_end INTEGER,
  enforcement_mode TEXT NOT NULL DEFAULT 'soft'
);

CREATE TABLE IF NOT EXISTS api_key_usage (
  key_id TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  period TEXT NOT NULL,
  requests_total INTEGER NOT NULL DEFAULT 0,
  requests_success INTEGER NOT NULL DEFAULT 0,
  requests_failed INTEGER NOT NULL DEFAULT 0,
  requests_cancelled INTEGER NOT NULL DEFAULT 0,
  requests_rejected INTEGER NOT NULL DEFAULT 0,
  requests_reserved INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  credits REAL NOT NULL DEFAULT 0,
  last_used_at INTEGER,
  PRIMARY KEY (key_id, period)
);

CREATE TABLE IF NOT EXISTS request_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL DEFAULT '',
  key_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  endpoint TEXT NOT NULL DEFAULT '',
  client_model TEXT NOT NULL DEFAULT '',
  effective_model TEXT NOT NULL DEFAULT '',
  status_code INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  credits REAL NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  ttfb_ms INTEGER NOT NULL DEFAULT 0,
  stream INTEGER NOT NULL DEFAULT 0,
  cancelled INTEGER NOT NULL DEFAULT 0,
  error_code TEXT NOT NULL DEFAULT '',
  sanitized_error TEXT NOT NULL DEFAULT '',
  usage_source TEXT NOT NULL DEFAULT '',
  usage_estimated INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_events_key_ts ON request_events(key_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_events_key_status_ts ON request_events(key_id, status, ts DESC);
CREATE INDEX IF NOT EXISTS idx_events_key_model_ts ON request_events(key_id, effective_model, ts DESC);

CREATE TABLE IF NOT EXISTS usage_hourly (
  key_id TEXT NOT NULL,
  hour_utc INTEGER NOT NULL,
  requests INTEGER NOT NULL DEFAULT 0,
  requests_success INTEGER NOT NULL DEFAULT 0,
  requests_failed INTEGER NOT NULL DEFAULT 0,
  requests_cancelled INTEGER NOT NULL DEFAULT 0,
  requests_rejected INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  credits REAL NOT NULL DEFAULT 0,
  latency_ms_sum INTEGER NOT NULL DEFAULT 0,
  ttfb_ms_sum INTEGER NOT NULL DEFAULT 0,
  ttfb_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (key_id, hour_utc)
);

CREATE TABLE IF NOT EXISTS portal_tokens (
  key_id TEXT PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
  token_digest TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  revoked_at INTEGER
);

CREATE TABLE IF NOT EXISTS portal_sessions (
  id_digest TEXT PRIMARY KEY,
  key_id TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON portal_sessions(expires_at);
`

// Service is the API-key domain store. It is safe for concurrent use.
type Service struct {
	db           *sql.DB
	pepper       []byte
	now          func() time.Time
	retain       time.Duration
	hourlyRetain time.Duration
}

// Open creates (or opens) the SQLite store and applies schema migrations.
func Open(dbPath string, pepper []byte, opts Options) (*Service, error) {
	if len(pepper) == 0 {
		return nil, fmt.Errorf("api key pepper must not be empty")
	}
	if dbPath == "" {
		return nil, fmt.Errorf("api key database path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create api key db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection: SQLite's writer lock plus our reservation transactions
	// stay simple. WAL still lets the same connection serve reads between writes.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pragma wal: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pragma busy_timeout: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pragma foreign_keys: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pragma synchronous: %w", err)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureSchemaVersion(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// In-flight reservations die with the process.
	if _, err := db.Exec(`UPDATE api_key_usage SET requests_reserved = 0`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("clear reservations: %w", err)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	retain := opts.Retention
	if retain <= 0 {
		retain = 30 * 24 * time.Hour
	}
	s := &Service{
		db:           db,
		pepper:       append([]byte(nil), pepper...),
		now:          now,
		retain:       retain,
		hourlyRetain: 365 * 24 * time.Hour,
	}
	return s, nil
}

func ensureSchemaVersion(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return fmt.Errorf("schema version: %w", err)
	}
	if v < 1 {
		if _, err := db.Exec(`INSERT INTO schema_migrations(version) VALUES (1)`); err != nil {
			return fmt.Errorf("record schema version 1: %w", err)
		}
		v = 1
	}
	if v < 2 {
		if err := migrateV2(db); err != nil {
			return fmt.Errorf("migrate v2: %w", err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version) VALUES (2)`); err != nil {
			return fmt.Errorf("record schema version 2: %w", err)
		}
	}
	return nil
}

func migrateV2(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE request_events ADD COLUMN usage_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_events ADD COLUMN usage_estimated INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_hourly ADD COLUMN requests_rejected INTEGER NOT NULL DEFAULT 0`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				return err
			}
		}
	}
	return nil
}

func (s *Service) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Service) HasKeys() bool {
	if s == nil {
		return false
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func unixPtr(sec int64) *time.Time {
	if sec <= 0 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}

func unixOrZero(t *time.Time) interface{} {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

func scanNullInt64(ns sql.NullInt64) *time.Time {
	if !ns.Valid || ns.Int64 <= 0 {
		return nil
	}
	t := time.Unix(ns.Int64, 0).UTC()
	return &t
}
