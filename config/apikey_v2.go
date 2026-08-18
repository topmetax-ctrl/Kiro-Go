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

// LegacyPlaintextRetained is true unless an operator has finalized scrubbing.
// nil (unset) keeps rollback plaintext, matching the V1 default.
func LegacyPlaintextRetained() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LegacyPlaintextRetention == nil {
		return true
	}
	return *cfg.LegacyPlaintextRetention
}

// FinalizeLegacyPlaintext scrubs apiKeys[].key after verify succeeds for every
// non-empty secret. It is irreversible for older binaries and must be invoked
// explicitly — startup never flips the default. Save() already writes a .bak.
func FinalizeLegacyPlaintext(verify func(id, secret string) error) (int, error) {
	if verify == nil {
		return 0, errors.New("verify callback is required")
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return 0, errors.New("config not initialized")
	}
	type pair struct{ id, secret string }
	var pending []pair
	for _, e := range cfg.ApiKeys {
		secret := strings.TrimSpace(e.Key)
		if secret == "" {
			continue
		}
		pending = append(pending, pair{e.ID, secret})
	}
	for _, p := range pending {
		if err := verify(p.id, p.secret); err != nil {
			return 0, err
		}
	}
	n := 0
	for i := range cfg.ApiKeys {
		if strings.TrimSpace(cfg.ApiKeys[i].Key) == "" {
			continue
		}
		cfg.ApiKeys[i].Key = ""
		n++
	}
	off := false
	cfg.LegacyPlaintextRetention = &off
	if err := saveLocked(); err != nil {
		return 0, err
	}
	return n, nil
}

func IsPortalEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PortalEnabled == nil {
		return true
	}
	return *cfg.PortalEnabled
}
