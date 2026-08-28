package providererr

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseBodyOpenAI(t *testing.T) {
	p := ParseBody([]byte(`{"error":{"code":"ACCOUNT_SUSPENDED","message":"account abc suspended"}}`))
	if p.Code != "ACCOUNT_SUSPENDED" || !strings.Contains(p.Message, "account abc") {
		t.Fatalf("%+v", p)
	}
}

func TestParseBodyGenericAndHTML(t *testing.T) {
	p := ParseBody([]byte(`{"message":"Quota exceeded","code":"quota"}`))
	if p.Code != "quota" || p.Message != "Quota exceeded" {
		t.Fatalf("%+v", p)
	}
	h := ParseBody([]byte("<html><body>502 Bad Gateway</body></html>"))
	if h.Message != "502 bad gateway" {
		t.Fatalf("html: %+v", h)
	}
}

func TestRedactBearerAndAPIKey(t *testing.T) {
	in := `Authorization: Bearer SECRET_TOKEN_CANARY api_key=sk-abc123456789`
	out := Redact(in)
	if strings.Contains(out, "SECRET_TOKEN_CANARY") || strings.Contains(out, "sk-abc123456789") {
		t.Fatalf("leaked: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected marker: %s", out)
	}
}

func TestFromHTTP401IsNotClientAuth(t *testing.T) {
	in := FromHTTP(401, []byte(`{"error":{"message":"invalid API key"}}`), nil)
	pub := in.Public()
	if pub.HTTPStatus == 401 {
		t.Fatal("upstream 401 must not become client 401")
	}
	if pub.Code != CodeError {
		t.Fatalf("code=%s", pub.Code)
	}
	if pub.Retryable {
		t.Fatal("upstream 401 must not be retryable for clients")
	}
	if strings.Contains(pub.Message, "invalid API key") {
		t.Fatalf("public leaked: %s", pub.Message)
	}
}

func TestFromHTTP429(t *testing.T) {
	in := FromHTTP(429, []byte(`{"error":{"message":"rate limited"}}`), http.Header{"Retry-After": []string{"7"}})
	pub := in.Public()
	if pub.Code != CodeRateLimited || pub.HTTPStatus != 429 {
		t.Fatalf("%+v", pub)
	}
	if in.RetryAfter != "7" {
		t.Fatalf("retry-after=%s", in.RetryAfter)
	}
}

func TestFromNetworkTimeout(t *testing.T) {
	in := FromNetwork(context.DeadlineExceeded)
	if in.Category != CategoryTimeout {
		t.Fatalf("cat=%s", in.Category)
	}
	pub := in.Public()
	if pub.Code != CodeTimeout || pub.HTTPStatus != 504 {
		t.Fatalf("%+v", pub)
	}
}

func TestSSERewriteHidesCanary(t *testing.T) {
	frame := []byte("event: error\ndata: {\"error\":{\"message\":\"UPSTREAM_SECRET_CANARY_12345\"}}\n\n")
	pub := PublicError{Code: CodeError, Message: MsgError, RequestID: "req_1"}
	out, ok := RewriteSSEFrame(frame, pub, DialectClaude)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if strings.Contains(string(out), "UPSTREAM_SECRET_CANARY_12345") {
		t.Fatalf("canary leaked: %s", out)
	}
	if !strings.Contains(string(out), MsgError) {
		t.Fatalf("missing public message: %s", out)
	}
}

func TestSSEPassThroughContent(t *testing.T) {
	frame := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n")
	out, ok := RewriteSSEFrame(frame, PublicError{Code: CodeError}, DialectClaude)
	if ok {
		t.Fatal("content frame rewritten")
	}
	if string(out) != string(frame) {
		t.Fatalf("mutated: %s", out)
	}
}

func TestBoundAndRedactTruncates(t *testing.T) {
	big := strings.Repeat("x", MaxDetailBytes+100)
	got, _ := BoundAndRedactString(big)
	if !got.Truncated || len(got.Text) > MaxDetailBytes {
		t.Fatalf("trunc=%v len=%d", got.Truncated, len(got.Text))
	}
}

// Diagnose* is the UNMETERED twin of From*: the admin Test button runs the same
// classification, and an operator clicking Test twelve times must not read back as
// twelve upstream failures on the dashboards.
func TestDiagnoseDoesNotTouchCounters(t *testing.T) {
	before := [...]int64{
		ClassifiedTotal(CategoryRejected),
		ClassifiedTotal(CategoryUnavailable),
		RedactionTotal(),
		TruncatedTotal(),
	}

	d := DiagnoseHTTP(400, []byte(`{"error":{"code":"model_not_found","message":"no such model, key sk-abcdefghijkl"}}`), nil)
	if d.Category != CategoryRejected || d.Retryable {
		t.Fatalf("%+v", d)
	}
	if strings.Contains(d.Detail, "sk-abcdefghijkl") {
		t.Fatalf("diagnostic leaked a key: %s", d.Detail)
	}
	if DiagnoseError(context.DeadlineExceeded).Category != CategoryTimeout {
		t.Fatal("timeout misclassified")
	}

	after := [...]int64{
		ClassifiedTotal(CategoryRejected),
		ClassifiedTotal(CategoryUnavailable),
		RedactionTotal(),
		TruncatedTotal(),
	}
	if before != after {
		t.Fatalf("Diagnose moved the counters: %v -> %v", before, after)
	}
}

// FromHTTP still IS metered — the pure path must not have quietly disarmed it.
func TestFromHTTPStillCounts(t *testing.T) {
	before := ClassifiedTotal(CategoryRateLimited)
	redactedBefore := RedactionTotal()
	FromHTTP(429, []byte(`{"error":{"message":"slow down, token sk-abcdefghijkl"}}`), nil)
	if ClassifiedTotal(CategoryRateLimited) != before+1 {
		t.Fatalf("classification not counted: %d -> %d", before, ClassifiedTotal(CategoryRateLimited))
	}
	if RedactionTotal() != redactedBefore+1 {
		t.Fatalf("redaction not counted: %d -> %d", redactedBefore, RedactionTotal())
	}
}

// An outbound proxy is configured as http://user:pass@host, and a dial failure
// carries that URL verbatim. The host stays (it is the useful half); the
// credential does not.
func TestRedactProxyURLUserinfo(t *testing.T) {
	out := Redact(`Post "https://api.example/v1/chat/completions": proxyconnect tcp: dial http://bob:hunter2@10.0.0.9:8080`)
	if strings.Contains(out, "hunter2") {
		t.Fatalf("proxy password leaked: %s", out)
	}
	if !strings.Contains(out, "10.0.0.9:8080") {
		t.Fatalf("proxy host must survive — it is what the operator needs: %s", out)
	}
}

// A body cut at the storage cap must not end mid-character: this text is rendered
// later, and a sliced rune shows up as a replacement glyph on every long
// diagnostic.
func TestBoundAndRedactKeepsRunesWhole(t *testing.T) {
	body := strings.Repeat("好", MaxDetailBytes) // 3 bytes each, so the cap lands mid-rune
	got, _ := BoundAndRedactString(body)
	if !got.Truncated {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(got.Text) {
		t.Fatal("truncated text ends mid-rune")
	}
	if len(got.Text) > MaxDetailBytes {
		t.Fatalf("over cap: %d", len(got.Text))
	}
}

// Upstream error envelopes are not consistent about the type of "code": several
// real gateways send it as a NUMBER where OpenAI sends a string. Because the whole
// envelope was decoded in one pass, one numeric field used to cost the message too
// — the strict decode failed, the parse fell through to the plain-text branch, and
// the panel showed the entire raw JSON body as the "message" with no code at all.
func TestParseBodyToleratesNumericCode(t *testing.T) {
	p := ParseBody([]byte(`{"error": {"code": 401, "message": "API key rejected", "type": "new_api_error"}}`))
	if p.Code != "401" {
		t.Fatalf("Code = %q, want the numeric code rendered as text", p.Code)
	}
	if p.Message != "API key rejected" {
		t.Fatalf("Message = %q — a wrong type on `code` must not cost the message beside it", p.Message)
	}
}

// The string form must keep working unchanged, and `type` must still be the
// fallback when no code is sent at all.
func TestParseBodyStringCodeAndTypeFallback(t *testing.T) {
	p := ParseBody([]byte(`{"error": {"code": "model_not_found", "message": "no such model"}}`))
	if p.Code != "model_not_found" || p.Message != "no such model" {
		t.Fatalf("%+v", p)
	}
	q := ParseBody([]byte(`{"error": {"message": "bad", "type": "invalid_request_error"}}`))
	if q.Code != "invalid_request_error" {
		t.Fatalf("Code = %q, want the type as fallback", q.Code)
	}
}

// A shape nobody predicted degrades to empty rather than rendering "{...}" into a
// field labelled "code" — and the readable fields around it still survive.
func TestParseBodyIgnoresNonScalarCode(t *testing.T) {
	p := ParseBody([]byte(`{"error": {"code": {"weird": 1}, "message": "still readable"}}`))
	if p.Code != "" {
		t.Fatalf("Code = %q, want empty for a non-scalar", p.Code)
	}
	if p.Message != "still readable" {
		t.Fatalf("Message = %q", p.Message)
	}
}

// The classification must not depend on the code's JSON type either: a numeric
// code on a 401 is still an upstream credential problem, non-retryable.
func TestFromHTTPNumericCodeStillClassifies(t *testing.T) {
	in := FromHTTP(401, []byte(`{"error": {"code": 401, "message": "API key rejected"}}`), nil)
	if in.Category != CategoryError || in.Retryable {
		t.Fatalf("%+v", in)
	}
	if in.UpstreamCode != "401" || in.UpstreamMessage != "API key rejected" {
		t.Fatalf("code=%q msg=%q", in.UpstreamCode, in.UpstreamMessage)
	}
}
