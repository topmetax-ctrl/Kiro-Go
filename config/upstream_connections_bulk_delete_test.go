package config

import (
	"errors"
	"path/filepath"
	"testing"
)

// Clearing a selection from the admin key list is one call, not N deletes: each
// delete takes the config lock and rewrites the file while forwards are in
// flight. These pin the semantics the UI relies on.

func setupBulkDeleteCfg(t *testing.T, n int) []UpstreamConnection {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	conns := make([]UpstreamConnection, 0, n)
	for i := 1; i <= n; i++ {
		conns = append(conns, UpstreamConnection{
			ID:      "c" + string(rune('0'+i)),
			Name:    NextKeyNName(nil),
			ApiKey:  "sk-FIXTUREnotarealkey00000000" + string(rune('0'+i)),
			Enabled: true,
		})
	}
	for i := range conns {
		conns[i].Name = "Key " + string(rune('0'+i+1))
	}
	p := UpstreamProvider{ID: "p1", Name: "prov", BaseURL: "https://h/v1", Enabled: true, Connections: conns}
	if err := UpdateUpstreamConfig([]UpstreamProvider{p}, nil); err != nil {
		t.Fatal(err)
	}
	return conns
}

func storedConnections(t *testing.T) []UpstreamConnection {
	t.Helper()
	providers, _ := GetUpstreamConfig()
	for _, p := range providers {
		if p.ID == "p1" {
			return p.Connections
		}
	}
	t.Fatal("provider p1 missing")
	return nil
}

func TestDeleteUpstreamConnectionsRemovesOnlySelected(t *testing.T) {
	conns := setupBulkDeleteCfg(t, 5)
	removed, err := DeleteUpstreamConnections("p1", []string{conns[1].ID, conns[3].ID})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	left := storedConnections(t)
	if len(left) != 3 {
		t.Fatalf("kept %d connections, want 3", len(left))
	}
	want := []string{conns[0].ID, conns[2].ID, conns[4].ID}
	for i, id := range want {
		if left[i].ID != id {
			t.Fatalf("kept[%d] = %q, want %q (order must survive)", i, left[i].ID, id)
		}
	}
	// The surviving keys must still be usable, not blanked by the rewrite.
	for _, c := range left {
		if c.ApiKey == "" {
			t.Fatalf("connection %q lost its key", c.Name)
		}
	}
}

func TestDeleteUpstreamConnectionsSkipsUnknownIDs(t *testing.T) {
	// The list the operator selected from can be seconds stale: a key deleted in
	// another tab must not fail the whole batch.
	conns := setupBulkDeleteCfg(t, 3)
	removed, err := DeleteUpstreamConnections("p1", []string{conns[0].ID, "gone-already", ""})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if len(storedConnections(t)) != 2 {
		t.Fatalf("kept %d, want 2", len(storedConnections(t)))
	}
}

func TestDeleteUpstreamConnectionsMatchingNothingErrors(t *testing.T) {
	// Reporting success for a no-op would let the UI claim it deleted keys that
	// are still forwarding traffic.
	setupBulkDeleteCfg(t, 2)
	if _, err := DeleteUpstreamConnections("p1", []string{"nope"}); !errors.Is(err, ErrUpstreamConnectionNotFound) {
		t.Fatalf("err = %v, want ErrUpstreamConnectionNotFound", err)
	}
	if _, err := DeleteUpstreamConnections("p1", nil); !errors.Is(err, ErrUpstreamConnectionNotFound) {
		t.Fatalf("empty ids err = %v, want ErrUpstreamConnectionNotFound", err)
	}
	if len(storedConnections(t)) != 2 {
		t.Fatal("a failed bulk delete must not touch the pool")
	}
}

func TestDeleteUpstreamConnectionsUnknownProvider(t *testing.T) {
	setupBulkDeleteCfg(t, 1)
	if _, err := DeleteUpstreamConnections("nope", []string{"c1"}); !errors.Is(err, ErrUpstreamProviderNotFound) {
		t.Fatalf("err = %v, want ErrUpstreamProviderNotFound", err)
	}
}

func TestDeleteUpstreamConnectionsClearingPoolDropsLegacyKey(t *testing.T) {
	// Emptying the pool must also clear the deprecated provider-level ApiKey, or
	// the provider keeps forwarding on a key with no row in the list.
	conns := setupBulkDeleteCfg(t, 2)
	removed, err := DeleteUpstreamConnections("p1", []string{conns[0].ID, conns[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	providers, _ := GetUpstreamConfig()
	for _, p := range providers {
		if p.ID != "p1" {
			continue
		}
		if len(p.Connections) != 0 {
			t.Fatalf("kept %d connections, want 0", len(p.Connections))
		}
		if p.ApiKey != "" {
			t.Fatalf("legacy ApiKey = %q, want cleared", p.ApiKey)
		}
	}
}

func TestDeleteUpstreamConnectionsKeepsLegacyKeyInSyncWithFirstRow(t *testing.T) {
	// Deleting the head of the pool promotes the next key; the legacy mirror has
	// to follow or exports hand out a secret the provider no longer uses.
	conns := setupBulkDeleteCfg(t, 3)
	if _, err := DeleteUpstreamConnections("p1", []string{conns[0].ID}); err != nil {
		t.Fatal(err)
	}
	providers, _ := GetUpstreamConfig()
	for _, p := range providers {
		if p.ID != "p1" {
			continue
		}
		if p.ApiKey != conns[1].ApiKey {
			t.Fatalf("legacy ApiKey = %q, want the new first row %q", p.ApiKey, conns[1].ApiKey)
		}
	}
}

func TestDeleteUpstreamConnectionsDoesNotCorruptASharedSnapshot(t *testing.T) {
	// A forward in flight holds a slice that shares its backing array with the
	// stored pool. Compacting in place would rewrite the rows under it.
	conns := setupBulkDeleteCfg(t, 4)
	snapshot := storedConnections(t)
	before := make([]string, len(snapshot))
	for i, c := range snapshot {
		before[i] = c.ID
	}
	if _, err := DeleteUpstreamConnections("p1", []string{conns[0].ID, conns[1].ID}); err != nil {
		t.Fatal(err)
	}
	for i, c := range snapshot {
		if c.ID != before[i] {
			t.Fatalf("snapshot[%d] changed from %q to %q under a concurrent reader", i, before[i], c.ID)
		}
	}
}
