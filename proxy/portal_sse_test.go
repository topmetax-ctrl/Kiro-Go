package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/apikey"
)

type sseFrame struct {
	ID    string
	Event string
	Data  string
}

func readSSE(r io.Reader, stop func([]sseFrame) bool, d time.Duration) []sseFrame {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	br := bufio.NewReader(r)
	var frames []sseFrame
	var cur sseFrame
	for {
		select {
		case <-ctx.Done():
			return frames
		default:
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return frames
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if cur.ID != "" || cur.Data != "" || cur.Event != "" {
				frames = append(frames, cur)
				cur = sseFrame{}
				if stop != nil && stop(frames) {
					return frames
				}
			}
		case strings.HasPrefix(line, "id:"):
			cur.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case strings.HasPrefix(line, ":"):
			cur.Event = "ping"
			cur.Data = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			frames = append(frames, cur)
			cur = sseFrame{}
			if stop != nil && stop(frames) {
				return frames
			}
		}
	}
}

func openPortalSSE(t *testing.T, srv *httptest.Server, cookies []*http.Cookie, extra string, lastID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/portal/api/events/stream"+extra, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		cancel()
		t.Fatalf("sse %d", res.StatusCode)
	}
	return res, cancel
}

func TestPortalSSEReplayIsolationHeartbeatSession(t *testing.T) {
	prevPing, prevReauth := portalSSEPing, portalSSEReauth
	portalSSEPing = 40 * time.Millisecond
	portalSSEReauth = 40 * time.Millisecond
	t.Cleanup(func() {
		portalSSEPing = prevPing
		portalSSEReauth = prevReauth
	})

	h, svc := testKeyHandler(t)
	a, sa, err := svc.Create(apikey.CreateInput{Name: "alice", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := svc.Create(apikey.CreateInput{Name: "bob", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Commit(b.Key.ID, apikey.CommitInput{
		RequestID: "bob-secret", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", InputTokens: 77, StatusCode: 200,
	})
	_ = svc.Commit(a.Key.ID, apikey.CommitInput{
		RequestID: "alice-1", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200,
	})
	hw, _ := svc.EventHighWater(a.Key.ID)

	srv := httptest.NewServer(http.HandlerFunc(h.handlePortal))
	t.Cleanup(srv.Close)

	res, err := http.Post(srv.URL+"/portal/api/session", "application/json", strings.NewReader(`{"key":"`+sa+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	cookies := res.Cookies()
	_ = res.Body.Close()

	stream, cancel := openPortalSSE(t, srv, cookies, "", "")
	defer cancel()
	defer stream.Body.Close()

	_ = svc.Commit(b.Key.ID, apikey.CommitInput{RequestID: "bob-live", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	_ = svc.Commit(a.Key.ID, apikey.CommitInput{RequestID: "alice-live", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})

	frames := readSSE(stream.Body, func(fs []sseFrame) bool {
		joined := ""
		for _, f := range fs {
			joined += f.Data
		}
		return strings.Contains(joined, "alice-live")
	}, 2*time.Second)
	blob := ""
	var lastAliceID string
	for _, f := range frames {
		blob += f.Event + ":" + f.Data + "\n"
		if strings.Contains(f.Data, "alice-live") {
			lastAliceID = f.ID
		}
		if f.Event == "request" {
			var ev apikey.PublicEvent
			_ = json.Unmarshal([]byte(f.Data), &ev)
			if ev.ApiKeyID != "" {
				t.Fatalf("sse leaked apiKeyId: %+v", ev)
			}
		}
	}
	if strings.Contains(blob, "bob-secret") || strings.Contains(blob, "bob-live") {
		t.Fatalf("alice sse leaked bob: %s", blob)
	}
	if !strings.Contains(blob, "alice-live") {
		t.Fatalf("missed live: %s", blob)
	}
	cancel()
	_ = stream.Body.Close()

	// Missed events while disconnected must replay.
	_ = svc.Commit(a.Key.ID, apikey.CommitInput{RequestID: "alice-gap", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	_ = svc.Commit(b.Key.ID, apikey.CommitInput{RequestID: "bob-gap", Outcome: apikey.OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	if lastAliceID == "" {
		lastAliceID = fmt.Sprintf("%d", hw)
	}
	stream2, cancel2 := openPortalSSE(t, srv, cookies, "", lastAliceID)
	defer cancel2()
	defer stream2.Body.Close()
	frames2 := readSSE(stream2.Body, func(fs []sseFrame) bool {
		for _, f := range fs {
			if strings.Contains(f.Data, "alice-gap") {
				return true
			}
		}
		return false
	}, 2*time.Second)
	blob2 := ""
	seen := map[string]int{}
	for _, f := range frames2 {
		blob2 += f.Data
		if f.Event == "request" {
			var ev apikey.PublicEvent
			_ = json.Unmarshal([]byte(f.Data), &ev)
			seen[ev.RequestID]++
		}
	}
	if strings.Contains(blob2, "bob-gap") || strings.Contains(blob2, "bob-secret") {
		t.Fatalf("replay leaked bob: %s", blob2)
	}
	if seen["alice-gap"] == 0 {
		t.Fatalf("replay missed gap: %s", blob2)
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate %s x%d", id, n)
		}
	}

	// Heartbeat.
	stream3, cancel3 := openPortalSSE(t, srv, cookies, "", "")
	defer cancel3()
	defer stream3.Body.Close()
	pings := readSSE(stream3.Body, func(fs []sseFrame) bool {
		for _, f := range fs {
			if f.Event == "ping" && f.Data == "ping" {
				return true
			}
		}
		return false
	}, time.Second)
	gotPing := false
	for _, f := range pings {
		if f.Event == "ping" && f.Data == "ping" {
			gotPing = true
		}
	}
	if !gotPing {
		t.Fatal("expected SSE comment heartbeat")
	}

	// Session revoke closes an already-open stream on the next re-auth tick.
	stream4, cancel4 := openPortalSSE(t, srv, cookies, "", "")
	defer cancel4()
	defer stream4.Body.Close()
	_ = svc.CloseSession(cookies[0].Value)
	expired := readSSE(stream4.Body, func(fs []sseFrame) bool {
		for _, f := range fs {
			if f.Event == "session" && strings.Contains(f.Data, "portal_session_expired") {
				return true
			}
		}
		return false
	}, time.Second)
	gotExp := false
	for _, f := range expired {
		if f.Event == "session" {
			gotExp = true
		}
	}
	if !gotExp {
		t.Fatal("expected session expiry event after CloseSession")
	}
}

func TestPortalSSEExpiredSessionReconnect(t *testing.T) {
	h, svc := testKeyHandler(t)
	_, sa, err := svc.Create(apikey.CreateInput{Name: "z", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(h.handlePortal))
	t.Cleanup(srv.Close)
	res, err := http.Post(srv.URL+"/portal/api/session", "application/json", strings.NewReader(`{"key":"`+sa+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	cookies := res.Cookies()
	_ = res.Body.Close()
	_ = svc.CloseSession(cookies[0].Value)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/portal/api/events/stream", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	out, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if out.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reconnect after revoke: %d", out.StatusCode)
	}
}

func TestPortalTokenRevokeKillsSSE(t *testing.T) {
	prev := portalSSEReauth
	portalSSEReauth = 30 * time.Millisecond
	t.Cleanup(func() { portalSSEReauth = prev })

	h, svc := testKeyHandler(t)
	rec, _, err := svc.Create(apikey.CreateInput{Name: "tok", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := svc.CreatePortalToken(rec.Key.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(h.handlePortal))
	t.Cleanup(srv.Close)
	res, err := http.Post(srv.URL+"/portal/api/session/token", "application/json", strings.NewReader(`{"token":"`+tok+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	cookies := res.Cookies()
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("token session %d", res.StatusCode)
	}

	stream, cancel := openPortalSSE(t, srv, cookies, "", "")
	defer cancel()
	defer stream.Body.Close()
	_ = svc.RevokePortalToken(rec.Key.ID)
	frames := readSSE(stream.Body, func(fs []sseFrame) bool {
		for _, f := range fs {
			if f.Event == "session" {
				return true
			}
		}
		return false
	}, time.Second)
	ok := false
	for _, f := range frames {
		if f.Event == "session" {
			ok = true
		}
	}
	if !ok {
		t.Fatal("revoked portal token left SSE open")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/portal/api/events/stream", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	out, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if out.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reconnect after token revoke: %d", out.StatusCode)
	}
}
