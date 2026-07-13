package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// initEmptyConfig gives each test a fresh config file so package globals do not
// bleed across tests.
func initEmptyConfig(t *testing.T) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
}

// TestWebSearchDefaultsResolveWhenUnset verifies that a config with no webSearch
// section still yields every nested default, so downstream code never sees a
// zero cap.
func TestWebSearchDefaultsResolveWhenUnset(t *testing.T) {
	initEmptyConfig(t)

	ws := GetWebSearchConfig()

	if ws.Routing.Mode != DefaultWebSearchRoutingMode {
		t.Fatalf("routing mode = %q, want %q", ws.Routing.Mode, DefaultWebSearchRoutingMode)
	}
	if ws.Routing.PrimaryProvider != DefaultWebSearchPrimaryProvider {
		t.Fatalf("primary provider = %q, want %q", ws.Routing.PrimaryProvider, DefaultWebSearchPrimaryProvider)
	}
	if len(ws.Routing.FallbackProviders) != 1 || ws.Routing.FallbackProviders[0] != ProviderTavily {
		t.Fatalf("fallback providers = %v, want [tavily]", ws.Routing.FallbackProviders)
	}
	if ws.Routing.AllowPaidUsage {
		t.Fatalf("allowPaidUsage must default to false")
	}
	if ws.Limits.MaxRounds != DefaultWebSearchMaxRounds {
		t.Fatalf("maxRounds = %d, want %d", ws.Limits.MaxRounds, DefaultWebSearchMaxRounds)
	}
	if ws.Limits.MaxSearchesPerRequest != DefaultWebSearchMaxSearches {
		t.Fatalf("maxSearchesPerRequest = %d, want %d", ws.Limits.MaxSearchesPerRequest, DefaultWebSearchMaxSearches)
	}
	if ws.Limits.MaxConcurrentSearches != DefaultWebSearchMaxConcurrency {
		t.Fatalf("maxConcurrentSearches = %d, want %d", ws.Limits.MaxConcurrentSearches, DefaultWebSearchMaxConcurrency)
	}
	if ws.SearXNG.Enabled == nil || !*ws.SearXNG.Enabled {
		t.Fatalf("searxng must default to enabled")
	}
	if ws.SearXNG.BaseURL != DefaultSearXNGBaseURL {
		t.Fatalf("searxng baseURL = %q, want %q", ws.SearXNG.BaseURL, DefaultSearXNGBaseURL)
	}
	if ws.SearXNG.SafeSearch == nil || *ws.SearXNG.SafeSearch != DefaultSearXNGSafeSearch {
		t.Fatalf("searxng safeSearch default wrong: %v", ws.SearXNG.SafeSearch)
	}
	if len(ws.SearXNG.Categories) != 1 || ws.SearXNG.Categories[0] != "general" {
		t.Fatalf("searxng categories = %v, want [general]", ws.SearXNG.Categories)
	}
	if ws.Tavily.FreeOnly == nil || !*ws.Tavily.FreeOnly {
		t.Fatalf("tavily freeOnly must default to true")
	}
	if ws.Tavily.MonthlyCreditLimit != DefaultTavilyMonthlyCredits {
		t.Fatalf("tavily monthlyCreditLimit = %d, want %d", ws.Tavily.MonthlyCreditLimit, DefaultTavilyMonthlyCredits)
	}
	if ws.Cache.Enabled == nil || !*ws.Cache.Enabled {
		t.Fatalf("cache must default to enabled")
	}
	if ws.Reranking.Provider != "heuristic" {
		t.Fatalf("rerank provider = %q, want heuristic", ws.Reranking.Provider)
	}
}

// TestWebSearchZeroSafeSearchPreserved checks that an explicit safeSearch:0 is
// not overwritten by the default (pointer distinguishes 0 from unset).
func TestWebSearchZeroSafeSearchPreserved(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	zero := 0
	c := Config{
		Port:      8080,
		Host:      "0.0.0.0",
		WebSearch: WebSearchConfig{SearXNG: SearXNGConfig{SafeSearch: &zero}},
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(cfgFile, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	ws := GetWebSearchConfig()
	if ws.SearXNG.SafeSearch == nil || *ws.SearXNG.SafeSearch != 0 {
		t.Fatalf("explicit safeSearch:0 was overwritten: %v", ws.SearXNG.SafeSearch)
	}
}

// TestWebSearchEnabledFreeFirst proves the free-first gate: SearXNG alone (no
// Tavily key) satisfies WebSearchEnabled, and the master toggle still governs.
func TestWebSearchEnabledFreeFirst(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	enabled := true
	c := Config{
		Port: 8080,
		Host: "0.0.0.0",
		WebSearch: WebSearchConfig{
			Enabled: true,
			SearXNG: SearXNGConfig{Enabled: &enabled, BaseURL: "http://searxng:8080"},
		},
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(cfgFile, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	if !SearXNGProviderEnabled() {
		t.Fatalf("SearXNG should be enabled with a base URL")
	}
	if TavilyProviderEnabled() {
		t.Fatalf("Tavily must be disabled without a key")
	}
	if !WebSearchEnabled() {
		t.Fatalf("web search should be enabled via SearXNG alone (free-first)")
	}
	if WebSearchAllowPaidUsage() {
		t.Fatalf("paid usage must default to false")
	}
}

// TestWebSearchDisabledWhenNoProvider ensures the toggle alone, with no usable
// provider, does not enable the loop.
func TestWebSearchDisabledWhenNoProvider(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	disabled := false
	c := Config{
		Port: 8080,
		Host: "0.0.0.0",
		WebSearch: WebSearchConfig{
			Enabled: true,
			SearXNG: SearXNGConfig{Enabled: &disabled},
		},
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(cfgFile, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	if WebSearchEnabled() {
		t.Fatalf("web search must be disabled when no provider is usable")
	}
}

// TestTavilyProviderEnabledWithEnvKey verifies the env override alone (Tavily
// turned on) makes Tavily usable.
func TestTavilyProviderEnabledWithEnvKey(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "tvly-test")
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	c := Config{
		Port:      8080,
		Host:      "0.0.0.0",
		WebSearch: WebSearchConfig{Enabled: true, Tavily: TavilyConfig{Enabled: true}},
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(cfgFile, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	if !TavilyProviderEnabled() {
		t.Fatalf("Tavily should be enabled with env key + toggle")
	}
	if TavilyAPIKeyResolved() != "tvly-test" {
		t.Fatalf("env key should resolve, got %q", TavilyAPIKeyResolved())
	}
}
