package apikey

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestProviderErrorDetailRoundTripAndCleanup(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "apikeys.db"), []byte("pepper"), Options{
		Retention: time.Hour,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.PutProviderErrorDetail(ProviderErrorDetail{
		RequestID: "req-1", Attempt: 0, ProviderName: "9router",
		UpstreamStatus: 403, UpstreamCode: "ACCOUNT_SUSPENDED",
		Detail: "account abc", PublicCode: ErrorProviderError, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProviderErrorDetails("req-1")
	if err != nil || len(got) != 1 || got[0].UpstreamCode != "ACCOUNT_SUSPENDED" {
		t.Fatalf("got %+v err=%v", got, err)
	}

	if err := s.PutProviderErrorDetail(ProviderErrorDetail{
		RequestID: "old", Detail: "gone", CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProviderErrorDetails("old"); err != ErrNotFound {
		t.Fatalf("expected old detail purged, err=%v", err)
	}
	if _, err := s.GetProviderErrorDetails("req-1"); err != nil {
		t.Fatalf("fresh detail purged: %v", err)
	}
}

// A pool that rejects every key must leave one diagnostic row PER KEY. attempt is
// the route-target index and does not move between keys, so a (request_id, attempt)
// primary key would have each rejection overwrite the last, hiding which
// credential is actually dead behind whichever failed last.
func TestProviderErrorKeepsOneRowPerConnection(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "apikeys.db"), []byte("pepper"), Options{
		Retention: time.Hour,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := ProviderErrorDetail{
		RequestID: "req-pool", Attempt: 0,
		ProviderID: "up-a", ProviderName: "primary",
		UpstreamStatus: 401, Category: "error", PublicCode: ErrorProviderError,
		CreatedAt: now,
	}
	want := map[string]string{"c1": "key one is dead", "c2": "key two is dead", "c3": "key three is dead"}
	for _, c := range []struct{ id, name, msg string }{
		{"c1", "Key 1", "key one is dead"},
		{"c2", "Key 2", "key two is dead"},
		{"c3", "Key 3", "key three is dead"},
	} {
		d := base
		d.ConnectionID, d.ConnectionName, d.UpstreamMessage = c.id, c.name, c.msg
		if err := s.PutProviderErrorDetail(d); err != nil {
			t.Fatalf("put %s: %v", c.id, err)
		}
	}

	got, err := s.GetProviderErrorDetails("req-pool")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want one row per key, got %d", len(got))
	}
	for _, d := range got {
		if d.ConnectionName == "" {
			t.Errorf("connection %s lost its name", d.ConnectionID)
		}
		if d.UpstreamMessage != want[d.ConnectionID] {
			t.Errorf("connection %s message = %q, want %q", d.ConnectionID, d.UpstreamMessage, want[d.ConnectionID])
		}
	}

	// Re-reporting the SAME key upserts rather than appending a duplicate.
	again := base
	again.ConnectionID, again.ConnectionName, again.UpstreamMessage = "c2", "Key 2 renamed", "still dead"
	if err := s.PutProviderErrorDetail(again); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetProviderErrorDetails("req-pool")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("re-report duplicated a row: %d", len(got))
	}
}

// An existing v4 database carries rows under the old (request_id, attempt) key.
// migrateV5 has to rebuild the table to widen that key, so the thing worth proving
// is that the rebuild does not drop the history it inherits.
func TestMigrateV5PreservesExistingProviderErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apikeys.db")
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE provider_error_details (
  request_id TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 0,
  provider_id TEXT NOT NULL DEFAULT '',
  provider_name TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL DEFAULT '',
  endpoint TEXT NOT NULL DEFAULT '',
  client_model TEXT NOT NULL DEFAULT '',
  effective_model TEXT NOT NULL DEFAULT '',
  upstream_status INTEGER NOT NULL DEFAULT 0,
  upstream_code TEXT NOT NULL DEFAULT '',
  upstream_message TEXT NOT NULL DEFAULT '',
  upstream_request_id TEXT NOT NULL DEFAULT '',
  retry_after TEXT NOT NULL DEFAULT '',
  category TEXT NOT NULL DEFAULT '',
  public_code TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  detail_truncated INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (request_id, attempt)
)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO provider_error_details(request_id,attempt,provider_name,upstream_status,upstream_code,created_at)
		VALUES('legacy-req',0,'9router',429,'RATE_LIMIT',?)`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version) VALUES (1),(2),(3),(4)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, []byte("pepper"), Options{Retention: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("open after v4: %v", err)
	}
	defer s.Close()

	got, err := s.GetProviderErrorDetails("legacy-req")
	if err != nil {
		t.Fatalf("legacy row lost: %v", err)
	}
	if len(got) != 1 || got[0].UpstreamCode != "RATE_LIMIT" || got[0].ProviderName != "9router" {
		t.Fatalf("legacy row mangled: %+v", got)
	}
	if got[0].ConnectionID != "" {
		t.Errorf("pre-pool row should have no connection, got %q", got[0].ConnectionID)
	}

	// And the widened key works on the migrated table, not just a fresh one.
	for _, cid := range []string{"c1", "c2"} {
		if err := s.PutProviderErrorDetail(ProviderErrorDetail{
			RequestID: "legacy-req", Attempt: 1, ConnectionID: cid, ConnectionName: cid,
			UpstreamStatus: 401, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.GetProviderErrorDetails("legacy-req")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want legacy row plus two keys, got %d", len(got))
	}
}
