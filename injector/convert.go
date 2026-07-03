package injector

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// msToISO converts a Unix MILLISECONDS timestamp to the ISO-8601 / RFC3339-ish
// string Kiro IDE writes in kiro-auth-token.json, e.g. "2026-05-14T07:29:35.000Z".
// A zero or negative input yields an empty string (caller decides a fallback).
func msToISO(ms int64) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).UTC()
	return t.Format("2006-01-02T15:04:05.000Z")
}

// msToRFC3339 converts Unix MILLISECONDS to the RFC3339 format the Amazon Q CLI
// stores (time::serde::rfc3339), e.g. "2026-05-14T07:29:35Z".
func msToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// sha1Hex returns the hex-encoded SHA-1 of clientId. This is the filename Kiro
// IDE / AWS SSO use for the OIDC client registration file ({hash}.json), as
// confirmed against the Kiro IDE on-disk layout.
func sha1Hex(clientID string) string {
	sum := sha1.Sum([]byte(clientID))
	return hex.EncodeToString(sum[:])
}

// regExpiryFromSecret decodes an OIDC clientSecret (a JWT) and pulls the
// embedded serialized.expirationTimestamp (Unix seconds) to use as the
// registration file's expiresAt. Falls back to now+80d if it can't be decoded,
// matching the behavior of the reference Kiro swapper tooling.
func regExpiryFromSecret(clientSecret string) string {
	fallback := time.Now().UTC().Add(80 * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	parts := strings.Split(clientSecret, ".")
	if len(parts) < 2 {
		return fallback
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return fallback
	}
	var outer struct {
		Serialized json.RawMessage `json:"serialized"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return fallback
	}
	// "serialized" may be a JSON string containing JSON, or a nested object.
	var inner struct {
		ExpirationTimestamp float64 `json:"expirationTimestamp"`
	}
	ser := outer.Serialized
	if len(ser) > 0 && ser[0] == '"' {
		var s string
		if json.Unmarshal(ser, &s) == nil {
			_ = json.Unmarshal([]byte(s), &inner)
		}
	} else {
		_ = json.Unmarshal(ser, &inner)
	}
	if inner.ExpirationTimestamp > 0 {
		return time.Unix(int64(inner.ExpirationTimestamp), 0).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return fallback
}

// KiroIDEToken is the on-disk shape of ~/.aws/sso/cache/kiro-auth-token.json.
// Fields are emitted only when set (omitempty) so social/external_idp files
// don't carry IdC-only keys. The field names/casing mirror what the Kiro IDE
// itself writes (verified via auth/local_cache.go rawTokenFile).
type KiroIDEToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	ProfileArn   string `json:"profileArn,omitempty"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider,omitempty"`
	Region       string `json:"region,omitempty"`
	ClientIDHash string `json:"clientIdHash,omitempty"`
	StartURL     string `json:"startUrl,omitempty"`
	// External IdP (Microsoft Entra etc.) fields.
	IssuerURL   string `json:"issuerUrl,omitempty"`
	IdPClientID string `json:"idpClientId,omitempty"`
	Scopes      string `json:"scopes,omitempty"`
	LoginHint   string `json:"loginHint,omitempty"`
}

// clientRegistration is the {clientIdHash}.json file content for IdC accounts.
type clientRegistration struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	ExpiresAt    string `json:"expiresAt"`
}

// buildKiroIDEToken converts an export account into the IDE token file plus,
// for IdC accounts, the side registration file (hashName -> content). reg is nil
// for social / external_idp accounts.
func buildKiroIDEToken(a ExportAccount) (tok KiroIDEToken, regName string, reg *clientRegistration) {
	c := a.Credentials
	method := a.NormalizedAuthMethod()

	expISO := msToISO(c.ExpiresAt)
	if expISO == "" {
		// Unknown expiry: set in the past so the IDE refreshes immediately.
		expISO = time.Now().UTC().Add(-time.Minute).Format("2006-01-02T15:04:05.000Z")
	}

	region := c.Region
	if region == "" {
		region = "us-east-1"
	}

	tok = KiroIDEToken{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		ExpiresAt:    expISO,
		ProfileArn:   c.ProfileArn,
		Provider:     c.Provider,
	}

	switch method {
	case "idc":
		tok.AuthMethod = "IdC"
		tok.Region = region
		if tok.Provider == "" {
			tok.Provider = "Enterprise"
		}
		if c.ClientID != "" && c.ClientSecret != "" {
			hash := sha1Hex(c.ClientID)
			tok.ClientIDHash = hash
			regName = hash + ".json"
			reg = &clientRegistration{
				ClientID:     c.ClientID,
				ClientSecret: c.ClientSecret,
				ExpiresAt:    regExpiryFromSecret(c.ClientSecret),
			}
		}
	case "external_idp":
		tok.AuthMethod = "external_idp"
		tok.Region = region
		if tok.Provider == "" {
			tok.Provider = "MicrosoftEntra"
		}
		tok.IssuerURL = c.IssuerURL
		tok.IdPClientID = c.IdPClientID
		tok.Scopes = c.Scopes
		tok.LoginHint = c.LoginHint
	default: // social
		tok.AuthMethod = "social"
		if tok.Provider == "" {
			tok.Provider = "Google"
		}
	}
	return tok, regName, reg
}

// --- kiro-cli SQLite auth_kv / state values ---
//
// kiro-cli (the rebranded Amazon Q Developer CLI) stores credentials under the
// "kirocli:" key prefix — NOT "codewhisperer:" (that is the upstream Amazon Q
// CLI prefix). Verified against an installed kiro-cli 2.10.0 binary: the keys
// are kirocli:social:token, kirocli:odic:token, kirocli:odic:device-registration
// and kirocli:external-idp:token.
//
// The injection strategy uses the SOCIAL token shape for every injectable
// account, deliberately — the same approach the production agent-vibes exporter
// settled on. Writing an IdC-shaped (kirocli:odic:token) row makes `whoami` look
// signed in, but the backend then rejects chat. The social shape carries the
// profileArn inline and is the one shape the CLI accepts end-to-end for an
// externally minted token. A matching profile row in the `state` table
// (api.codewhisperer.profile) is required alongside it.
const (
	cliSocialTokenKey  = "kirocli:social:token"
	cliProfileStateKey = "api.codewhisperer.profile"
)

// cliCompetingTokenKeys are the other auth_kv token keys that must be cleared so
// the CLI unambiguously uses the social token we inject.
var cliCompetingTokenKeys = []string{
	"kirocli:odic:token",
	"kirocli:odic:device-registration",
	"kirocli:external-idp:token",
}

// cliSocialToken is the JSON shape stored at kirocli:social:token. The CLI reads
// access_token / refresh_token / expires_at to authenticate and refresh, and
// profile_arn to target the data plane.
type cliSocialToken struct {
	AccessToken  string `json:"access_token"`
	ExpiresAt    string `json:"expires_at"` // RFC3339
	RefreshToken string `json:"refresh_token"`
	Provider     string `json:"provider"` // "google" | "github"
	ProfileArn   string `json:"profile_arn"`
}

// cliProfile is the JSON shape stored at state/api.codewhisperer.profile.
type cliProfile struct {
	Arn         string `json:"arn"`
	ProfileName string `json:"profile_name"`
}

// normalizeCLISocialProvider maps any provider onto the two values the CLI's
// social path understands. Anything that isn't GitHub falls back to "google".
func normalizeCLISocialProvider(provider string) string {
	if strings.EqualFold(strings.TrimSpace(provider), "github") {
		return "github"
	}
	return "google"
}

// buildCLISocialToken builds the kirocli:social:token JSON value. profileArn is
// required (the CLI treats a social token with no profile ARN as invalid).
func buildCLISocialToken(a ExportAccount) (string, error) {
	c := a.Credentials
	if strings.TrimSpace(c.ProfileArn) == "" {
		return "", fmt.Errorf("account has no profileArn; kiro-cli requires it")
	}
	exp := msToRFC3339(c.ExpiresAt)
	if exp == "" {
		exp = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	}
	tok := cliSocialToken{
		AccessToken:  c.AccessToken,
		ExpiresAt:    exp,
		RefreshToken: c.RefreshToken,
		Provider:     normalizeCLISocialProvider(c.Provider),
		ProfileArn:   c.ProfileArn,
	}
	b, err := json.Marshal(tok)
	if err != nil {
		return "", fmt.Errorf("marshal cli social token: %w", err)
	}
	return string(b), nil
}

// buildCLIProfile builds the state/api.codewhisperer.profile JSON value.
func buildCLIProfile(a ExportAccount) (string, error) {
	arn := strings.TrimSpace(a.Credentials.ProfileArn)
	if arn == "" {
		return "", fmt.Errorf("account has no profileArn; kiro-cli requires it")
	}
	b, err := json.Marshal(cliProfile{Arn: arn, ProfileName: "Social_Default_Profile"})
	if err != nil {
		return "", fmt.Errorf("marshal cli profile: %w", err)
	}
	return string(b), nil
}
