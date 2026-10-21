package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A config that never touched Token Optimization must resolve to "off" with
// every engine disabled, so upgrading never changes inference behavior.
func TestTokenOptimizationDefaultsAreOff(t *testing.T) {
	initEmptyConfig(t)

	c := GetTokenOptimization()
	if c.Mode != TokenOptModeOff || c.InputCompression != TokenOptInputNone {
		t.Fatalf("mode/input = %q/%q, want off/none", c.Mode, c.InputCompression)
	}
	if c.Caveman != TokenOptStyleOff || c.Ponytail != TokenOptPonytailOff {
		t.Fatalf("caveman/ponytail = %q/%q, want off/off", c.Caveman, c.Ponytail)
	}
	if c.Headroom.TimeoutMs != DefaultHeadroomTimeout || c.Headroom.MinBytes != DefaultHeadroomMin || c.Headroom.Mode != HeadroomModeCache {
		t.Fatalf("headroom defaults not applied: %+v", c.Headroom)
	}
}

// Upgrading must not rewrite the config file of an install that never opted in:
// the section is omitted entirely while the value is zero.
func TestTokenOptimizationOmittedFromFileWhileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "tokenOptimization") {
		t.Fatalf("untouched config must not mention tokenOptimization:\n%s", data)
	}
}

// Hand-edited or newer-version values must collapse to the safe default rather
// than switching inference behavior on.
func TestTokenOptimizationResolvedCollapsesUnknownValues(t *testing.T) {
	got := TokenOptimizationConfig{
		Mode: "TURBO", InputCompression: "auto", Caveman: "wenyan", Ponytail: "extreme",
		Headroom: HeadroomConfig{Mode: "lossy_inline", TimeoutMs: -5, MinBytes: -1},
	}.Resolved()

	if got.Mode != TokenOptModeOff || got.InputCompression != TokenOptInputNone ||
		got.Caveman != TokenOptStyleOff || got.Ponytail != TokenOptPonytailOff {
		t.Fatalf("unknown values must resolve to off/none: %+v", got)
	}
	if got.Headroom.Mode != HeadroomModeCache || got.Headroom.TimeoutMs != DefaultHeadroomTimeout || got.Headroom.MinBytes != DefaultHeadroomMin {
		t.Fatalf("headroom fallbacks wrong: %+v", got.Headroom)
	}
}

func TestTokenOptimizationResolvedNormalizesCaseAndURLs(t *testing.T) {
	got := TokenOptimizationConfig{
		Mode: " On ", InputCompression: "HEADROOM", Ponytail: "Ultra",
		Headroom: HeadroomConfig{BaseURL: " http://127.0.0.1:8787/ ", APIKey: "  tok  "},
		RTK:      RTKConfig{ProxyURL: "http://127.0.0.1:8081//"},
	}.Resolved()

	if got.Mode != TokenOptModeOn || got.InputCompression != TokenOptInputHeadroom || got.Ponytail != TokenOptPonytailUltra {
		t.Fatalf("case not normalized: %+v", got)
	}
	if got.Headroom.BaseURL != "http://127.0.0.1:8787" || got.Headroom.APIKey != "tok" || got.RTK.ProxyURL != "http://127.0.0.1:8081" {
		t.Fatalf("whitespace/slashes not trimmed: %+v %+v", got.Headroom, got.RTK)
	}
}

func TestValidateTokenOptimization(t *testing.T) {
	on := func(mut func(*TokenOptimizationConfig)) TokenOptimizationConfig {
		c := TokenOptimizationConfig{Mode: "on", InputCompression: "headroom", Headroom: HeadroomConfig{BaseURL: "http://127.0.0.1:8787"}}
		mut(&c)
		return c
	}
	tests := []struct {
		name  string
		cfg   TokenOptimizationConfig
		field string // "" = valid
	}{
		{"zero value is valid", TokenOptimizationConfig{}, ""},
		{"full valid", on(func(*TokenOptimizationConfig) {}), ""},
		{"bad mode", on(func(c *TokenOptimizationConfig) { c.Mode = "maybe" }), "mode"},
		{"auto is refused with a reason", on(func(c *TokenOptimizationConfig) { c.InputCompression = "auto" }), "inputCompression"},
		{"bad input", on(func(c *TokenOptimizationConfig) { c.InputCompression = "zstd" }), "inputCompression"},
		{"bad caveman (wenyan is not offered)", on(func(c *TokenOptimizationConfig) { c.Caveman = "megacave" }), "caveman"},
		{"bad ponytail", on(func(c *TokenOptimizationConfig) { c.Ponytail = "review" }), "ponytail"},
		{"bad headroom mode (ccr needs a retrieval loop we do not run)", on(func(c *TokenOptimizationConfig) { c.Headroom.Mode = "ccr" }), "headroom.mode"},
		{"token mode is valid", on(func(c *TokenOptimizationConfig) { c.Headroom.Mode = "token" }), ""},
		{"timeout too small", on(func(c *TokenOptimizationConfig) { c.Headroom.TimeoutMs = 10 }), "headroom.timeoutMs"},
		{"timeout too large", on(func(c *TokenOptimizationConfig) { c.Headroom.TimeoutMs = 999999 }), "headroom.timeoutMs"},
		{"min bytes too small", on(func(c *TokenOptimizationConfig) { c.Headroom.MinBytes = 5 }), "headroom.minBytes"},
		{"url without scheme", on(func(c *TokenOptimizationConfig) { c.Headroom.BaseURL = "127.0.0.1:8787" }), "headroom.baseUrl"},
		{"url with credentials", on(func(c *TokenOptimizationConfig) { c.Headroom.BaseURL = "http://u:p@127.0.0.1:8787" }), "headroom.baseUrl"},
		{"url with query", on(func(c *TokenOptimizationConfig) { c.Headroom.BaseURL = "http://127.0.0.1:8787?x=1" }), "headroom.baseUrl"},
		{"file scheme", on(func(c *TokenOptimizationConfig) { c.Headroom.BaseURL = "file:///etc/passwd" }), "headroom.baseUrl"},
		{"headroom selected + live but no url", on(func(c *TokenOptimizationConfig) { c.Headroom.BaseURL = "" }), "headroom.baseUrl"},
		{"headroom selected but feature off needs no url", on(func(c *TokenOptimizationConfig) { c.Mode = "off"; c.Headroom.BaseURL = "" }), ""},
		{"rtk proxy url validated", on(func(c *TokenOptimizationConfig) { c.RTK.ProxyURL = "ftp://x" }), "rtk.proxyUrl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTokenOptimization(tt.cfg)
			if tt.field == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			te, ok := err.(*TokenOptimizationError)
			if !ok {
				t.Fatalf("want *TokenOptimizationError on %q, got %T %v", tt.field, err, err)
			}
			if te.Field != tt.field {
				t.Fatalf("field = %q, want %q (%v)", te.Field, tt.field, err)
			}
		})
	}
}

func TestTokenOptimizationPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	want := TokenOptimizationConfig{
		Mode: "observe", InputCompression: "headroom", Caveman: "ultracave", Ponytail: "lite",
		Headroom: HeadroomConfig{BaseURL: "http://127.0.0.1:8787", APIKey: "tok", TimeoutMs: 1500, Mode: "token", MinBytes: 2048},
		RTK:      RTKConfig{ProxyURL: "http://127.0.0.1:8081"},
	}
	if err := UpdateTokenOptimization(want); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil { // reload from disk
		t.Fatal(err)
	}
	if got := GetTokenOptimizationRaw(); got != want.Canonical() {
		t.Fatalf("reloaded = %+v\nwant     %+v", got, want)
	}
}

// A rejected update must not touch what is stored.
func TestUpdateTokenOptimizationRejectsInvalidWithoutSaving(t *testing.T) {
	initEmptyConfig(t)
	if err := UpdateTokenOptimization(TokenOptimizationConfig{Mode: "observe"}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateTokenOptimization(TokenOptimizationConfig{Mode: "bogus"}); err == nil {
		t.Fatal("invalid update was accepted")
	}
	if got := GetTokenOptimizationRaw().Mode; got != "observe" {
		t.Fatalf("stored mode = %q after rejected update, want observe", got)
	}
}

// GetTokenOptimization hands out copies: mutating one must never leak into the
// shared config that every request reads.
func TestGetTokenOptimizationReturnsIndependentCopy(t *testing.T) {
	initEmptyConfig(t)
	if err := UpdateTokenOptimization(TokenOptimizationConfig{Mode: "on", Ponytail: "full"}); err != nil {
		t.Fatal(err)
	}
	a := GetTokenOptimization()
	a.Mode = "off"
	a.Headroom.BaseURL = "http://evil"
	if b := GetTokenOptimization(); b.Mode != "on" || b.Headroom.BaseURL != "" {
		t.Fatalf("mutation leaked into shared config: %+v", b)
	}
}

// Settings updates racing with readers (every request reads the config) must be
// safe and always observe a coherent snapshot. Run with -race.
func TestTokenOptimizationConcurrentUpdatesAreCoherent(t *testing.T) {
	initEmptyConfig(t)
	a := TokenOptimizationConfig{Mode: "on", InputCompression: "headroom", Caveman: "caveman", Headroom: HeadroomConfig{BaseURL: "http://127.0.0.1:1"}}
	b := TokenOptimizationConfig{Mode: "off", InputCompression: "none", Caveman: "off"}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := GetTokenOptimization()
				// a and b differ in every field; a torn read would mix them.
				if (c.Mode == "on") != (c.InputCompression == "headroom") || (c.Mode == "on") != (c.Caveman == "caveman") {
					t.Errorf("torn snapshot: %+v", c)
					return
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		next := a
		if i%2 == 1 {
			next = b
		}
		if err := UpdateTokenOptimization(next); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestMaskSecretNeverRevealsShortSecrets(t *testing.T) {
	cases := map[string]string{"": "", "abc": "****", "12345678": "****", "123456789": "****6789", "a-long-proxy-token": "****oken"}
	for in, want := range cases {
		if got := MaskSecret(in); got != want {
			t.Errorf("MaskSecret(%q) = %q, want %q", in, got, want)
		}
	}
}

// Restating defaults must leave no trace: the config returns to the zero value,
// so the file is byte-identical to one written before the feature existed.
func TestCanonicalFormDropsDefaultsAndRoundTripsToTheZeroValue(t *testing.T) {
	restated := TokenOptimizationConfig{
		Mode: "off", InputCompression: "none", Caveman: "off", Ponytail: "off",
		Headroom: HeadroomConfig{Mode: "cache", TimeoutMs: DefaultHeadroomTimeout, MinBytes: DefaultHeadroomMin},
	}
	if got := restated.Canonical(); got != (TokenOptimizationConfig{}) {
		t.Fatalf("defaults were kept: %+v", got)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := UpdateTokenOptimization(TokenOptimizationConfig{Mode: "on", Caveman: "caveman"}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateTokenOptimization(restated); err != nil { // the UI's "reset"
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "tokenOptimization") {
		t.Fatalf("a reset config must vanish from the file:\n%s", data)
	}

	// Non-default values survive, URLs and secrets are never touched.
	kept := TokenOptimizationConfig{Mode: "on", Headroom: HeadroomConfig{BaseURL: "http://127.0.0.1:8787", APIKey: "k", TimeoutMs: 900, Mode: "token"}}.Canonical()
	if kept.Mode != "on" || kept.Headroom.TimeoutMs != 900 || kept.Headroom.Mode != "token" || kept.Headroom.APIKey != "k" || kept.Headroom.BaseURL != "http://127.0.0.1:8787" {
		t.Fatalf("canonical form lost a real choice: %+v", kept)
	}
}

// Two admins saving different fields at the same moment must both win: the
// read-modify-write happens under one lock.
func TestPatchTokenOptimizationLosesNoUpdates(t *testing.T) {
	initEmptyConfig(t)
	var wg sync.WaitGroup
	for i, mut := range []func(*TokenOptimizationConfig){
		func(c *TokenOptimizationConfig) { c.Mode = "observe" },
		func(c *TokenOptimizationConfig) { c.Caveman = "ultracave" },
		func(c *TokenOptimizationConfig) { c.Ponytail = "ultra" },
		func(c *TokenOptimizationConfig) { c.Headroom.TimeoutMs = 1234 },
		func(c *TokenOptimizationConfig) { c.RTK.ProxyURL = "http://127.0.0.1:8081" },
	} {
		wg.Add(1)
		go func(i int, mut func(*TokenOptimizationConfig)) {
			defer wg.Done()
			if err := PatchTokenOptimization(mut); err != nil {
				t.Errorf("patch %d: %v", i, err)
			}
		}(i, mut)
	}
	wg.Wait()
	got := GetTokenOptimization()
	if got.Mode != "observe" || got.Caveman != "ultracave" || got.Ponytail != "ultra" || got.Headroom.TimeoutMs != 1234 || got.RTK.ProxyURL != "http://127.0.0.1:8081" {
		t.Fatalf("a concurrent patch was lost: %+v", got)
	}
}

// A patch that fails validation stores nothing, in memory or on disk.
func TestPatchTokenOptimizationRejectsInvalidResultAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := PatchTokenOptimization(func(c *TokenOptimizationConfig) { c.Mode = "observe" }); err != nil {
		t.Fatal(err)
	}
	err := PatchTokenOptimization(func(c *TokenOptimizationConfig) { c.Mode = "on"; c.InputCompression = "headroom" }) // needs a URL
	if _, ok := err.(*TokenOptimizationError); !ok {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if got := GetTokenOptimizationRaw().Mode; got != "observe" {
		t.Fatalf("a rejected patch changed memory: mode = %q", got)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := GetTokenOptimizationRaw().Mode; got != "observe" {
		t.Fatalf("a rejected patch reached disk: mode = %q", got)
	}
}

// A config that only sets the mode must not leave empty nested objects behind.
func TestCanonicalConfigWritesNoEmptyNestedObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := UpdateTokenOptimization(TokenOptimizationConfig{Mode: "on", Caveman: "caveman"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, junk := range []string{`"headroom"`, `"rtk"`, `"inputCompression"`, `"ponytail"`, `"timeoutMs"`} {
		if strings.Contains(string(data), junk) {
			t.Errorf("default-valued field %s was written:\n%s", junk, data)
		}
	}
}
