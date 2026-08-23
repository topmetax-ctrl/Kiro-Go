package providererr

import (
	"context"
	"net/http"
	"strings"
	"testing"
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
