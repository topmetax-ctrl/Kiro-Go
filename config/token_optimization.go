package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Token Optimization settings.
//
// The feature is a thin integration layer over external engines (Headroom,
// RTK) and official prompt assets (Caveman, Ponytail). Nothing here
// reimplements those projects; this file only owns the persisted operator
// choices and their validation. Runtime behavior lives in package tokenopt.
//
// The zero value means "off": existing installs that never touch the feature
// keep byte-identical config files (the field is omitted when zero) and
// unchanged inference behavior.

// Engine mode: how the gateway treats requests.
const (
	TokenOptModeOff     = "off"     // inference is never touched
	TokenOptModeObserve = "observe" // measure what an engine would save; send nothing different
	TokenOptModeOn      = "on"      // apply the selected integrations
)

// Input compression engine selection. There is deliberately no "auto": it would
// need a second gateway-level engine to fall back to, and RTK has none (see
// docs/token-optimization.md). Offering it would promise a fallback that does
// not exist.
const (
	TokenOptInputNone     = "none"
	TokenOptInputRTK      = "rtk"
	TokenOptInputHeadroom = "headroom"
)

// Output-style (Caveman) and coding-behavior (Ponytail) levels. The values are
// the upstream projects' own mode names so the UI never renames an official
// asset into something it is not.
const (
	TokenOptStyleOff      = "off"
	TokenOptCaveman       = "caveman"
	TokenOptUltracave     = "ultracave"
	TokenOptPonytailOff   = "off"
	TokenOptPonytailLite  = "lite"
	TokenOptPonytailFull  = "full"
	TokenOptPonytailUltra = "ultra"
)

// Headroom operating modes. These mirror Headroom's own "cache" and "token"
// proxy modes (headroom/agent_savings.py): cache mode compresses only the newest
// tool-result delta and keeps everything before it byte-identical, so the
// provider's prompt-prefix cache survives; token mode lets the engine rewrite
// older messages too, trading cache hits for more savings.
const (
	HeadroomModeCache = "cache"
	HeadroomModeToken = "token"

	DefaultHeadroomTimeout = 3000 // ms
	MinHeadroomTimeout     = 200
	MaxHeadroomTimeout     = 30000

	// A tool result smaller than this is never sent to the engine: the
	// round-trip costs more than it can save.
	DefaultHeadroomMin  = 4096
	MinHeadroomMinBytes = 256
	MaxHeadroomMinBytes = 1 << 20
)

// TokenOptimizationConfig is the persisted Token Optimization state.
type TokenOptimizationConfig struct {
	// Mode is "off" (default), "observe" or "on".
	Mode string `json:"mode,omitempty"`
	// InputCompression selects the input engine: "none" (default), "rtk" or "headroom".
	InputCompression string `json:"inputCompression,omitempty"`

	Headroom HeadroomConfig `json:"headroom,omitzero"`
	RTK      RTKConfig      `json:"rtk,omitzero"`

	// Caveman is the response-style asset: "off" (default), "caveman" or "ultracave".
	Caveman string `json:"caveman,omitempty"`
	// Ponytail is the coding-behavior asset: "off" (default), "lite", "full" or "ultra".
	Ponytail string `json:"ponytail,omitempty"`
}

// HeadroomConfig locates and tunes the Headroom compress service
// (`headroom proxy`, POST /v1/compress).
type HeadroomConfig struct {
	// BaseURL is the service root, e.g. http://127.0.0.1:8787.
	BaseURL string `json:"baseUrl,omitempty"`
	// APIKey is Headroom's inbound proxy token (HEADROOM_PROXY_TOKEN), sent as
	// x-headroom-proxy-token. Never returned by the admin API.
	APIKey string `json:"apiKey,omitempty"`
	// TimeoutMs bounds one compress call. 0 means DefaultHeadroomTimeout.
	TimeoutMs int `json:"timeoutMs,omitempty"`
	// Mode is the operating mode: "cache" (default) or "token". See the
	// HeadroomMode* constants.
	Mode string `json:"mode,omitempty"`
	// MinBytes is the smallest tool result worth a round-trip. 0 means DefaultHeadroomMin.
	MinBytes int `json:"minBytes,omitempty"`
}

// RTKConfig describes how RTK is deployed. RTK has no gateway-level compress
// API: its CLI hooks run on the agent's machine and rtk-proxy is a transparent
// hop. ProxyURL is only used to verify and display that deployment.
type RTKConfig struct {
	ProxyURL string `json:"proxyUrl,omitempty"`
}

// TokenOptimizationError is a validation failure naming the offending field, so
// the admin API can answer 400 instead of 500.
type TokenOptimizationError struct {
	Field string
	Msg   string
}

func (e *TokenOptimizationError) Error() string { return e.Field + ": " + e.Msg }

// Resolved returns a normalized copy: unknown or empty values collapse to their
// safe defaults ("off"/"none"), so a hand-edited or newer-version config can
// never switch inference behavior on by accident.
func (c TokenOptimizationConfig) Resolved() TokenOptimizationConfig {
	c.Mode = oneOf(c.Mode, TokenOptModeOff, TokenOptModeOff, TokenOptModeObserve, TokenOptModeOn)
	c.InputCompression = oneOf(c.InputCompression, TokenOptInputNone, TokenOptInputNone, TokenOptInputRTK, TokenOptInputHeadroom)
	c.Caveman = oneOf(c.Caveman, TokenOptStyleOff, TokenOptStyleOff, TokenOptCaveman, TokenOptUltracave)
	c.Ponytail = oneOf(c.Ponytail, TokenOptPonytailOff, TokenOptPonytailOff, TokenOptPonytailLite, TokenOptPonytailFull, TokenOptPonytailUltra)

	c.Headroom.BaseURL = strings.TrimRight(strings.TrimSpace(c.Headroom.BaseURL), "/")
	c.Headroom.APIKey = strings.TrimSpace(c.Headroom.APIKey)
	c.Headroom.Mode = oneOf(c.Headroom.Mode, HeadroomModeCache, HeadroomModeCache, HeadroomModeToken)
	if c.Headroom.TimeoutMs <= 0 {
		c.Headroom.TimeoutMs = DefaultHeadroomTimeout
	}
	if c.Headroom.MinBytes <= 0 {
		c.Headroom.MinBytes = DefaultHeadroomMin
	}
	c.RTK.ProxyURL = strings.TrimRight(strings.TrimSpace(c.RTK.ProxyURL), "/")
	return c
}

// oneOf lower-cases v and returns it when it is one of allowed, else def.
func oneOf(v, def string, allowed ...string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

// Canonical is the minimal stored form of c: normalized, with every field that
// equals its default zeroed. A config that only restates defaults becomes the
// zero value (and so disappears from the file), and a later change to a default
// reaches every install that never chose a value of its own.
func (c TokenOptimizationConfig) Canonical() TokenOptimizationConfig {
	r := c.Resolved()
	if r.Mode == TokenOptModeOff {
		r.Mode = ""
	}
	if r.InputCompression == TokenOptInputNone {
		r.InputCompression = ""
	}
	if r.Caveman == TokenOptStyleOff {
		r.Caveman = ""
	}
	if r.Ponytail == TokenOptPonytailOff {
		r.Ponytail = ""
	}
	if r.Headroom.Mode == HeadroomModeCache {
		r.Headroom.Mode = ""
	}
	if r.Headroom.TimeoutMs == DefaultHeadroomTimeout {
		r.Headroom.TimeoutMs = 0
	}
	if r.Headroom.MinBytes == DefaultHeadroomMin {
		r.Headroom.MinBytes = 0
	}
	return r
}

// NormalizeServiceURL validates and canonicalizes the base URL of an optional
// local service. It is a static check only; the network-level guard (blocking
// link-local and metadata addresses at dial time) lives where the connection is
// made, because a hostname can resolve to anything.
//
// Rules: absolute http(s) URL, a host, no credentials, no query or fragment. A
// path prefix is allowed so the service can sit behind a reverse proxy.
func NormalizeServiceURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a valid URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", fmt.Errorf("must be an absolute http(s) URL with a host")
	}
	if u.User != nil {
		return "", fmt.Errorf("must not embed credentials; use the token field")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must not contain a query string or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// ValidateTokenOptimization checks a full config an operator is about to save.
// It rejects values the Resolved() fallback would silently swallow, so a typo in
// the admin API surfaces as an error instead of a quietly disabled feature.
func ValidateTokenOptimization(c TokenOptimizationConfig) error {
	bad := func(field, msg string) error { return &TokenOptimizationError{Field: field, Msg: msg} }

	if v := strings.ToLower(strings.TrimSpace(c.Mode)); v != "" && oneOf(v, "", TokenOptModeOff, TokenOptModeObserve, TokenOptModeOn) == "" {
		return bad("mode", `must be "off", "observe" or "on"`)
	}
	if v := strings.ToLower(strings.TrimSpace(c.InputCompression)); v != "" {
		if v == "auto" {
			return bad("inputCompression", `"auto" is not available: only Headroom is a gateway-level engine, so there is nothing to fall back to`)
		}
		if oneOf(v, "", TokenOptInputNone, TokenOptInputRTK, TokenOptInputHeadroom) == "" {
			return bad("inputCompression", `must be "none", "rtk" or "headroom"`)
		}
	}
	if v := strings.ToLower(strings.TrimSpace(c.Caveman)); v != "" && oneOf(v, "", TokenOptStyleOff, TokenOptCaveman, TokenOptUltracave) == "" {
		return bad("caveman", `must be "off", "caveman" or "ultracave"`)
	}
	if v := strings.ToLower(strings.TrimSpace(c.Ponytail)); v != "" && oneOf(v, "", TokenOptPonytailOff, TokenOptPonytailLite, TokenOptPonytailFull, TokenOptPonytailUltra) == "" {
		return bad("ponytail", `must be "off", "lite", "full" or "ultra"`)
	}
	if v := strings.ToLower(strings.TrimSpace(c.Headroom.Mode)); v != "" && oneOf(v, "", HeadroomModeCache, HeadroomModeToken) == "" {
		return bad("headroom.mode", `must be "cache" or "token"`)
	}
	if t := c.Headroom.TimeoutMs; t != 0 && (t < MinHeadroomTimeout || t > MaxHeadroomTimeout) {
		return bad("headroom.timeoutMs", fmt.Sprintf("must be between %d and %d", MinHeadroomTimeout, MaxHeadroomTimeout))
	}
	if m := c.Headroom.MinBytes; m != 0 && (m < MinHeadroomMinBytes || m > MaxHeadroomMinBytes) {
		return bad("headroom.minBytes", fmt.Sprintf("must be between %d and %d", MinHeadroomMinBytes, MaxHeadroomMinBytes))
	}
	if u := strings.TrimSpace(c.Headroom.BaseURL); u != "" {
		if _, err := NormalizeServiceURL(u); err != nil {
			return bad("headroom.baseUrl", err.Error())
		}
	}
	if u := strings.TrimSpace(c.RTK.ProxyURL); u != "" {
		if _, err := NormalizeServiceURL(u); err != nil {
			return bad("rtk.proxyUrl", err.Error())
		}
	}

	// A selected engine that cannot work is a configuration mistake, not a
	// runtime degradation: refuse it while the feature is live.
	r := c.Resolved()
	if r.Mode != TokenOptModeOff && r.InputCompression == TokenOptInputHeadroom && r.Headroom.BaseURL == "" {
		return bad("headroom.baseUrl", "is required while Headroom is the selected input engine")
	}
	return nil
}

// GetTokenOptimization returns the resolved settings. The result is a copy
// (it holds only strings and ints), so callers cannot mutate shared state.
func GetTokenOptimization() TokenOptimizationConfig {
	return GetTokenOptimizationRaw().Resolved()
}

// GetTokenOptimizationRaw returns the stored settings WITHOUT defaults applied.
// Use it as the base of an admin patch: saving a resolved copy would persist
// today's defaults as if the operator chose them and freeze them against future
// default changes.
func GetTokenOptimizationRaw() TokenOptimizationConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return TokenOptimizationConfig{}
	}
	return cfg.TokenOptimization
}

// UpdateTokenOptimization validates and persists the settings atomically, in
// canonical form. The new value is visible to the next request; no restart is
// needed.
func UpdateTokenOptimization(c TokenOptimizationConfig) error {
	return PatchTokenOptimization(func(cur *TokenOptimizationConfig) { *cur = c })
}

// PatchTokenOptimization applies fn to the stored settings and saves the result,
// all under one lock, so two admins saving at the same moment cannot overwrite
// each other's change (a read-modify-write done outside the lock would). fn
// receives the resolved settings; if the result is invalid nothing is stored.
func PatchTokenOptimization(fn func(*TokenOptimizationConfig)) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	next := cfg.TokenOptimization.Resolved()
	fn(&next)
	if err := ValidateTokenOptimization(next); err != nil {
		return err
	}
	prev := cfg.TokenOptimization
	cfg.TokenOptimization = next.Canonical()
	if err := Save(); err != nil {
		cfg.TokenOptimization = prev // keep memory and disk agreeing
		return err
	}
	return nil
}

// MaskSecret hides a secret for display. Unlike MaskApiKey it never returns a
// short secret verbatim: a Headroom token is often short and human-chosen.
func MaskSecret(s string) string {
	switch n := len(s); {
	case n == 0:
		return ""
	case n <= 8:
		return "****"
	default:
		return "****" + s[n-4:]
	}
}
