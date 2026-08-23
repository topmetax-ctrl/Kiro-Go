package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRejectsFutureSchema4(t *testing.T) {
	b := UpstreamBundle{
		Kind:      UpstreamBundleKind,
		Schema:    4,
		Providers: []UpstreamProvider{prov("p1", "p", "http://a/v1", "sk-a")},
	}
	err := ValidateUpstreamBundle(&b)
	if err == nil || !strings.Contains(err.Error(), "unsupported schema") {
		t.Fatalf("schema 4 should be rejected, got %v", err)
	}
}

func TestImportV1BundleMigratesApiKeyToConnection(t *testing.T) {
	b := UpstreamBundle{
		Version: Version, Kind: UpstreamBundleKind, Schema: 1,
		Providers: []UpstreamProvider{prov("remote-1", "alpha", "https://a.example/v1", "sk-v1-secret")},
		Routes:    []ModelRoute{{ID: "r1", Model: "coding", UpstreamID: "remote-1", Enabled: true}},
	}
	if err := ValidateUpstreamBundle(&b); err != nil {
		t.Fatalf("v1 must still validate: %v", err)
	}
	res := MergeUpstreamBundle(nil, nil, b, nil)
	if res.ProvidersAdded != 1 {
		t.Fatalf("providers added = %d", res.ProvidersAdded)
	}
	p := res.Providers[0]
	if len(p.Connections) != 1 || p.Connections[0].ApiKey != "sk-v1-secret" || p.Connections[0].Name != "Key 1" {
		t.Fatalf("v1 migrate = %+v", p.Connections)
	}
}

func TestImportV2BundleMigratesApiKeyToConnection(t *testing.T) {
	b := bundle(
		[]UpstreamProvider{prov("p1", "alpha", "https://a.example/v1", "sk-v2-secret")},
		[]ModelRoute{multiRoute("r1", "coding", "p1")},
	)
	b.Schema = 2
	if err := ValidateUpstreamBundle(&b); err != nil {
		t.Fatalf("v2 must still validate: %v", err)
	}
	res := MergeUpstreamBundle(nil, nil, b, nil)
	p := res.Providers[0]
	if len(p.Connections) != 1 || p.Connections[0].ApiKey != "sk-v2-secret" {
		t.Fatalf("v2 migrate = %+v", p.Connections)
	}
}

func TestSchema3RoundTripKeepsConnections(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	src := []UpstreamProvider{{
		ID: "p1", Name: "cun.ai", BaseURL: "https://a.example/v1", Enabled: true,
		Connections: []UpstreamConnection{
			{ID: "c1", Name: "user_test_01", ApiKey: "sk-one", Enabled: true},
			{ID: "c2", Name: "user_test_02", ApiKey: "sk-two", Enabled: true},
		},
		ConnectionStrategy: ConnectionStrategyRoundRobin,
	}}
	if err := UpdateUpstreamConfig(src, []ModelRoute{multiRoute("r1", "coding", "p1")}); err != nil {
		t.Fatal(err)
	}
	exported := ExportUpstreamBundle()
	if exported.Schema != 3 {
		t.Fatalf("schema = %d, want 3", exported.Schema)
	}
	if len(exported.Providers) != 1 || len(exported.Providers[0].Connections) != 2 {
		t.Fatalf("export connections = %+v", exported.Providers)
	}
	if exported.Providers[0].ApiKey != "sk-one" {
		t.Fatalf("legacy apiKey = %q, want first connection", exported.Providers[0].ApiKey)
	}

	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	res, err := ImportUpstreamBundle(exported)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProvidersAdded != 1 {
		t.Fatalf("imported providers = %d", res.ProvidersAdded)
	}
	got, _ := GetUpstreamConfig()
	if len(got) != 1 || len(got[0].Connections) != 2 {
		t.Fatalf("imported = %+v", got)
	}
	if got[0].Connections[0].ApiKey != "sk-one" || got[0].Connections[1].ApiKey != "sk-two" {
		t.Fatalf("keys lost: %+v", got[0].Connections)
	}
	if got[0].Connections[0].Name != "user_test_01" {
		t.Fatalf("name = %q", got[0].Connections[0].Name)
	}
}

func TestMergeDuplicateProviderDoesNotOverwriteConnections(t *testing.T) {
	existing := []UpstreamProvider{{
		ID: "local", Name: "alpha", BaseURL: "https://a.example/v1", Enabled: true,
		ApiKey: "sk-local",
		Connections: []UpstreamConnection{
			{ID: "c1", Name: "Key 1", ApiKey: "sk-local", Enabled: true},
		},
	}}
	b := bundle([]UpstreamProvider{{
		ID: "remote", Name: "alpha", BaseURL: "https://a.example/v1", Enabled: true,
		Connections: []UpstreamConnection{
			{ID: "cx", Name: "imported", ApiKey: "sk-imported", Enabled: true},
		},
	}}, nil)
	res := MergeUpstreamBundle(existing, nil, b, nil)
	if res.ProvidersSkipped != 1 {
		t.Fatalf("skipped = %d", res.ProvidersSkipped)
	}
	if got := res.Providers[0].Connections[0].ApiKey; got != "sk-local" {
		t.Fatalf("local key overwritten: %q", got)
	}
}
