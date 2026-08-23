package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNextKeyNNameGapFill(t *testing.T) {
	got := NextKeyNName([]string{"Key 1", "Key 3", "Key 4", "user_test"})
	if got != "Key 2" {
		t.Fatalf("NextKeyNName = %q, want Key 2", got)
	}
	if got := NextKeyNName(nil); got != "Key 1" {
		t.Fatalf("empty = %q, want Key 1", got)
	}
	if got := NextKeyNName([]string{"Key 1", "Key 2"}); got != "Key 3" {
		t.Fatalf("sequential = %q, want Key 3", got)
	}
}

func TestMaskConnectionSecretHidesShortKeys(t *testing.T) {
	if got := MaskConnectionSecret("short"); got != "****" {
		t.Fatalf("short key = %q, want ****", got)
	}
	if got := MaskConnectionSecret(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
	got := MaskConnectionSecret("sk-abcdefghijklmnopqrstuvwxyz")
	if got != "sk-abc****wxyz" {
		t.Fatalf("long = %q", got)
	}
}

func TestResolvedConnectionsFallsBackToLegacyApiKey(t *testing.T) {
	p := UpstreamProvider{ID: "p", ApiKey: "sk-legacy"}
	conns := ResolvedConnections(p)
	if len(conns) != 1 || conns[0].ApiKey != "sk-legacy" || conns[0].Name != "Key 1" {
		t.Fatalf("unexpected fallback: %+v", conns)
	}
	p.Connections = []UpstreamConnection{{ID: "c1", Name: "A", ApiKey: "sk-a", Enabled: true}}
	conns = ResolvedConnections(p)
	if len(conns) != 1 || conns[0].ApiKey != "sk-a" {
		t.Fatalf("connections should win: %+v", conns)
	}
}

func TestMigrateLegacyApiKeyIdempotentOnLoad(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	seed := map[string]interface{}{
		"password": "p",
		"port":     8080,
		"host":     "127.0.0.1",
		"upstreams": []map[string]interface{}{
			{
				"id":      "p1",
				"name":    "cun.ai",
				"baseUrl": "https://api.example/v1",
				"apiKey":  "sk-real-secret-key",
				"enabled": true,
			},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	providers, _ := GetUpstreamConfig()
	if len(providers) != 1 {
		t.Fatalf("providers = %d", len(providers))
	}
	p := providers[0]
	if len(p.Connections) != 1 {
		t.Fatalf("connections = %d, want 1", len(p.Connections))
	}
	c := p.Connections[0]
	if c.ApiKey != "sk-real-secret-key" || c.Name != "Key 1" || !c.Enabled || c.ID == "" {
		t.Fatalf("migrated connection = %+v", c)
	}
	firstID := c.ID

	if err := Init(cfgFile); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	providers, _ = GetUpstreamConfig()
	if len(providers[0].Connections) != 1 {
		t.Fatalf("re-init created extra connections: %+v", providers[0].Connections)
	}
	if providers[0].Connections[0].ID != firstID {
		t.Fatalf("connection id changed across restart: %q -> %q", firstID, providers[0].Connections[0].ID)
	}
	if providers[0].Connections[0].ApiKey != "sk-real-secret-key" {
		t.Fatal("secret lost on reload")
	}
}

func TestHundredsOfConnectionsStayOnOneProvider(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	p := UpstreamProvider{ID: "p1", Name: "one", BaseURL: "https://h/v1", Enabled: true}
	if err := UpdateUpstreamConfig([]UpstreamProvider{p}, nil); err != nil {
		t.Fatal(err)
	}
	batch := make([]UpstreamConnection, 200)
	for i := range batch {
		batch[i] = UpstreamConnection{Name: NextKeyNName(nil), ApiKey: "sk-batch-" + itoaForTest(i), Enabled: true}
	}
	// Unique names aren't required; keys are.
	added, err := AddUpstreamConnections("p1", batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 200 {
		t.Fatalf("added = %d, want 200", len(added))
	}
	providers, _ := GetUpstreamConfig()
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	if len(providers[0].Connections) != 200 {
		t.Fatalf("connections = %d, want 200", len(providers[0].Connections))
	}
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestConnectionCRUDAndDedupe(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := UpdateUpstreamConfig([]UpstreamProvider{{
		ID: "p1", Name: "n", BaseURL: "https://h/v1", Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := AddUpstreamConnection("p1", UpstreamConnection{Name: "user1", ApiKey: "sk-aaa", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == "" {
		t.Fatal("expected id")
	}
	if _, err := AddUpstreamConnection("p1", UpstreamConnection{ApiKey: "sk-aaa", Enabled: true}); err != ErrDuplicateConnectionKey {
		t.Fatalf("dup = %v, want ErrDuplicateConnectionKey", err)
	}
	name := "renamed"
	enabled := false
	got, err := UpdateUpstreamConnection("p1", c.ID, UpstreamConnectionPatch{Name: &name, Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.Enabled || got.ApiKey != "sk-aaa" {
		t.Fatalf("patch = %+v", got)
	}
	if err := DeleteUpstreamConnection("p1", c.ID); err != nil {
		t.Fatal(err)
	}
	providers, _ := GetUpstreamConfig()
	if len(providers[0].Connections) != 0 {
		t.Fatalf("expected empty connections, got %+v", providers[0].Connections)
	}
}
