package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/apikey"
	"kiro-go/config"
	"kiro-go/metrics"
)

func setupProviderWithKeys(t *testing.T, baseURL string, keys []string, backupURL string) {
	t.Helper()
	resetConnectionHealthForTest()
	resetConnectionRRForTest()
	metrics.Reset()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	conns := make([]config.UpstreamConnection, len(keys))
	for i, k := range keys {
		conns[i] = config.UpstreamConnection{
			ID: "c" + string(rune('1'+i)), Name: "Key " + string(rune('1'+i)),
			ApiKey: k, Enabled: true,
		}
	}
	ups := []config.UpstreamProvider{{
		ID: "up-a", Name: "primary", BaseURL: baseURL, Enabled: true,
		Connections: conns, ConnectionStrategy: config.ConnectionStrategyPrimary,
	}}
	targets := []config.RouteTarget{{UpstreamID: "up-a", Priority: 0, Weight: 1, Enabled: true}}
	if backupURL != "" {
		ups = append(ups, config.UpstreamProvider{
			ID: "up-b", Name: "backup", BaseURL: backupURL, ApiKey: "backup-key", Enabled: true,
		})
		targets = append(targets, config.RouteTarget{UpstreamID: "up-b", Priority: 1, Weight: 1, Enabled: true})
	}
	route := config.ModelRoute{ID: "r1", Model: "m", Targets: targets, Enabled: true}
	if err := config.UpdateUpstreamConfig(ups, []config.ModelRoute{route}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionFailoverOn401DoesNotHitBackupProvider(t *testing.T) {
	var keys []string
	var backupHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		keys = append(keys, key)
		if key == "key-a" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-b"}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"backup"}`))
	}))
	defer backup.Close()

	setupProviderWithKeys(t, primary.URL, []string{"key-a", "key-b"}, backup.URL)
	rec := forwardOnce(t, "m", false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "from-b") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backupHits != 0 {
		t.Fatalf("backup provider was hit")
	}
	if len(keys) != 2 || keys[0] != "key-a" || keys[1] != "key-b" {
		t.Fatalf("keys tried = %v", keys)
	}
}

func TestAllAuthFailuresDoNotFailoverProvider(t *testing.T) {
	var backupHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		_, _ = w.Write([]byte(`{"id":"backup"}`))
	}))
	defer backup.Close()

	setupProviderWithKeys(t, primary.URL, []string{"key-a", "key-b"}, backup.URL)
	rec := forwardOnce(t, "m", false)
	// The provider error boundary folds the upstream's own 401 into a generic
	// public api_error (502) — the client must not learn whether a key or an
	// entitlement was rejected. The behavioral point of this test is what the
	// status does NOT trigger: no walk to the backup provider.
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "api_error") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backupHits != 0 {
		t.Fatalf("should not fail over on all-401, backupHits=%d", backupHits)
	}
}

func Test429RotatesKeyThenProvider(t *testing.T) {
	var primaryHits, backupHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate"}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"backup-ok"}`))
	}))
	defer backup.Close()

	setupProviderWithKeys(t, primary.URL, []string{"key-a", "key-b"}, backup.URL)
	rec := forwardOnce(t, "m", false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "backup-ok") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if primaryHits != 2 {
		t.Fatalf("primary hits = %d, want 2 keys", primaryHits)
	}
	if backupHits != 1 {
		t.Fatalf("backup hits = %d", backupHits)
	}
}

func TestRoundRobinIsThreadSafeAndSkipsDisabled(t *testing.T) {
	resetConnectionHealthForTest()
	resetConnectionRRForTest()
	p := config.UpstreamProvider{
		ID: "rr", ConnectionStrategy: config.ConnectionStrategyRoundRobin,
		Connections: []config.UpstreamConnection{
			{ID: "a", Name: "A", ApiKey: "a", Enabled: true},
			{ID: "b", Name: "B", ApiKey: "b", Enabled: false},
			{ID: "c", Name: "C", ApiKey: "c", Enabled: true},
		},
	}
	var mu sync.Mutex
	first := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order := orderProviderConnections(p, time.Now())
			if len(order) != 2 {
				t.Errorf("expected 2 enabled, got %d", len(order))
				return
			}
			mu.Lock()
			first[order[0].ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if first["a"] == 0 || first["c"] == 0 || first["b"] != 0 {
		t.Fatalf("distribution = %v", first)
	}
}

func TestCooldownConnectionSkipped(t *testing.T) {
	resetConnectionHealthForTest()
	resetConnectionRRForTest()
	p := config.UpstreamProvider{
		ID: "cd", ConnectionStrategy: config.ConnectionStrategyPrimary,
		Connections: []config.UpstreamConnection{
			{ID: "hot", Name: "hot", ApiKey: "hot", Enabled: true},
			{ID: "ok", Name: "ok", ApiKey: "ok", Enabled: true},
		},
	}
	now := time.Now()
	recordConnectionFailure("cd", "hot", 429, now)
	recordConnectionFailure("cd", "hot", 429, now)
	recordConnectionFailure("cd", "hot", 429, now)
	order := orderProviderConnections(p, now)
	if len(order) != 1 || order[0].ID != "ok" {
		t.Fatalf("expected only the healthy key, got %+v", order)
	}
}

func TestConnectionDoesNotRotateAfterStreamCommit(t *testing.T) {
	var second int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key != "key-a" {
			atomic.AddInt32(&second, 1)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"chunk\"}\n\n"))
		flusher.Flush()
	}))
	defer primary.Close()
	setupProviderWithKeys(t, primary.URL, []string{"key-a", "key-b"}, "")
	rec := forwardOnce(t, "m", true)
	if !strings.Contains(rec.Body.String(), "chunk") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if second != 0 {
		t.Fatalf("rotated to second key after stream started")
	}
}

// End to end: every key on a pool is rejected, so the admin diagnostic must name
// each dead credential. This is the pairing of the key pool with the provider
// error boundary — the client is told nothing beyond a generic api_error, while the
// operator gets one classified row per key, enough to know which to replace.
func TestPoolFailureNamesEveryDeadKeyToAdminOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"INVALID_KEY","message":"rejected ` + key + `"}}`))
	}))
	defer upstream.Close()

	setupProviderWithKeys(t, upstream.URL, []string{"key-a", "key-b", "key-c"}, "")
	svc, err := apikey.Open(filepath.Join(t.TempDir(), "k.db"), []byte("pepper-32-bytes-long-for-tests!!!!"), apikey.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	h := &Handler{keys: svc}
	rec := httptest.NewRecorder()
	body := `{"model":"m","messages":[],"stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, ""))
	r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, "req-pool-e2e"))
	if !h.tryForwardUpstream(r, rec, []byte(body), "m", false, "/messages", true, "") {
		t.Fatal("expected the route to forward")
	}

	// The client learns nothing about the credentials.
	client := rec.Body.String()
	for _, key := range []string{"key-a", "key-b", "key-c", "INVALID_KEY"} {
		if strings.Contains(client, key) {
			t.Fatalf("client response leaked %q: %s", key, client)
		}
	}

	details, err := svc.GetProviderErrorDetails("req-pool-e2e")
	if err != nil {
		t.Fatalf("no diagnostic stored: %v", err)
	}
	if len(details) != 3 {
		t.Fatalf("want one row per key tried, got %d: %+v", len(details), details)
	}
	seen := map[string]bool{}
	for _, d := range details {
		seen[d.ConnectionName] = true
		if d.UpstreamStatus != 401 {
			t.Errorf("%s recorded status %d", d.ConnectionName, d.UpstreamStatus)
		}
		if d.ConnectionID == "" {
			t.Errorf("%s has no connection id", d.ConnectionName)
		}
	}
	for _, name := range []string{"Key 1", "Key 2", "Key 3"} {
		if !seen[name] {
			t.Errorf("diagnostic missing %s; got %v", name, seen)
		}
	}
}
