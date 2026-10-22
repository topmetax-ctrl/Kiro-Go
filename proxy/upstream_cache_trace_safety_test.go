package proxy

// What the cache trace must never do to a request: change the bytes sent
// upstream, outlive the request, hold on to its body, or lose comparability
// across a restart.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/metrics"
)

// goroutinesIn counts live goroutines whose stack mentions marker.
func goroutinesIn(marker string) int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, marker) {
			n++
		}
	}
	return n
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwardSendsTheClientBodyUnchanged(t *testing.T) {
	for name, config := range map[string]string{"tracing on": "", "tracing off": `{"cacheTrace":false}`} {
		t.Run(name, func(t *testing.T) {
			metrics.Reset()
			var got []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"claude-sonnet-5-5","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer upstream.Close()
			setupTraceRoute(t, upstream.URL, config)

			forwardClaude(t, &Handler{}, false)
			want := strings.Replace(claudeBody, "%STREAM%", "false", 1)
			if string(got) != want {
				t.Fatalf("upstream received a different body:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// A client that hangs up mid-request still gets its event, with its trace, and
// the hashing goroutine is gone once the request is.
func TestForwardCanceledRequestKeepsTraceAndLeavesNoGoroutine(t *testing.T) {
	metrics.Reset()
	reached := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server only notices a closed connection once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		close(reached)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	setupTraceRoute(t, upstream.URL, "")

	body := strings.Replace(claudeBody, "%STREAM%", "false", 1)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), apiKeyContextKey{}, ""))
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Handler{}).tryForwardUpstream(r, httptest.NewRecorder(), []byte(body), "claude-trace", false, "/messages", true, "")
	}()
	<-reached
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("forward did not return after the client canceled")
	}

	evs, _ := metrics.Events(metrics.EventFilter{ProviderID: "up-1"})
	if len(evs) != 1 || evs[0].Status != 499 || evs[0].Cache == nil || evs[0].Cache.Msgs != 3 {
		t.Fatalf("canceled request must record its trace: %+v", evs)
	}
	waitUntil(t, "hashing goroutines to exit", func() bool { return goroutinesIn("startCacheTrace") == 0 })
}

// Nothing waits on the result, yet the goroutine must still finish and release
// what it holds: a request that fails before its event is recorded never reads it.
func TestStartCacheTraceNeverReadStillFinishes(t *testing.T) {
	setupTraceRoute(t, "http://127.0.0.1:1", "")
	h := &Handler{}
	body := []byte(strings.Replace(claudeBody, "%STREAM%", "false", 1))
	for i := 0; i < 50; i++ {
		h.startCacheTrace(nil, body, true, "") // reader dropped unread
	}
	waitUntil(t, "unread traces to finish", func() bool { return goroutinesIn("startCacheTrace") == 0 })
}

// After the event is built, the reader must not keep the request body alive: the
// ring holds an event for days, a body can be megabytes.
func TestCacheTraceReaderDoesNotRetainTheBody(t *testing.T) {
	setupTraceRoute(t, "http://127.0.0.1:1", "")
	h := &Handler{}

	head := `{"model":"m","messages":[{"role":"user","content":"`
	tail := `"}]}`
	buf := make([]byte, 0, 4<<20)
	buf = append(buf, head...)
	buf = append(buf, bytes.Repeat([]byte("x"), 3<<20)...)
	buf = append(buf, tail...)
	freed := make(chan struct{})
	runtime.SetFinalizer(&buf[0], func(*byte) { close(freed) })

	read := h.startCacheTrace(nil, buf, true, "")
	if tr := read(); tr == nil || tr.Msgs != 1 {
		t.Fatalf("no trace for a valid body: %+v", tr)
	}
	buf = nil
	waitUntil(t, "the body to be collectable", func() bool {
		select {
		case <-freed:
			return true
		default:
			return false
		}
	})
	runtime.KeepAlive(read)
}

// The key derives from the persisted pepper, so a restart (a fresh Handler over
// the same config file) must reproduce the digests of the run before it.
func TestCacheTraceDigestsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Replace(claudeBody, "%STREAM%", "false", 1))
	trace := func() string {
		if err := config.Init(path); err != nil {
			t.Fatalf("config.Init: %v", err)
		}
		tr := (&Handler{}).startCacheTrace(nil, body, true, "")()
		if tr == nil {
			t.Fatal("no trace")
		}
		raw, _ := json.Marshal(tr)
		return string(raw)
	}
	first := trace() // creates and persists the pepper
	if second := trace(); second != first {
		t.Fatalf("digests changed across a restart:\n%s\n%s", first, second)
	}
	t.Setenv("KIRO_APIKEY_PEPPER", strings.Repeat("ab", 32))
	if other := trace(); other == first {
		t.Fatal("a different secret must give different digests")
	}
}

// Many requests at once each get their own trace and leave no goroutine behind.
func TestStartCacheTraceIsSafeUnderLoad(t *testing.T) {
	setupTraceRoute(t, "http://127.0.0.1:1", "")
	h := &Handler{}
	body := []byte(strings.Replace(claudeBody, "%STREAM%", "false", 1))
	var wg sync.WaitGroup
	for i := 0; i < 256; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.startCacheTrace(nil, body, true, "")() == nil {
				t.Error("nil trace under load")
			}
		}()
	}
	wg.Wait()
	waitUntil(t, "hashing goroutines to exit", func() bool { return goroutinesIn("startCacheTrace") == 0 })
}
