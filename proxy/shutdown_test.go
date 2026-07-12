package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// Handler.Shutdown stops the background workers and is idempotent.
func TestHandlerShutdownStopsWorkersIdempotently(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{
		pool:           accountpool.GetPool(),
		stopRefresh:    make(chan struct{}),
		stopStatsSaver: make(chan struct{}),
		promptCache:    newPromptCacheTracker(defaultPromptCacheTTL),
	}
	h.tokenManager = NewTokenManager(h.pool, func(*config.Account) (string, string, int64, string, error) {
		return "t", "r", 0, "", nil
	}, func(string, string, string, int64) error { return nil })

	// Simulate the two background loops so we can prove they exit on Shutdown.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-h.stopRefresh }()
	go func() { defer wg.Done(); <-h.stopStatsSaver }()

	h.Shutdown()
	// Second call must not panic (double close guarded by sync.Once).
	h.Shutdown()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background workers did not observe shutdown")
	}
}

// A graceful srv.Shutdown drains an in-flight streaming request rather than
// cutting it off mid-response.
func TestServerShutdownDrainsInFlightStream(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: chunk1\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			close(started)
			<-release // hold the stream open until the test releases it
			_, _ = w.Write([]byte("data: done\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}),
	}
	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.Start()
	defer ts.Close()

	// Kick off a request that will be in-flight when we shut down.
	respErr := make(chan error, 1)
	go func() {
		resp, err := http.Get(ts.URL)
		if err != nil {
			respErr <- err
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 64)
		_, _ = resp.Body.Read(buf) // read the first chunk
		respErr <- nil
	}()

	<-started
	// Begin graceful shutdown in the background; it should wait for the in-flight
	// handler to finish rather than abort it.
	shutdownDone := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = ts.Config.Shutdown(ctx)
		close(shutdownDone)
	}()

	// Shutdown must NOT complete while the handler is still holding the stream.
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before in-flight stream finished")
	case <-time.After(150 * time.Millisecond):
		// expected: still draining
	}

	close(release) // let the handler finish
	select {
	case <-shutdownDone:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not complete after stream finished")
	}
	if err := <-respErr; err != nil {
		t.Fatalf("client request errored: %v", err)
	}
}
