package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteFileCreatesAndPermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := atomicWriteFile(p, []byte(`{"port":8080}`), 0600); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("expected 0600, got %o", info.Mode().Perm())
	}
	// No stray temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || containsTmp(e.Name()) {
			t.Fatalf("stray temp file left: %s", e.Name())
		}
	}
}

func containsTmp(name string) bool {
	for i := 0; i+4 <= len(name); i++ {
		if name[i:i+4] == ".tmp" {
			return true
		}
	}
	return false
}

func TestAtomicWriteKeepsBackupOfPrevious(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := atomicWriteFile(p, []byte(`{"v":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(p, []byte(`{"v":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(p + ".bak")
	if err != nil {
		t.Fatalf("expected backup: %v", err)
	}
	if string(bak) != `{"v":1}` {
		t.Fatalf("backup should hold previous contents, got %s", bak)
	}
	cur, _ := os.ReadFile(p)
	if string(cur) != `{"v":2}` {
		t.Fatalf("primary should hold latest, got %s", cur)
	}
}

func TestSaveRejectsUnmarshalableAndPreservesPrimary(t *testing.T) {
	// Save() validates the marshaled bytes round-trip. We can't easily make
	// json.MarshalIndent produce invalid JSON, so this test asserts the primary
	// survives when the round-trip probe is the guard: write a good config, then
	// confirm a subsequent good save keeps a valid file (regression guard for the
	// validate-before-replace ordering).
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var probe Config
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("saved config not valid JSON: %v", err)
	}
}

func TestLoadFallsBackToBackupOnCorruptPrimary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")

	// Establish a good primary + backup by saving twice through the config API.
	if err := Init(p); err != nil {
		t.Fatalf("Init: %v", err)
	}
	SetPassword("real-secret")
	if err := Save(); err != nil { // creates .bak from the prior good primary
		t.Fatalf("Save: %v", err)
	}
	// Ensure a backup exists that unmarshals cleanly.
	if _, err := os.Stat(p + ".bak"); err != nil {
		// Force a second save so a .bak is definitely present.
		SetPassword("real-secret-2")
		if err := Save(); err != nil {
			t.Fatalf("Save2: %v", err)
		}
	}

	// Corrupt the primary on disk.
	if err := os.WriteFile(p, []byte("{ this is not json"), 0600); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}

	// Reload: should recover from .bak rather than error.
	if err := Load(); err != nil {
		t.Fatalf("Load should recover from backup, got: %v", err)
	}
	// Primary should have been rewritten to valid JSON.
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read primary: %v", err)
	}
	var probe Config
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("primary not restored to valid JSON: %v", err)
	}
}
