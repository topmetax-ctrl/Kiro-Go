package proxy

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// Account injection into the local Kiro IDE token cache.
//
// The Kiro IDE reads ~/.aws/sso/cache/kiro-auth-token.json on every launch. By
// writing a token file reconstructed from a Kiro-Go account, the IDE logs in as
// that account without a browser flow. When Kiro-Go runs in Docker this only
// works if the host's cache dir is bind-mounted into the container and exposed
// via the KIRO_INJECT_CACHE_DIR env var — the container cannot otherwise reach
// the host filesystem, nor can it relaunch the host's Kiro.app (PID/namespace
// isolation), so the user restarts Kiro themselves after injecting.
//
// kiro-cli is intentionally NOT handled here: Microsoft (external_idp) accounts
// fail on the CLI's data plane (it omits the TokenType: EXTERNAL_IDP header the
// backend requires), and the standalone cmd/inject tool covers the CLI for the
// social/IdC cases. This server path is IDE-only and dependency-free.

// injectCacheDirEnv names the env var that points at the Kiro IDE SSO cache dir.
// In Docker this is the in-container path of the bind-mounted host cache dir.
const injectCacheDirEnv = "KIRO_INJECT_CACHE_DIR"

// injectProfileDirEnv names the env var that points at the Kiro IDE
// globalStorage dir holding profile.json (the active-profile pointer). In Docker
// this is the in-container path of the bind-mounted host globalStorage dir. When
// unset, injection still writes the token file but skips profile.json (the IDE
// self-heals the profile only when its arn is missing, not when it is stale).
const injectProfileDirEnv = "KIRO_INJECT_PROFILE_DIR"

const kiroTokenFileName = "kiro-auth-token.json"

const kiroProfileFileName = "profile.json"

// injectCacheDir resolves where to write the token file. The env override wins
// (set it to the bind-mount target in Docker); otherwise the host default
// ~/.aws/sso/cache is used (for a natively-run server).
func injectCacheDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv(injectCacheDirEnv)); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".aws", "sso", "cache"), nil
}

// injectProfileDir resolves where to write profile.json. The env override wins
// (set it to the bind-mount target in Docker). When unset it returns "" — the
// caller then skips the profile write rather than guessing a host path it can't
// reach from a container.
func injectProfileDir() string {
	return strings.TrimSpace(os.Getenv(injectProfileDirEnv))
}

// kiroProfile is the shape of profile.json.
type kiroProfile struct {
	Arn  string `json:"arn"`
	Name string `json:"name"`
}

// regionFromInjectProfileArn extracts the AWS region from a CodeWhisperer profile
// ARN ("arn:aws:codewhisperer:<region>:<account>:profile/<id>"). Returns "" when
// the ARN is empty or malformed.
func regionFromInjectProfileArn(profileArn string) string {
	arn := strings.TrimSpace(profileArn)
	if arn == "" {
		return ""
	}
	parts := strings.Split(arn, ":")
	if len(parts) < 4 || parts[0] != "arn" || parts[2] != "codewhisperer" {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

// writeInjectProfile writes profile.json so the IDE's active profile points at
// the account's profileArn. This is required because the IDE reads the active
// profileArn from profile.json in its globalStorage, NOT from the token file,
// and its ProfileArnGuard only self-heals when the arn is MISSING — a stale arn
// left by a previous login is never validated against the current token, so
// switching to an account on a different backend silently 403s usage/model-list
// calls. Returns "" (no path, no error) when profileDir is unset or the account
// has no profileArn to pin.
func writeInjectProfile(profileDir, profileArn string) (string, error) {
	arn := strings.TrimSpace(profileArn)
	if profileDir == "" || arn == "" {
		return "", nil
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return "", fmt.Errorf("create profile dir: %w", err)
	}
	region := regionFromInjectProfileArn(arn)
	if region == "" {
		region = "us-east-1"
	}
	b, err := json.MarshalIndent(kiroProfile{Arn: arn, Name: "KiroProfile-" + region}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal profile: %w", err)
	}
	path := filepath.Join(profileDir, kiroProfileFileName)
	// profile.json is not secret (holds only an ARN); the IDE writes it 0644.
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", kiroProfileFileName, err)
	}
	return path, nil
}

// injectAvailable reports whether injection is usable on this deployment. It is
// available when the cache dir is explicitly configured (env) or already exists
// on disk. This lets the UI hide the feature on containerized deployments that
// have not bind-mounted a cache dir.
func injectAvailable() (bool, string) {
	if v := strings.TrimSpace(os.Getenv(injectCacheDirEnv)); v != "" {
		return true, v
	}
	dir, err := injectCacheDir()
	if err != nil {
		return false, ""
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return true, dir
	}
	return false, dir
}

// kiroIDEToken is the on-disk shape of kiro-auth-token.json. Fields are emitted
// only when set so social/external_idp files don't carry IdC-only keys. Casing
// mirrors what the Kiro IDE itself writes (auth/local_cache.go rawTokenFile).
type kiroIDEToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	ProfileArn   string `json:"profileArn,omitempty"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider,omitempty"`
	Region       string `json:"region,omitempty"`
	ClientIDHash string `json:"clientIdHash,omitempty"`
	StartURL     string `json:"startUrl,omitempty"`
	IssuerURL    string `json:"issuerUrl,omitempty"`
	// Kiro IDE refresh destructures token.clientId (verified: idpClientId occurs
	// 0 times in the 0.12.263/0.12.333 bundle). Emitting "idpClientId" here makes
	// the IDE read clientId=undefined and fail refresh with
	// `"clientId" must be a non-empty string`, surfacing as "Auth provider:
	// unexpected issue". The native external-IdP login writes "clientId".
	IdPClientID string `json:"clientId,omitempty"`
	Scopes      string `json:"scopes,omitempty"`
	LoginHint   string `json:"loginHint,omitempty"`
}

// clientRegistration is the {clientIdHash}.json side file for IdC accounts.
type clientRegistration struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	ExpiresAt    string `json:"expiresAt"`
}

// secToISO converts a Unix SECONDS timestamp (config.Account.ExpiresAt) to the
// ISO-8601 string the Kiro IDE writes, e.g. "2026-05-14T07:29:35.000Z".
func secToISO(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// sha1Hex returns the hex SHA-1 of clientId — the OIDC client registration
// filename ({hash}.json) Kiro IDE / AWS SSO use.
func sha1Hex(clientID string) string {
	sum := sha1.Sum([]byte(clientID))
	return hex.EncodeToString(sum[:])
}

// regExpiryFromSecret decodes an OIDC clientSecret (a JWT) and pulls the
// embedded serialized.expirationTimestamp (Unix seconds) for the registration
// file's expiresAt. Falls back to now+80d if it can't be decoded.
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

// normalizeInjectAuthMethod maps a config account onto idc / social /
// external_idp using the same precedence as the importer: IdP signals win.
func normalizeInjectAuthMethod(a config.Account) string {
	am := strings.ToLower(strings.TrimSpace(a.AuthMethod))
	if a.IssuerURL != "" || a.IdPClientID != "" ||
		strings.Contains(strings.ToLower(a.Provider), "entra") ||
		strings.Contains(strings.ToLower(a.Provider), "microsoft") {
		return "external_idp"
	}
	switch am {
	case "external_idp", "externalidp":
		return "external_idp"
	case "idc", "builderid", "enterprise", "idcenter":
		return "idc"
	case "social", "google", "github":
		return "social"
	}
	if a.ClientID != "" && a.ClientSecret != "" {
		return "idc"
	}
	return "social"
}

// buildInjectToken converts a config account into the IDE token file plus, for
// IdC accounts, the side registration file (regName -> content). reg is nil for
// social / external_idp.
func buildInjectToken(a config.Account) (tok kiroIDEToken, regName string, reg *clientRegistration) {
	method := normalizeInjectAuthMethod(a)

	expISO := secToISO(a.ExpiresAt)
	if expISO == "" {
		// Unknown expiry: set in the past so the IDE refreshes immediately.
		expISO = time.Now().UTC().Add(-time.Minute).Format("2006-01-02T15:04:05.000Z")
	}

	region := a.Region
	if region == "" {
		region = "us-east-1"
	}

	tok = kiroIDEToken{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    expISO,
		ProfileArn:   a.ProfileArn,
		Provider:     a.Provider,
		StartURL:     a.StartUrl,
	}

	switch method {
	case "idc":
		tok.AuthMethod = "IdC"
		tok.Region = region
		if tok.Provider == "" {
			tok.Provider = "Enterprise"
		}
		if a.ClientID != "" && a.ClientSecret != "" {
			hash := sha1Hex(a.ClientID)
			tok.ClientIDHash = hash
			regName = hash + ".json"
			reg = &clientRegistration{
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				ExpiresAt:    regExpiryFromSecret(a.ClientSecret),
			}
		}
	case "external_idp":
		tok.AuthMethod = "external_idp"
		tok.Region = region
		// Kiro IDE maps token.provider through a fixed table where only
		// "ExternalIdp" yields a valid label ("External Identity Provider");
		// it is also the exact value the IDE's own external-IdP login writes.
		// Any other value (e.g. "MicrosoftEntra") renders as "Signed in with
		// undefined". The specific IdP is identified by issuerUrl, not this field.
		tok.Provider = "ExternalIdp"
		tok.IssuerURL = a.IssuerURL
		tok.IdPClientID = a.IdPClientID
		tok.Scopes = a.Scopes
		tok.LoginHint = a.LoginHint
	default: // social
		tok.AuthMethod = "social"
		if tok.Provider == "" {
			tok.Provider = "Google"
		}
	}
	return tok, regName, reg
}

// writeInjectFile writes data at 0600 (the cache holds secrets).
func writeInjectFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	return nil
}

// apiInjectStatus handles GET /admin/api/inject/status — tells the UI whether
// the inject feature is usable and where it would write.
func (h *Handler) apiInjectStatus(w http.ResponseWriter, r *http.Request) {
	available, dir := injectAvailable()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"available": available,
		"cacheDir":  dir,
		"envVar":    injectCacheDirEnv,
	})
}

// apiInjectAccount handles POST /admin/api/inject — writes the selected
// account's credentials into the Kiro IDE token cache so the IDE auto-logs-in
// as that account on next launch.
//
// Body: {"id": "<accountId>"}
func (h *Handler) apiInjectAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "account id is required"})
		return
	}

	available, _ := injectAvailable()
	if !available {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "inject not available: no Kiro cache dir. Bind-mount the host cache dir and set " + injectCacheDirEnv,
		})
		return
	}

	var acc *config.Account
	for _, a := range config.GetAccounts() {
		if a.ID == req.ID {
			ac := a
			acc = &ac
			break
		}
	}
	if acc == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "account not found"})
		return
	}

	if strings.TrimSpace(acc.AccessToken) == "" || strings.TrimSpace(acc.RefreshToken) == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "account has no access/refresh token to inject"})
		return
	}

	dir, err := injectCacheDir()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "create cache dir: " + err.Error()})
		return
	}

	tok, regName, reg := buildInjectToken(*acc)

	tokBytes, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "marshal token: " + err.Error()})
		return
	}
	tokPath := filepath.Join(dir, kiroTokenFileName)
	if err := writeInjectFile(tokPath, tokBytes); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	written := []string{tokPath}

	if reg != nil && regName != "" {
		regBytes, err := json.MarshalIndent(reg, "", "  ")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "marshal registration: " + err.Error()})
			return
		}
		regPath := filepath.Join(dir, regName)
		if err := writeInjectFile(regPath, regBytes); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		written = append(written, regPath)
	}

	// Pin the IDE's active profile to this account's profileArn. The IDE reads
	// the active arn from profile.json in its globalStorage (not the token file);
	// leaving a stale arn from a prior login makes usage/model-list calls 403.
	// Only possible when the globalStorage dir is bind-mounted (env set).
	if pPath, pErr := writeInjectProfile(injectProfileDir(), acc.ProfileArn); pErr != nil {
		logger.Warnf("[Inject] token written but profile.json update failed for %s: %v", acc.Email, pErr)
	} else if pPath != "" {
		written = append(written, pPath)
	}

	method := normalizeInjectAuthMethod(*acc)
	logger.Infof("[Inject] Wrote Kiro IDE token for account %s (%s) to %s", acc.Email, method, tokPath)

	resp := map[string]interface{}{
		"success":    true,
		"email":      acc.Email,
		"authMethod": method,
		"written":    written,
		"message":    "Đã ghi credential. Khởi động lại Kiro IDE để đăng nhập vào account này.",
	}
	// CLI guidance for Microsoft accounts: surfaced so the user isn't surprised.
	if method == "external_idp" {
		resp["cliNote"] = "kiro-cli không dùng được account Microsoft (backend yêu cầu header TokenType: EXTERNAL_IDP mà CLI không gửi)."
	}
	json.NewEncoder(w).Encode(resp)
}
