package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// A connection label is rendered in the admin list and printed in [Forward]
// logs. These tests pin that a raw credential can never become that label,
// whichever door it arrives through.

func setupNameCfg(t *testing.T) UpstreamProvider {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	p := UpstreamProvider{ID: "p1", Name: "prov", BaseURL: "https://h/v1", Enabled: true}
	if err := UpdateUpstreamConfig([]UpstreamProvider{p}, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddConnectionRejectsKeyAsName(t *testing.T) {
	setupNameCfg(t)
	const key = "sk-FIXTUREonlyNOTAREALKEY1111222233334444555566667777"
	c, err := AddUpstreamConnection("p1", UpstreamConnection{Name: key, ApiKey: key, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Key 1" {
		t.Fatalf("name = %q, want the Key N fallback", c.Name)
	}
	if strings.Contains(c.Name, key[:12]) {
		t.Fatalf("name still carries the secret: %q", c.Name)
	}
}

func TestAddConnectionRejectsForeignSecretAsName(t *testing.T) {
	setupNameCfg(t)
	// Pasting a different credential as the label is just as leaky.
	other := "sk-proj-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	c, err := AddUpstreamConnection("p1", UpstreamConnection{Name: other, ApiKey: "sk-real-one-1234567890", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Key 1" {
		t.Fatalf("name = %q, want Key 1", c.Name)
	}
}

func TestAddConnectionKeepsHumanNames(t *testing.T) {
	setupNameCfg(t)
	for _, name := range []string{"user2", "alice@example.com", "acct-3 | prod", "sk"} {
		c, err := AddUpstreamConnection("p1", UpstreamConnection{Name: name, ApiKey: "sk-" + name + "-0123456789abcdef", Enabled: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Name != name {
			t.Fatalf("name %q was rewritten to %q", name, c.Name)
		}
	}
}

func TestPatchConnectionRejectsSecretRename(t *testing.T) {
	setupNameCfg(t)
	c, err := AddUpstreamConnection("p1", UpstreamConnection{Name: "user1", ApiKey: "sk-aaaa-1234567890abcdef", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// Rename and rotate together: the label must be checked against the NEW key.
	newKey := "sk-bbbb-cccccccccccccccccccc"
	got, err := UpdateUpstreamConnection("p1", c.ID, UpstreamConnectionPatch{Name: &newKey, ApiKey: &newKey})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "user1" {
		t.Fatalf("name = %q, want the old label kept", got.Name)
	}
	if got.ApiKey != newKey {
		t.Fatalf("rotation was dropped: %q", got.ApiKey)
	}
}

func TestBulkAddRejectsKeyAsName(t *testing.T) {
	setupNameCfg(t)
	k1, k2 := "sk-1111111111111111111111", "sk-2222222222222222222222"
	added, err := AddUpstreamConnections("p1", []UpstreamConnection{
		{Name: k1, ApiKey: k1, Enabled: true},
		{Name: "team-b", ApiKey: k2, Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("added = %d", len(added))
	}
	if added[0].Name != "Key 1" {
		t.Fatalf("secret label survived bulk add: %q", added[0].Name)
	}
	if added[1].Name != "team-b" {
		t.Fatalf("human label rewritten: %q", added[1].Name)
	}
}

func TestLoadRepairsStoredSecretName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	const key = "sk-FIXTUREonlyNOTAREALKEY11112222333344445555"
	// Simulate a config written by a build without the guard.
	if err := UpdateUpstreamConfig([]UpstreamProvider{{
		ID: "p1", Name: "prov", BaseURL: "https://h/v1", Enabled: true,
		Connections: []UpstreamConnection{{ID: "c1", Name: key, ApiKey: key, Enabled: true}},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	providers, _ := GetUpstreamConfig()
	got := providers[0].Connections[0]
	if got.Name == key {
		t.Fatal("stored secret name survived reload")
	}
	if got.Name != "Key 1" {
		t.Fatalf("name = %q, want Key 1", got.Name)
	}
	if got.ApiKey != key {
		t.Fatalf("repair must not touch the secret: %q", got.ApiKey)
	}
}

func TestSafeConnectionLabelMasksLegacyRow(t *testing.T) {
	const key = "sk-FIXTUREonlyNOTAREALKEY1111"
	got := SafeConnectionLabel(UpstreamConnection{Name: key, ApiKey: key})
	if got == key || !strings.Contains(got, "****") {
		t.Fatalf("label = %q, want masked", got)
	}
	if plain := SafeConnectionLabel(UpstreamConnection{Name: "Key 7", ApiKey: key}); plain != "Key 7" {
		t.Fatalf("human label masked: %q", plain)
	}
}

func TestImportNeverNamesRowAfterASecondCredential(t *testing.T) {
	// "key | key" exports and rotated pairs both put a live credential in a
	// non-key column. Naming the row after it would print the secret in the admin
	// list and the [Forward] log, so those rows fall back to Key N.
	cases := []struct{ line, wantName string }{
		{"sk-bulk-gggg7777 | sk-bulk-hhhh8888iiii9999jjjj0000", "Key 1"},
		{"Bearer sk-xyz1111 | sk-real2222333344445555", "Key 1"},
		{"teamB | pw | sk-bulk-dddd4444eeee5555ffff6666", "teamB"},
	}
	for _, tc := range cases {
		for _, naming := range []string{NamingFirstNonKey, NamingJoinNonKey} {
			pv := ParseConnectionImport(tc.line, "", naming, nil, nil, nil)
			if len(pv.Lines) != 1 {
				t.Fatalf("%q: lines = %d", tc.line, len(pv.Lines))
			}
			got := pv.Lines[0].Name
			if naming == NamingFirstNonKey && got != tc.wantName {
				t.Fatalf("%q first_non_key: name = %q, want %q", tc.line, got, tc.wantName)
			}
			if hasKnownKeyPrefix(got) {
				t.Fatalf("%q %s: name still looks like a credential: %q", tc.line, naming, got)
			}
		}
	}
}
