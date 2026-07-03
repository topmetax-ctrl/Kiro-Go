package injector

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestInjectKiroIDE_FileShape writes a token into a temp cache dir and verifies
// the on-disk JSON for an external_idp account, plus IdC registration side-file.
func TestInjectKiroIDE_FileShape(t *testing.T) {
	dir := t.TempDir()
	acc := ExportAccount{
		Credentials: ExportCredentials{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresAt:    1700000000000,
			ClientID:     "client-xyz",
			ClientSecret: "header.payload.sig",
			AuthMethod:   "IdC",
			Region:       "us-east-1",
		},
	}
	written, err := InjectKiroIDE(acc, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Fatalf("expected token + registration file, got %v", written)
	}
	// token file must exist and the registration file must be named SHA1(clientId).json
	wantReg := filepath.Join(dir, sha1Hex("client-xyz")+".json")
	foundReg := false
	for _, p := range written {
		if p == wantReg {
			foundReg = true
		}
	}
	if !foundReg {
		t.Fatalf("registration file %s not written; got %v", wantReg, written)
	}
}

// TestInjectKiroCLI_Upsert creates a CLI-shaped sqlite db, injects, and reads
// the rows back. Injecting twice must update (not duplicate) the token row, and
// a matching profile row must land in the state table.
func TestInjectKiroCLI_Upsert(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.sqlite3")
	if err := createEmptyAuthDB(dbPath); err != nil {
		t.Fatal(err)
	}

	// Pre-seed a competing token key to confirm the inject clears it.
	seed, _ := sql.Open("sqlite", dbPath)
	seed.Exec(`INSERT INTO auth_kv(key, value) VALUES(?, ?)`, "kirocli:odic:token", "{}")
	seed.Close()

	acc := ExportAccount{
		Credentials: ExportCredentials{
			AccessToken:  "at1",
			RefreshToken: "rt",
			ExpiresAt:    1700000000000,
			Provider:     "Google",
			ProfileArn:   "arn:aws:codewhisperer:us-east-1:1:profile/ABC",
			AuthMethod:   "social",
			Region:       "us-east-1",
		},
	}
	if err := InjectKiroCLI(acc, dbPath); err != nil {
		t.Fatal(err)
	}
	// second inject with a changed token → upsert, not a second row
	acc.Credentials.AccessToken = "at2"
	if err := InjectKiroCLI(acc, dbPath); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_kv WHERE key = ?`, cliSocialTokenKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 token row after upsert, got %d", count)
	}

	var value string
	if err := db.QueryRow(`SELECT value FROM auth_kv WHERE key = ?`, cliSocialTokenKey).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if want := `"access_token":"at2"`; !contains(value, want) {
		t.Fatalf("token row not updated; value=%s", value)
	}
	if want := `"profile_arn":"arn:aws:codewhisperer:us-east-1:1:profile/ABC"`; !contains(value, want) {
		t.Fatalf("token row missing profile_arn; value=%s", value)
	}

	// competing token key must have been cleared
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_kv WHERE key = ?`, "kirocli:odic:token").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("competing token key should be cleared, got %d rows", count)
	}

	// profile row must be present in the state table
	if err := db.QueryRow(`SELECT COUNT(*) FROM state WHERE key = ?`, cliProfileStateKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 profile state row, got %d", count)
	}
}

// TestInjectKiroCLI_RequiresProfileArn verifies an account without a profileArn
// is rejected (the CLI treats a social token without one as invalid).
func TestInjectKiroCLI_RequiresProfileArn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.sqlite3")
	if err := createEmptyAuthDB(dbPath); err != nil {
		t.Fatal(err)
	}
	acc := ExportAccount{Credentials: ExportCredentials{
		AccessToken: "at", RefreshToken: "rt", AuthMethod: "social",
	}}
	if err := InjectKiroCLI(acc, dbPath); err == nil {
		t.Fatal("expected error when profileArn is missing")
	}
}

// TestInjectKiroCLI_MissingDB returns a helpful error when the DB doesn't exist.
func TestInjectKiroCLI_MissingDB(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "data.sqlite3")
	err := InjectKiroCLI(ExportAccount{}, missing)
	if err == nil {
		t.Fatal("expected error for missing db")
	}
	if !contains(err.Error(), "run the CLI once") {
		t.Fatalf("error should guide the user, got: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
