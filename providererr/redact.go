package providererr

import (
	"regexp"
	"strings"
	"sync/atomic"
)

var (
	classifiedTotal [8]atomic.Int64
	persistFailed   atomic.Int64
	truncatedTotal  atomic.Int64
	redactionTotal  atomic.Int64
)

func categoryIndex(c Category) int {
	switch c {
	case CategoryRateLimited:
		return 0
	case CategoryTimeout:
		return 1
	case CategoryUnavailable:
		return 2
	case CategoryRejected:
		return 3
	case CategoryError:
		return 4
	case CategoryCanceled:
		return 5
	case CategoryInternal:
		return 6
	default:
		return 7
	}
}

func IncClassified(c Category) { classifiedTotal[categoryIndex(c)].Add(1) }
func IncPersistFailed()        { persistFailed.Add(1) }
func IncTruncated()            { truncatedTotal.Add(1) }
func IncRedacted()             { redactionTotal.Add(1) }

func IncRedactedIfChanged(before, after string) int {
	if before != after {
		redactionTotal.Add(1)
		return 1
	}
	return 0
}

func ClassifiedTotal(c Category) int64 { return classifiedTotal[categoryIndex(c)].Load() }
func PersistFailedTotal() int64        { return persistFailed.Load() }
func TruncatedTotal() int64            { return truncatedTotal.Load() }
func RedactionTotal() int64            { return redactionTotal.Load() }

// bearerRe matches Authorization-style tokens without a giant catch-all.
var (
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|token)\s+[a-z0-9._\-+=/]{8,}`)
	skRe     = regexp.MustCompile(`(?i)\bsk-[a-z0-9_\-]{8,}`)
	ptRe     = regexp.MustCompile(`(?i)\bpt-[a-z0-9_\-]{8,}`)
	apiKeyKV = regexp.MustCompile(`(?i)("?(?:authorization|proxy-authorization|api[_-]?key|apiKey|access_token|refresh_token|x-api-key|cookie)"?\s*[:=]\s*"?)([^"\s,}]+)`)
)

const redacted = "[REDACTED]"

// Redact strips credential-shaped values from diagnostic text. It is
// deliberately small: known header/token shapes, not a generic PII filter.
func Redact(s string) string {
	if s == "" {
		return s
	}
	out := bearerRe.ReplaceAllString(s, "$1 "+redacted)
	out = skRe.ReplaceAllString(out, redacted)
	out = ptRe.ReplaceAllString(out, redacted)
	out = apiKeyKV.ReplaceAllString(out, "${1}"+redacted)
	return out
}

// LooksLikeCredential reports whether s still contains an obvious secret shape.
func LooksLikeCredential(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "bearer ") && !strings.Contains(lower, "bearer "+strings.ToLower(redacted)) {
		if bearerRe.MatchString(s) {
			return true
		}
	}
	if skRe.MatchString(s) || ptRe.MatchString(s) {
		return true
	}
	return false
}
