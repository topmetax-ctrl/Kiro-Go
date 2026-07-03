package injector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
