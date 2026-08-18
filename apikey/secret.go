package apikey

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Digest returns hex(HMAC-SHA256(pepper, secret)). The pepper is the server
// secret; the API key is the message. Empty pepper is rejected by Open, not here.
func Digest(secret string, pepper []byte) string {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	return hex.EncodeToString(mac.Sum(nil))
}

// GenerateSecret matches the historical config.GenerateApiKeyValue format
// (sk- + 32 CSPRNG bytes hex) so existing clients and docs stay valid.
func GenerateSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is unrecoverable on a healthy kernel; return a
		// non-empty impossible value rather than panic in a library path.
		return ""
	}
	return "sk-" + hex.EncodeToString(buf)
}

// GeneratePortalToken returns a capability token that is not a valid API key.
func GeneratePortalToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return "pt-" + hex.EncodeToString(buf)
}

// GenerateSessionID returns an opaque portal session secret.
func GenerateSessionID() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// SplitDisplay returns the prefix (first 6) and last 4 of a secret for masking.
func SplitDisplay(secret string) (prefix, last4 string) {
	if secret == "" {
		return "", ""
	}
	if len(secret) >= 6 {
		prefix = secret[:6]
	} else {
		prefix = secret
	}
	if len(secret) >= 4 {
		last4 = secret[len(secret)-4:]
	} else {
		last4 = secret
	}
	return prefix, last4
}

// MaskFromParts builds the admin/portal display form without needing plaintext.
func MaskFromParts(prefix, last4 string) string {
	if prefix == "" && last4 == "" {
		return ""
	}
	return prefix + "••••••••" + last4
}

// MaskSecret is used only at create/rotate time when plaintext is still in hand.
func MaskSecret(secret string) string {
	p, l := SplitDisplay(secret)
	return MaskFromParts(p, l)
}

func normalizePolicy(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case ResetDaily, ResetWeekly, ResetMonthly:
		return strings.ToLower(strings.TrimSpace(p))
	default:
		return ResetLifetime
	}
}

func normalizeEnforcement(p string) string {
	if strings.ToLower(strings.TrimSpace(p)) == EnforceStrict {
		return EnforceStrict
	}
	return EnforceSoft
}
