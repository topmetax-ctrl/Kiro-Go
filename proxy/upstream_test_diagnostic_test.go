package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// The admin Test button is the panel's only way to ask "why did this target
// refuse". Before this, its answer was `{"ok":false,"status":400}` plus the raw
// body — a status code and an unexplained blob — so the two things an operator
// actually decides on were both missing: what CLASS of failure this is, and
// whether a real request would have failed over to the next target or been
// returned to the client.
//
// The contract pinned here is that Test reports the SAME classification the
// forward path applies to live traffic (providererr), and that the upstream's
// own words reach the panel only through the boundary's redactor.

func postProbe(t *testing.T, h *Handler, baseURL, model string) map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"baseUrl": baseURL, "model": model})
	rec := httptest.NewRecorder()
	h.apiUpstreamTest(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstream-test", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("probe HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return got
}

func probeServer(t *testing.T, status int, body string, hdr map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A 400 on a model name is REJECTED: the router returns it to the client instead
// of trying the next target. "HTTP 400" alone does not say that, and it is the
// difference between "fix this row" and "this row is covered by its fallback".
func TestUpstreamTestReportsRejectedClassification(t *testing.T) {
	url := probeServer(t, 400, `{"error":{"code":"model_not_found","message":"model glm-5.3-flash does not exist"}}`, nil)
	got := postProbe(t, &Handler{}, url, "glm-5.3-flash")

	if got["ok"] != false {
		t.Fatalf("ok = %v, want false", got["ok"])
	}
	if got["category"] != "rejected" {
		t.Fatalf("category = %v, want rejected", got["category"])
	}
	if got["retryable"] != false {
		t.Fatalf("retryable = %v, want false: a rejected request is not retried elsewhere", got["retryable"])
	}
	if got["code"] != "model_not_found" {
		t.Fatalf("code = %v, want the upstream's own error code", got["code"])
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "glm-5.3-flash") {
		t.Fatalf("message = %q, want the upstream's own words (which name the bad field)", msg)
	}
}

// A 429 is retryable and carries Retry-After. Both are what turn "failed" into
// "come back in 30 seconds", so both have to survive to the panel.
func TestUpstreamTestReportsRateLimitAndRetryAfter(t *testing.T) {
	url := probeServer(t, 429, `{"error":{"message":"too many requests"}}`, map[string]string{
		"Retry-After":  "30",
		"X-Request-Id": "req_upstream_9",
	})
	got := postProbe(t, &Handler{}, url, "m")

	if got["category"] != "rate_limited" {
		t.Fatalf("category = %v, want rate_limited", got["category"])
	}
	if got["retryable"] != true {
		t.Fatalf("retryable = %v, want true", got["retryable"])
	}
	if got["retryAfter"] != "30" {
		t.Fatalf("retryAfter = %v, want 30", got["retryAfter"])
	}
	if got["upstreamRequestId"] != "req_upstream_9" {
		t.Fatalf("upstreamRequestId = %v, want the upstream's id (what its support desk asks for)", got["upstreamRequestId"])
	}
}

// An upstream 401 must NOT be reported as a client-auth problem, matching the
// forward path: it is the stored key that is wrong, not the admin's session.
func TestUpstreamTestClassifiesUpstream401AsProviderError(t *testing.T) {
	url := probeServer(t, 401, `{"error":{"message":"invalid api key"}}`, nil)
	got := postProbe(t, &Handler{}, url, "m")

	if got["category"] != "error" {
		t.Fatalf("category = %v, want error", got["category"])
	}
	if got["retryable"] != false {
		t.Fatalf("retryable = %v, want false: retrying a bad credential just multiplies it", got["retryable"])
	}
	if got["status"] != float64(401) {
		t.Fatalf("status = %v, want the upstream's own 401 surfaced", got["status"])
	}
}

// A 503 is UNAVAILABLE, not a generic error: the router retries it on the next
// target, so the operator reading this row is looking at a transient outage
// rather than a misconfigured row.
func TestUpstreamTestClassifies503AsUnavailable(t *testing.T) {
	url := probeServer(t, 503, `<html><body>503 Service Unavailable</body></html>`, nil)
	got := postProbe(t, &Handler{}, url, "m")

	if got["category"] != "unavailable" {
		t.Fatalf("category = %v, want unavailable", got["category"])
	}
	if got["retryable"] != true {
		t.Fatalf("retryable = %v, want true", got["retryable"])
	}
}

// The reason body goes through the boundary's redactor. A gateway that echoes the
// request back — several do, in their 400s — would otherwise hand the panel the
// Authorization header we just sent it, and from there into whatever the operator
// pastes into a bug report.
func TestUpstreamTestRedactsEchoedCredentials(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateUpstreamConfig([]config.UpstreamProvider{{
		ID: "p1", Name: "echo", Enabled: true,
		ApiKey: "sk-FIXTUREnotarealkey000000001",
	}}, nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The shape that causes this: a gateway quoting the request it refused.
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","echo":{"headers":{"authorization":"` +
			r.Header.Get("Authorization") + `"}}}}`))
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"id": "p1", "baseUrl": srv.URL, "model": "m"})
	rec := httptest.NewRecorder()
	(&Handler{}).apiUpstreamTest(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstream-test", bytes.NewReader(body)))

	out := rec.Body.String()
	if strings.Contains(out, "sk-FIXTUREnotarealkey000000001") {
		t.Fatalf("probe response leaked the stored key: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("want the redaction marker, got %s", out)
	}
	// The rest of the body still has to arrive: redaction that ate the diagnostic
	// would defeat the point of showing it.
	if !strings.Contains(out, "bad request") {
		t.Fatalf("want the upstream's own message kept, got %s", out)
	}
}

// Nothing answered: there is no status and no body, so the only useful facts are
// the class and the transport KIND. "does not resolve" and "resolved and refused"
// are the same red row and completely different fixes.
func TestUpstreamTestReportsTransportKind(t *testing.T) {
	// Port 1 on the loopback: nothing listens, so this is a refused connection
	// rather than a DNS failure or a hang.
	got := postProbe(t, &Handler{}, "http://127.0.0.1:1", "m")

	if got["ok"] != false {
		t.Fatalf("ok = %v, want false", got["ok"])
	}
	if got["category"] != "unavailable" {
		t.Fatalf("category = %v, want unavailable", got["category"])
	}
	if got["kind"] != "refused" {
		t.Fatalf("kind = %v, want refused", got["kind"])
	}
	if _, ok := got["status"]; ok {
		t.Fatalf("a transport failure has no HTTP status, got %v", got["status"])
	}
}

// A 2xx says nothing to explain, so none of the diagnostic fields appear: the
// panel decides what to render from which fields EXIST, and a category on a
// healthy probe would open an empty "why" panel on a green row.
func TestUpstreamTestOmitsDiagnosticsOnSuccess(t *testing.T) {
	url := probeServer(t, 200, `{"id":"ok"}`, nil)
	got := postProbe(t, &Handler{}, url, "m")

	if got["ok"] != true {
		t.Fatalf("ok = %v, want true", got["ok"])
	}
	for _, k := range []string{"category", "error", "code", "message", "retryAfter", "kind"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%q must not be reported on a healthy probe, got %v", k, got[k])
		}
	}
}

// An empty error body is ABSENT, not "". The panel says "the upstream sent no
// error body" for the first and shows a body for the second, and a present-but-
// empty field would read as the upstream having explained itself.
func TestUpstreamTestOmitsEmptyErrorBody(t *testing.T) {
	url := probeServer(t, 500, ``, nil)
	got := postProbe(t, &Handler{}, url, "m")

	if _, ok := got["error"]; ok {
		t.Fatalf("empty body must be omitted, got %q", got["error"])
	}
	if got["category"] != "error" {
		t.Fatalf("category = %v, want error", got["category"])
	}
}
