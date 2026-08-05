package injector

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

// cliAppDir is the per-app subdirectory under the platform data-local dir where
// the Kiro CLI stores its SQLite database. Verified against an installed
// kiro-cli 2.10.0 on macOS: ~/Library/Application Support/kiro-cli/data.sqlite3.
// Note this is "kiro-cli", NOT "amazon-q" — although the CLI is a fork of the
// Amazon Q Developer CLI (same auth_kv schema), it ships under its own app name.
const cliAppDir = "kiro-cli"

// DefaultCLIDatabasePath returns the Kiro CLI SQLite database path:
// <data_local_dir>/kiro-cli/data.sqlite3, where data_local_dir is
//   - macOS:   ~/Library/Application Support
//   - Linux:   $XDG_DATA_HOME or ~/.local/share
//   - Windows: %LOCALAPPDATA%
func DefaultCLIDatabasePath() (string, error) {
	base, err := dataLocalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, cliAppDir, "data.sqlite3"), nil
}

// dataLocalDir resolves the platform's local-data directory the same way the
// Rust `dirs` crate does (which the CLI relies on).
func dataLocalDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support"), nil
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v, nil
		}
		return filepath.Join(home, "AppData", "Local"), nil
	default: // linux & others
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return v, nil
		}
		return filepath.Join(home, ".local", "share"), nil
	}
}

// InjectKiroCLI writes a social-shaped auth token plus its matching profile row
// into the CLI's SQLite store so the CLI considers the account logged in.
// dbPath empty → default path.
//
// Strategy (verified against kiro-cli 2.10.0): write kirocli:social:token in
// auth_kv, write api.codewhisperer.profile in the state table, and clear the
// competing token keys so the CLI unambiguously uses the injected token. The
// account MUST carry a profileArn — the CLI rejects a social token without one.
//
// The database and its tables must already exist; the CLI creates them via its
// own migrations on first run. If the file is missing we return a clear error
// telling the user to run the CLI once, rather than creating an unmigrated
// database the CLI might reject.
func InjectKiroCLI(a ExportAccount, dbPath string) (err error) {
	if dbPath == "" {
		dbPath, err = DefaultCLIDatabasePath()
		if err != nil {
			return err
		}
	}
	if _, statErr := os.Stat(dbPath); statErr != nil {
		return fmt.Errorf("kiro-cli database not found at %s — run the CLI once (e.g. `kiro-cli login`) so it creates the database, then retry", dbPath)
	}

	tokenValue, err := buildCLISocialToken(a)
	if err != nil {
		return err
	}
	profileValue, err := buildCLIProfile(a)
	if err != nil {
		return err
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open cli db: %w", err)
	}
	defer db.Close()

	if err := ensureCLITables(db); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Clear competing token rows so the CLI uses the social token we inject.
	for _, k := range cliCompetingTokenKeys {
		if _, err := tx.Exec(`DELETE FROM auth_kv WHERE key = ?`, k); err != nil {
			return fmt.Errorf("clear %s: %w", k, err)
		}
	}
	if err := upsertAuthKV(tx, cliSocialTokenKey, tokenValue); err != nil {
		return err
	}
	if err := upsertState(tx, cliProfileStateKey, profileValue); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ensureCLITables verifies the auth_kv and state tables exist. It does NOT
// create the broader schema — only guards against a half-initialized database.
func ensureCLITables(db *sql.DB) error {
	for _, t := range []string{"auth_kv", "state"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&name)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%s table missing — run the CLI once so it migrates the database", t)
		}
		if err != nil {
			return fmt.Errorf("inspect cli db: %w", err)
		}
	}
	return nil
}

func upsertAuthKV(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(
		`INSERT INTO auth_kv(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("upsert %s: %w", key, err)
	}
	return nil
}

func upsertState(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(
		`INSERT INTO state(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("upsert state %s: %w", key, err)
	}
	return nil
}

// createEmptyAuthDB creates a sqlite database with just the auth_kv table, used
// for dry-run injection so we never touch the real CLI database. It mirrors the
// CLI's 005_auth_table migration (key TEXT PRIMARY KEY, value TEXT).
func createEmptyAuthDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("create dry-run db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS auth_kv (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		return fmt.Errorf("create auth_kv: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS state (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		return fmt.Errorf("create state: %w", err)
	}
	return nil
}
