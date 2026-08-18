package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

const envAPIKeyPepper = "KIRO_APIKEY_PEPPER"

// Initialized reports whether Init has loaded a config (used so NewHandler
// does not create apikeys.db in the working directory during bare tests).
func Initialized() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg != nil && cfgPath != ""
}

// GetOrCreateAPIKeyPepper returns the HMAC pepper. KIRO_APIKEY_PEPPER wins and
// is not written back. Otherwise a persisted value is used, or a new 32-byte
// value is generated and saved.
func GetOrCreateAPIKeyPepper() ([]byte, error) {
	if env := strings.TrimSpace(os.Getenv(envAPIKeyPepper)); env != "" {
		if raw, err := hex.DecodeString(env); err == nil && len(raw) >= 16 {
			return raw, nil
		}
		return []byte(env), nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return nil, errors.New("config not initialized")
	}
	if cfg.APIKeyPepper != "" {
		if raw, err := hex.DecodeString(cfg.APIKeyPepper); err == nil && len(raw) >= 16 {
			return raw, nil
		}
		return []byte(cfg.APIKeyPepper), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	cfg.APIKeyPepper = hex.EncodeToString(buf)
	if err := saveLocked(); err != nil {
		return nil, err
	}
	return buf, nil
}

func GetUsageRetentionDays() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.UsageRetentionDays <= 0 {
		return 30
	}
	return cfg.UsageRetentionDays
}

func IsPortalEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PortalEnabled == nil {
		return true
	}
	return *cfg.PortalEnabled
}
