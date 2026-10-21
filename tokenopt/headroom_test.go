package tokenopt

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func clientFor(t *testing.T, base string, mut func(*config.HeadroomConfig)) *HeadroomClient {
	t.Helper()
	h := config.HeadroomConfig{BaseURL: base, TimeoutMs: 1500}
	if mut != nil {
		mut(&h)
	}
	c, err := NewHeadroomClient(h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.hc.CloseIdleConnections)
	return c
}

func engineKind(t *testing.T, err error) (ErrKind, int) {
	t.Helper()
	var ee *EngineError
	if !errors.As(err, &ee) {
		t.Fatalf("want *EngineError, got %T %v", err, err)
	}
	return ee.Kind, ee.Status
}

// The request must have exactly the shape Headroom's /v1/compress reads.
func TestCompressRequestShape(t *testing.T) {
	f := newFakeHeadroom(t)
	c := clientFor(t, f.server.URL, func(h *config.HeadroomConfig) { h.APIKey = "s3cret" })
	msgs := []byte(`[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"c","content":"` + strings.Repeat("x", 300) + `"}]`)

	if _, err := c.Compress(context.Background(), CompressParams{Model: "claude-x", Messages: msgs, Mode: config.HeadroomModeCache, Frozen: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Compress(context.Background(), CompressParams{Model: "claude-x", Messages: msgs, Mode: config.HeadroomModeToken, Frozen: 1}); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	cache, token := calls[0], calls[1]
	if cache.Model != "claude-x" || len(cache.Messages) != 2 {
		t.Errorf("model/messages not forwarded: %+v", cache)
	}
	if cache.Token != "s3cret" {
		t.Errorf("proxy token not sent as x-headroom-proxy-token")
	}
	// Cache mode: newest observation compressed at once, everything before it frozen.
	if cache.Config["protect_recent"].(interface{ String() string }).String() != "0" ||
		cache.Config["frozen_message_count"].(interface{ String() string }).String() != "1" {
		t.Errorf("cache mode config = %v", cache.Config)
	}
	// Token mode leaves the engine's own policy alone: no config at all.
	if len(token.Config) != 0 {
		t.Errorf("token mode must not send config, got %v", token.Config)
	}
	// Never the knobs a stateless gateway cannot honor.
	for _, c := range calls {
		for _, forbidden := range []string{"mode", "session_id", "compress_user_messages"} {
			if _, ok := c.Config[forbidden]; ok {
				t.Errorf("config must not set %q: it would need a retrieval loop or per-session state", forbidden)
			}
		}
	}
}

func TestCompressSendsNoTokenHeaderWhenNoneConfigured(t *testing.T) {
	f := newFakeHeadroom(t)
	c := clientFor(t, f.server.URL, nil)
	if _, err := c.Compress(context.Background(), CompressParams{Model: "m", Messages: []byte(`[]`), Mode: config.HeadroomModeToken}); err != nil {
		t.Fatal(err)
	}
	if got := f.calls()[0].Token; got != "" {
		t.Fatalf("token header = %q, want none", got)
	}
}

func TestCompressParsesAReply(t *testing.T) {
	f := newFakeHeadroom(t)
	f.tokensSaved = 321
	c := clientFor(t, f.server.URL, nil)
	big := strings.Repeat("y", 400)
	r, err := c.Compress(context.Background(), CompressParams{Model: "m", Messages: []byte(`[{"role":"tool","tool_call_id":"c","content":"` + big + `"}]`), Mode: config.HeadroomModeToken})
	if err != nil {
		t.Fatal(err)
	}
	if r.TokensSaved != 321 || r.TokensBefore != 10000 || len(r.Transforms) != 1 || len(r.Messages) != 1 {
		t.Fatalf("reply = %+v", r)
	}
	if got := r.Messages[0].(map[string]any)["content"]; got != squeeze(big) {
		t.Fatalf("content = %v", got)
	}
}

func TestCompressClassifiesFailures(t *testing.T) {
	cases := []struct {
		name   string
		set    func(f *fakeHeadroom)
		kind   ErrKind
		status int
	}{
		{"500", func(f *fakeHeadroom) { f.status = 500 }, KindServer, 500},
		{"503", func(f *fakeHeadroom) { f.status = 503 }, KindServer, 503},
		{"429 is the engine shedding load", func(f *fakeHeadroom) { f.status = 429 }, KindServer, 429},
		{"401 wrong token", func(f *fakeHeadroom) { f.status = 401 }, KindRejected, 401},
		{"404 loopback guard", func(f *fakeHeadroom) { f.status = 404 }, KindRejected, 404},
		{"400 payload refused", func(f *fakeHeadroom) { f.status = 400 }, KindRejected, 400},
		{"not json", func(f *fakeHeadroom) { f.raw = "<html>oops</html>" }, KindBadResponse, 200},
		{"no messages field", func(f *fakeHeadroom) { f.raw = `{"tokens_saved":1}` }, KindBadResponse, 200},
		{"messages not an array", func(f *fakeHeadroom) { f.raw = `{"messages":"x"}` }, KindBadResponse, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHeadroom(t)
			tc.set(f)
			_, err := clientFor(t, f.server.URL, nil).Compress(context.Background(), CompressParams{Model: "m", Messages: []byte(`[]`), Mode: config.HeadroomModeToken})
			kind, status := engineKind(t, err)
			if kind != tc.kind || status != tc.status {
				t.Fatalf("kind/status = %s/%d, want %s/%d (%v)", kind, status, tc.kind, tc.status, err)
			}
			// A failure message must never carry request or response content.
			if strings.Contains(err.Error(), "oops") {
				t.Errorf("response body leaked into the error: %v", err)
			}
		})
	}
}

func TestCompressTimeout(t *testing.T) {
	f := newFakeHeadroom(t)
	f.delay = 500 * time.Millisecond
	c := clientFor(t, f.server.URL, func(h *config.HeadroomConfig) { h.TimeoutMs = 120 })
	start := time.Now()
	_, err := c.Compress(context.Background(), CompressParams{Model: "m", Messages: []byte(`[]`), Mode: config.HeadroomModeToken})
	if kind, _ := engineKind(t, err); kind != KindTimeout {
		t.Fatalf("kind = %s, want timeout (%v)", kind, err)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Fatalf("timeout not enforced: took %s", time.Since(start))
	}
}

func TestCompressCallerCancellationIsNotATimeoutOrAFailure(t *testing.T) {
	f := newFakeHeadroom(t)
	f.delay = 400 * time.Millisecond
	c := clientFor(t, f.server.URL, func(h *config.HeadroomConfig) { h.TimeoutMs = 5000 })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(60 * time.Millisecond); cancel() }()
	_, err := c.Compress(ctx, CompressParams{Model: "m", Messages: []byte(`[]`), Mode: config.HeadroomModeToken})
	if kind, _ := engineKind(t, err); kind != KindCanceled {
		t.Fatalf("kind = %s, want canceled", kind)
	}
}

func TestCompressUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens any more
	_, err := clientFor(t, base, nil).Compress(context.Background(), CompressParams{Model: "m", Messages: []byte(`[]`), Mode: config.HeadroomModeToken})
	if kind, _ := engineKind(t, err); kind != KindUnavailable {
		t.Fatalf("kind = %s, want unavailable (%v)", kind, err)
	}
	if strings.Contains(err.Error(), base) {
		t.Errorf("the configured URL should not be echoed in the error: %v", err)
	}
}

func TestCompressBoundsPayloadsBothWays(t *testing.T) {
	f := newFakeHeadroom(t)
	c := clientFor(t, f.server.URL, nil)

	if _, err := c.Compress(context.Background(), CompressParams{Model: "m", Messages: make([]byte, MaxEnginePayload+1), Mode: config.HeadroomModeToken}); err == nil {
		t.Fatal("oversized request accepted")
	} else if kind, _ := engineKind(t, err); kind != KindTooLarge {
		t.Fatalf("kind = %s", kind)
	}
	if f.callCount() != 0 {
		t.Fatal("an oversized request must be refused before any network call")
	}

	f.raw = `{"messages":[],"pad":"` + strings.Repeat("a", MaxEnginePayload+10) + `"}`
	if _, err := c.Compress(context.Background(), CompressParams{Model: "m", Messages: []byte(`[]`), Mode: config.HeadroomModeToken}); err == nil {
		t.Fatal("oversized reply accepted")
	} else if kind, _ := engineKind(t, err); kind != KindTooLarge {
		t.Fatalf("kind = %s", kind)
	}
}

// A redirect from the service must not steer the request (and the conversation)
// somewhere else.
func TestCompressDoesNotFollowRedirects(t *testing.T) {
	hit := make(chan struct{}, 1)
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit <- struct{}{} }))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/compress", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := clientFor(t, redirector.URL, nil).Compress(context.Background(), CompressParams{Model: "m", Messages: []byte(`[]`), Mode: config.HeadroomModeToken})
	if kind, status := engineKind(t, err); kind != KindRejected || status != http.StatusTemporaryRedirect {
		t.Fatalf("kind/status = %s/%d, want rejected/307", kind, status)
	}
	select {
	case <-hit:
		t.Fatal("the redirect target was contacted")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestUnsafeAddressClassification(t *testing.T) {
	blocked := []string{"169.254.169.254", "169.254.0.1", "0.0.0.0", "::", "fe80::1", "224.0.0.1", "ff02::1", "100.100.100.200", "fd00:ec2::254", "::ffff:169.254.169.254"}
	allowed := []string{"127.0.0.1", "::1", "10.0.0.5", "192.168.65.254", "172.17.0.1", "8.8.8.8", "fd12::1"}
	for _, s := range blocked {
		if ip := net.ParseIP(s); ip == nil || !unsafeAddr(ip) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range allowed {
		if ip := net.ParseIP(s); ip == nil || unsafeAddr(ip) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

// The guard has to hold at connect time, whatever the URL says.
func TestClientRefusesToDialMetadataAddresses(t *testing.T) {
	for _, base := range []string{"http://169.254.169.254", "http://[fe80::1]:8787", "http://0.0.0.0:8787"} {
		c := clientFor(t, base, func(h *config.HeadroomConfig) { h.TimeoutMs = 1000 })
		start := time.Now()
		_, err := c.Compress(context.Background(), CompressParams{Model: "m", Messages: []byte(`[]`), Mode: config.HeadroomModeToken})
		if err == nil || !strings.Contains(err.Error(), "refusing to connect") {
			t.Errorf("%s: err = %v, want a refusal", base, err)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Errorf("%s: refusal should be immediate, took %s", base, time.Since(start))
		}
	}
}

func TestNewHeadroomClientValidatesTheURL(t *testing.T) {
	for _, bad := range []string{"", "127.0.0.1:8787", "ftp://x", "http://u:p@x", "http://x?y=1", "file:///etc/passwd"} {
		if _, err := NewHeadroomClient(config.HeadroomConfig{BaseURL: bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if c, err := NewHeadroomClient(config.HeadroomConfig{BaseURL: "http://127.0.0.1:8787/"}); err != nil || c.base != "http://127.0.0.1:8787" {
		t.Errorf("good URL: %v %v", c, err)
	}
}

func TestProbe(t *testing.T) {
	t.Run("healthy and compressing", func(t *testing.T) {
		f := newFakeHeadroom(t)
		rep := clientFor(t, f.server.URL, nil).Probe(context.Background())
		if !rep.Reachable || !rep.CompressOK || rep.Version != TestedHeadroomVersion || rep.Compat != "tested" || rep.Service != "headroom-proxy" || rep.Error != "" {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("liveness is not enough: compress rejected by the loopback guard", func(t *testing.T) {
		f := newFakeHeadroom(t)
		f.status = 404
		rep := clientFor(t, f.server.URL, nil).Probe(context.Background())
		if !rep.Reachable || rep.CompressOK || !strings.Contains(rep.Error, "HEADROOM_COMPRESS_ALLOW_REMOTE") {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("token rejected", func(t *testing.T) {
		f := newFakeHeadroom(t)
		f.token = "right"
		rep := clientFor(t, f.server.URL, func(h *config.HeadroomConfig) { h.APIKey = "wrong" }).Probe(context.Background())
		if !rep.Reachable || rep.CompressOK || !strings.Contains(rep.Error, "token") {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("falls back to /health on a release without /livez", func(t *testing.T) {
		f := newFakeHeadroom(t)
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok","version":"0.20.1"}`)) })
		mux.HandleFunc("/v1/compress", f.compressHandler)
		srv := httptest.NewServer(mux)
		defer srv.Close()
		rep := clientFor(t, srv.URL, nil).Probe(context.Background())
		if !rep.Reachable || !rep.CompressOK || rep.Version != "0.20.1" || rep.Compat != "untested" {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("unhealthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
		defer srv.Close()
		rep := clientFor(t, srv.URL, nil).Probe(context.Background())
		if rep.Reachable || rep.CompressOK || rep.Error == "" {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		if rep := clientFor(t, base, nil).Probe(context.Background()); rep.Reachable || rep.Error == "" {
			t.Fatalf("report = %+v", rep)
		}
	})
}

func TestVersionCompat(t *testing.T) {
	got := map[string]string{}
	for _, v := range []string{"0.41.0", "0.41.9", "v0.41.2", "0.42.0", "1.0.0", "", "weird"} {
		got[v] = versionCompat(v)
	}
	want := map[string]string{"0.41.0": "tested", "0.41.9": "tested", "v0.41.2": "tested", "0.42.0": "untested", "1.0.0": "untested", "": "unknown", "weird": "unknown"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestProbeURLForRTKProxy(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte("ok"))
			return
		}
		http.NotFound(w, r)
	}))
	defer ok.Close()
	if rep := ProbeURL(context.Background(), ok.URL, time.Second); !rep.Reachable || rep.Error != "" {
		t.Fatalf("report = %+v", rep)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if rep := ProbeURL(context.Background(), bad.URL, time.Second); rep.Reachable || rep.Error == "" {
		t.Fatalf("report = %+v", rep)
	}
	if rep := ProbeURL(context.Background(), "not a url", time.Second); rep.Reachable || rep.Error == "" {
		t.Fatalf("report = %+v", rep)
	}
}
