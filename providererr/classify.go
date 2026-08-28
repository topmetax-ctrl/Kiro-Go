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
//
// This is the METERED entry point: it records the classification counters. The
// same work without metrics is DiagnoseHTTP, which admin probes use.
func FromHTTP(status int, body []byte, hdr http.Header) InternalError {
	d, redactedSomething := diagnoseHTTP(status, body, hdr)
	in := InternalError{
		UpstreamStatus:    status,
		UpstreamCode:      d.Code,
		UpstreamMessage:   d.Message,
		UpstreamRequestID: d.RequestID,
		RetryAfter:        d.RetryAfter,
		Detail:            d.Detail,
		DetailTruncated:   d.Truncated,
		Category:          d.Category,
		Retryable:         d.Retryable,
		Timestamp:         time.Now(),
	}
	IncClassified(d.Category)
	if redactedSomething {
		IncRedacted()
	}
	if d.Truncated {
		IncTruncated()
	}
	return in
}

// FromNetwork classifies a transport/Go error that never produced an HTTP
// response. Typed checks come first; string matching is a last resort.
//
// Metered, like FromHTTP; DiagnoseError is the unmetered twin.
func FromNetwork(err error) InternalError {
	if err == nil {
		in := InternalError{Timestamp: time.Now(), Category: CategoryInternal}
		IncClassified(in.Category)
		return in
	}
	d, redactedSomething := diagnoseError(err)
	in := InternalError{
		Category:        d.Category,
		NetworkKind:     d.Kind,
		UpstreamMessage: d.Message,
		Detail:          d.Detail,
		DetailTruncated: d.Truncated,
		Retryable:       d.Retryable,
		Timestamp:       time.Now(),
	}
	IncClassified(d.Category)
	if redactedSomething {
		IncRedacted()
	}
	return in
}

// CategoryForError classifies a transport/Go error and names the failure kind,
// without touching the classification counters.
//
// The counters exist to measure what real traffic hit, so anything that
// classifies OUTSIDE the forward path — an operator pressing Test, a diagnostic
// probe — has to be able to reuse this table without inflating them. FromNetwork
// is the metered entry point; this is the pure one.
func CategoryForError(err error) (Category, string) {
	switch {
	case err == nil:
		return CategoryInternal, ""
	case errors.Is(err, context.Canceled):
		return CategoryCanceled, "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return CategoryTimeout, "deadline"
	case isTimeout(err):
		return CategoryTimeout, "timeout"
	case isDNS(err):
		return CategoryUnavailable, "dns"
	case isTLS(err):
		return CategoryUnavailable, "tls"
	case isRefused(err):
		return CategoryUnavailable, "refused"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return CategoryError, "eof"
	default:
		return CategoryUnavailable, "network"
	}
}

// CategoryForStatus classifies an upstream HTTP status without recording
// metrics. Same relationship to FromHTTP that CategoryForError has to
// FromNetwork.
func CategoryForStatus(status int) Category {
	return categoryFromStatus(status, "", "")
}

// RetryableCategory is whether a transport-level classification is worth another
// attempt. Everything except a client cancel is: the request never reached a
// decision, so a different account or endpoint may still answer it.
func RetryableCategory(cat Category) bool {
	return cat != CategoryCanceled && cat != CategoryInternal
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
