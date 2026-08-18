package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFinalizeLegacyPlaintextRequiresVerify(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	seed := map[string]interface{}{
		"password": "p", "port": 8080, "host": "127.0.0.1",
		"apiKeys": []map[string]interface{}{
			{"id": "k1", "name": "one", "key": "sk-keep-me-for-rollback", "enabled": true},
		},
		"accounts": []interface{}{},
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatal(err)
	}
	if !LegacyPlaintextRetained() {
		t.Fatal("default must retain plaintext")
	}
	if _, err := FinalizeLegacyPlaintext(nil); err == nil {
		t.Fatal("verify required")
	}
	if _, err := FinalizeLegacyPlaintext(func(id, secret string) error {
		if id != "k1" || secret != "sk-keep-me-for-rollback" {
			t.Fatalf("verify %s %s", id, secret)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if LegacyPlaintextRetained() {
		t.Fatal("finalization must flip retention off")
	}
	keys := ListApiKeys()
	if len(keys) != 1 || keys[0].ID != "k1" || keys[0].Key != "" {
		t.Fatalf("scrub failed: %+v", keys)
	}
	if _, err := FinalizeLegacyPlaintext(func(id, secret string) error {
		t.Fatal("no remaining secrets to verify")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGetOrCreateAPIKeyPepperStable(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"password":"p","port":1,"host":"127.0.0.1","accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatal(err)
	}
	a, err := GetOrCreateAPIKeyPepper()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GetOrCreateAPIKeyPepper()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("pepper regenerated")
	}
}
