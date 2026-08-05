package injector

import (
	"database/sql"
	"encoding/json"
	"os"
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

// TestWriteKiroProfile writes profile.json into a temp dir and verifies the arn
// is pinned and the region-derived name is correct.
func TestWriteKiroProfile(t *testing.T) {
	dir := t.TempDir()
	acc := ExportAccount{Credentials: ExportCredentials{
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",
	}}
	path, err := WriteKiroProfile(acc, dir)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatalf("expected a written path")
	}
	if filepath.Base(path) != kiroProfileFileName {
		t.Fatalf("wrote %q, want %s", path, kiroProfileFileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p kiroProfile
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("profile.json not valid JSON: %v", err)
	}
	if p.Arn != acc.Credentials.ProfileArn {
		t.Fatalf("arn = %q, want %q", p.Arn, acc.Credentials.ProfileArn)
	}
	// region is parsed from the arn, so the name must reflect eu-central-1.
	if p.Name != "KiroProfile-eu-central-1" {
		t.Fatalf("name = %q, want KiroProfile-eu-central-1", p.Name)
	}
}

// TestWriteKiroProfile_NoArnSkips verifies an account without a profileArn writes
// nothing (returns "" with no error) — we must not clobber a valid profile.json
// with an empty arn.
func TestWriteKiroProfile_NoArnSkips(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteKiroProfile(ExportAccount{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("expected no write for an account with no profileArn, got %q", path)
	}
	if _, statErr := os.Stat(filepath.Join(dir, kiroProfileFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("profile.json should not exist when arn is empty")
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
