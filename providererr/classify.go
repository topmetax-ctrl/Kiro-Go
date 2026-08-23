package providererr

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// FromHTTP builds an InternalError from an upstream HTTP response. Body is
// parsed once; secrets are redacted before Detail is populated.
func FromHTTP(status int, body []byte, hdr http.Header) InternalError {
	parsed := ParseBody(body)
	redacted, _ := BoundAndRedact(body)
	cat := categoryFromStatus(status, parsed.Code, parsed.Message)
	in := InternalError{
		UpstreamStatus:    status,
		UpstreamCode:      parsed.Code,
		UpstreamMessage:   Redact(parsed.Message),
		UpstreamRequestID: firstHeader(hdr, "X-Request-Id", "X-Request-ID", "Request-Id", "Cf-Ray"),
		RetryAfter:        firstHeader(hdr, "Retry-After"),
		Detail:            redacted.Text,
		DetailTruncated:   redacted.Truncated,
		Category:          cat,
		Retryable:         RetryableStatus(status) || cat == CategoryTimeout || cat == CategoryUnavailable || cat == CategoryRateLimited,
		Timestamp:         time.Now(),
	}
	if parsed.RequestID != "" && in.UpstreamRequestID == "" {
		in.UpstreamRequestID = parsed.RequestID
	}
	IncClassified(cat)
	if redacted.Truncated {
		IncTruncated()
	}
	return in
}

// FromNetwork classifies a transport/Go error that never produced an HTTP
// response. Typed checks come first; string matching is a last resort.
func FromNetwork(err error) InternalError {
	in := InternalError{Timestamp: time.Now(), Retryable: true}
	if err == nil {
		in.Category = CategoryInternal
		in.Retryable = false
		IncClassified(in.Category)
		return in
	}

	switch {
	case errors.Is(err, context.Canceled):
		in.Category = CategoryCanceled
		in.NetworkKind = "canceled"
		in.Retryable = false
	case errors.Is(err, context.DeadlineExceeded):
		in.Category = CategoryTimeout
		in.NetworkKind = "deadline"
	case isTimeout(err):
		in.Category = CategoryTimeout
		in.NetworkKind = "timeout"
	case isDNS(err):
		in.Category = CategoryUnavailable
		in.NetworkKind = "dns"
	case isTLS(err):
		in.Category = CategoryUnavailable
		in.NetworkKind = "tls"
	case isRefused(err):
		in.Category = CategoryUnavailable
		in.NetworkKind = "refused"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		in.Category = CategoryError
		in.NetworkKind = "eof"
	default:
		in.Category = CategoryUnavailable
		in.NetworkKind = "network"
	}

	bound, _ := BoundAndRedactString(err.Error())
	in.Detail = bound.Text
	in.DetailTruncated = bound.Truncated
	in.UpstreamMessage = in.Detail
	IncClassified(in.Category)
	return in
}

func categoryFromStatus(status int, code, message string) Category {
	switch status {
	case http.StatusTooManyRequests:
		return CategoryRateLimited
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return CategoryTimeout
	case http.StatusServiceUnavailable:
		return CategoryUnavailable
	case http.StatusUnauthorized, http.StatusForbidden:
		// Upstream credential/entitlement — never client auth.
		return CategoryError
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		return CategoryRejected
	case http.StatusInternalServerError, http.StatusBadGateway:
		return CategoryError
	}
	if status >= 500 {
		if status == 503 {
			return CategoryUnavailable
		}
		return CategoryError
	}
	if status >= 400 {
		return CategoryRejected
	}
	_ = code
	_ = message
	return CategoryError
}

// RetryableStatus is the existing forward-path retry policy: 429 and 5xx.
// 401/403 stay non-retryable so a bad credential is not multiplied.
func RetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

func isDNS(err error) bool {
	var dns *net.DNSError
	return errors.As(err, &dns)
}

func isTLS(err error) bool {
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		return true
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return true
	}
	var hostname x509.HostnameError
	return errors.As(err, &hostname)
}

func isRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		if errors.Is(op.Err, syscall.ECONNREFUSED) {
			return true
		}
	}
	return false
}

func firstHeader(h http.Header, names ...string) string {
	if h == nil {
		return ""
	}
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}
