package providererr

import "net/http"

// Diagnostic is the admin-only, redacted view of ONE upstream failure for paths
// that PROBE an upstream instead of serving a client request: the admin Test
// button on a provider, a key, or a route target.
//
// Two properties are the reason it exists as its own type rather than reusing
// InternalError. It runs the same parse/classify/redact machinery the forward
// path runs, so what Test reports is what production would have classified — a
// second, hand-rolled error shape for the admin panel is exactly how the two
// drift apart. And it is metric-free: the classification counters measure what
// real traffic hit, so an operator clicking Test twelve times must not read back
// as twelve upstream failures.
type Diagnostic struct {
	Category Category
	// Kind names a transport failure (dns, tls, refused, timeout, eof…). Empty
	// when the upstream answered, since then the status carries that meaning.
	Kind       string
	Code       string
	Message    string
	RequestID  string
	RetryAfter string
	// Detail is the redacted, size-capped upstream body (or error text). It is
	// admin-only: it may name the upstream host, its proxy, and its internals.
	Detail    string
	Truncated bool
	Retryable bool
}

// MessageOrKind is the shortest honest one-liner for this failure: the upstream's
// own message when it sent one, else the transport kind, else the class.
func (d Diagnostic) MessageOrKind() string {
	switch {
	case d.Message != "":
		return d.Message
	case d.Kind != "":
		return d.Kind
	default:
		return string(d.Category)
	}
}

// DiagnoseHTTP classifies an upstream HTTP answer without recording metrics.
func DiagnoseHTTP(status int, body []byte, hdr http.Header) Diagnostic {
	d, _ := diagnoseHTTP(status, body, hdr)
	return d
}

// DiagnoseError classifies a transport/Go failure without recording metrics.
func DiagnoseError(err error) Diagnostic {
	d, _ := diagnoseError(err)
	return d
}

// diagnoseHTTP is the shared core. The bool reports whether redaction actually
// removed something, so the metered caller (FromHTTP) can count it without
// paying for a second redaction pass.
func diagnoseHTTP(status int, body []byte, hdr http.Header) (Diagnostic, bool) {
	parsed := ParseBody(body)
	raw := string(body)
	redacted := Redact(raw)
	detail, truncated := boundRedacted(redacted, len(raw))
	cat := categoryFromStatus(status, parsed.Code, parsed.Message)
	d := Diagnostic{
		Category:   cat,
		Code:       parsed.Code,
		Message:    Redact(parsed.Message),
		RequestID:  firstHeader(hdr, "X-Request-Id", "X-Request-ID", "Request-Id", "Cf-Ray"),
		RetryAfter: firstHeader(hdr, "Retry-After"),
		Detail:     detail,
		Truncated:  truncated,
		Retryable:  retryableHTTP(status, cat),
	}
	if parsed.RequestID != "" && d.RequestID == "" {
		d.RequestID = parsed.RequestID
	}
	return d, redacted != raw
}

func diagnoseError(err error) (Diagnostic, bool) {
	if err == nil {
		return Diagnostic{Category: CategoryInternal}, false
	}
	cat, kind := CategoryForError(err)
	raw := err.Error()
	redacted := Redact(raw)
	detail, truncated := boundRedacted(redacted, len(raw))
	return Diagnostic{
		Category:  cat,
		Kind:      kind,
		Message:   detail,
		Detail:    detail,
		Truncated: truncated,
		Retryable: RetryableCategory(cat),
	}, redacted != raw
}

// retryableHTTP is the forward path's retry rule, kept in one place so a probe
// and a real request never disagree about whether a status is worth retrying.
func retryableHTTP(status int, cat Category) bool {
	return RetryableStatus(status) || cat == CategoryTimeout || cat == CategoryUnavailable || cat == CategoryRateLimited
}
