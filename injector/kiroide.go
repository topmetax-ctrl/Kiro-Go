package injector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultKiroCacheDir returns the directory where the Kiro IDE stores its SSO
// token cache: <home>/.aws/sso/cache on every platform.
func DefaultKiroCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".aws", "sso", "cache"), nil
}

// kiroTokenFileName is the active token file the Kiro IDE reads on launch.
const kiroTokenFileName = "kiro-auth-token.json"

// kiroProfileFileName is the file inside the IDE's globalStorage that pins the
// active CodeWhisperer profile ARN.
const kiroProfileFileName = "profile.json"

// DefaultKiroProfileDir returns the directory where the Kiro IDE stores its
// active-profile pointer (profile.json), inside the extension's globalStorage.
// The IDE reads the active profileArn from here — NOT from the token file — so
// switching accounts requires updating this file too.
func DefaultKiroProfileDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support")
	case "windows":
		if ad := strings.TrimSpace(os.Getenv("APPDATA")); ad != "" {
			base = ad
		} else {
			base = filepath.Join(home, "AppData", "Roaming")
		}
	default: // linux and others
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			base = xdg
		} else {
			base = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(base, "Kiro", "User", "globalStorage", "kiro.kiroagent"), nil
}

// kiroProfile is the shape of profile.json.
type kiroProfile struct {
	Arn  string `json:"arn"`
	Name string `json:"name"`
}

// regionFromProfileArn extracts the AWS region from a CodeWhisperer profile ARN
// ("arn:aws:codewhisperer:<region>:<account>:profile/<id>"). Returns "" when the
// ARN is empty or malformed.
func regionFromProfileArn(profileArn string) string {
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

// WriteKiroProfile writes profile.json so the IDE's active profile points at the
// account's profileArn. This is required because the IDE's ProfileArnGuard only
// fetches/writes profile.json when the arn is MISSING — a stale arn left by a
// previous login is never validated against the current token, so switching to
// an account on a different backend silently 403s usage/model-list calls. When
// profileDir is empty the default globalStorage dir is used. Returns "" (no
// error) when the account has no profileArn to pin.
func WriteKiroProfile(a ExportAccount, profileDir string) (string, error) {
	arn := strings.TrimSpace(a.Credentials.ProfileArn)
	if arn == "" {
		return "", nil
	}
	if profileDir == "" {
		var err error
		profileDir, err = DefaultKiroProfileDir()
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return "", fmt.Errorf("create profile dir: %w", err)
	}
	region := regionFromProfileArn(arn)
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

// InjectKiroIDE writes the kiro-auth-token.json (and, for IdC accounts, the
// {clientIdHash}.json registration file) into cacheDir. When cacheDir is empty
// the default Kiro cache dir is used. It returns the paths it wrote.
//
// Existing files are overwritten — callers that want a backup should copy first.
func InjectKiroIDE(a ExportAccount, cacheDir string) (written []string, err error) {
	if cacheDir == "" {
		cacheDir, err = DefaultKiroCacheDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	tok, regName, reg := buildKiroIDEToken(a)

	tokBytes, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal token: %w", err)
	}
	tokPath := filepath.Join(cacheDir, kiroTokenFileName)
	if err := writeFile0600(tokPath, tokBytes); err != nil {
		return nil, err
	}
	written = append(written, tokPath)

	if reg != nil && regName != "" {
		regBytes, err := json.MarshalIndent(reg, "", "  ")
		if err != nil {
			return written, fmt.Errorf("marshal registration: %w", err)
		}
		regPath := filepath.Join(cacheDir, regName)
		if err := writeFile0600(regPath, regBytes); err != nil {
			return written, err
		}
		written = append(written, regPath)
	}
	return written, nil
}

// writeFile0600 writes data and tightens permissions to 0600 on unix (the cache
// holds secrets). On Windows the mode arg is advisory only.
func writeFile0600(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	return nil
}
