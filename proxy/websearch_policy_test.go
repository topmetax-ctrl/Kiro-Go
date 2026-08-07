package proxy

import (
	"encoding/json"
	"testing"

	"kiro-go/search"
)

func intPtr(i int) *int { return &i }

func TestIsWebSearchTool(t *testing.T) {
	cases := []struct {
		name string
		tool ClaudeTool
		want bool
	}{
		{"bare name", ClaudeTool{Name: "web_search"}, true},
		{"name case-insensitive", ClaudeTool{Name: "Web_Search"}, true},
		{"native 20250305", ClaudeTool{Type: "web_search_20250305", Name: "web_search"}, true},
		{"native 20260209", ClaudeTool{Type: "web_search_20260209", Name: "web_search"}, true},
		{"native 20260318", ClaudeTool{Type: "web_search_20260318", Name: "web_search"}, true},
		{"unrelated client tool", ClaudeTool{Name: "bash"}, false},
		{"empty", ClaudeTool{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWebSearchTool(tc.tool); got != tc.want {
				t.Fatalf("isWebSearchTool(%+v) = %v, want %v", tc.tool, got, tc.want)
			}
		})
	}
}

func TestExtractWebSearchPolicyNoTool(t *testing.T) {
	_, ok, err := extractWebSearchPolicy([]ClaudeTool{{Name: "bash"}})
	if ok {
		t.Fatal("expected ok=false when no web_search tool present")
	}
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
}

func TestExtractWebSearchPolicyDomainsMutuallyExclusive(t *testing.T) {
	_, ok, err := extractWebSearchPolicy([]ClaudeTool{{
		Type:           "web_search_20250305",
		Name:           "web_search",
		AllowedDomains: []string{"a.com"},
		BlockedDomains: []string{"b.com"},
	}})
	if !ok {
		t.Fatal("expected ok=true (tool present)")
	}
	if _, isCfg := err.(*search.ConfigError); !isCfg {
		t.Fatalf("expected search.ConfigError, got %T (%v)", err, err)
	}
}

func TestExtractWebSearchPolicyNegativeMaxUses(t *testing.T) {
	_, _, err := extractWebSearchPolicy([]ClaudeTool{{
		Name:    "web_search",
		MaxUses: intPtr(-1),
	}})
	if _, isCfg := err.(*search.ConfigError); !isCfg {
		t.Fatalf("expected search.ConfigError for negative max_uses, got %T (%v)", err, err)
	}
}

func TestExtractWebSearchPolicyMaxUsesCapsSearches(t *testing.T) {
	// max_uses smaller than the config cap should lower MaxSearches.
	policy, ok, err := extractWebSearchPolicy([]ClaudeTool{{
		Name:    "web_search",
		MaxUses: intPtr(2),
	}})
	if err != nil || !ok {
		t.Fatalf("unexpected: ok=%v err=%v", ok, err)
	}
	if policy.MaxSearches != 2 {
		t.Fatalf("expected MaxSearches capped to 2, got %d", policy.MaxSearches)
	}
}

func TestNormalizeDomains(t *testing.T) {
	in := []string{" HTTPS://Example.com ", "example.com", "", "https://other.org/path", "other.org/path"}
	got := normalizeDomains(in)
	want := map[string]bool{"example.com": true, "other.org/path": true}
	if len(got) != len(want) {
		t.Fatalf("expected %d unique domains, got %d (%v)", len(want), len(got), got)
	}
	for _, d := range got {
		if !want[d] {
			t.Fatalf("unexpected domain %q in %v", d, got)
		}
	}
}

func TestWebSearchSchemaInjection(t *testing.T) {
	// A native web_search spec is never forwarded as a Kiro tool — the proxy
	// executes it server-side, and forwarding would make the model emit a
	// client-side tool_use no host can run. Declared ALONGSIDE a client tool it
	// is replaced by an injected function schema, which must name {query}
	// explicitly rather than collapse to a bare {"type":"object"}.
	tools := []ClaudeTool{
		{Type: "web_search_20250305", Name: "web_search"},
		{Name: "bash"},
	}
	kiro, _ := convertClaudeTools(tools)
	if len(kiro) != 2 {
		t.Fatalf("expected client tool + injected web_search, got %d", len(kiro))
	}
	var injected *KiroToolWrapper
	for i := range kiro {
		if kiro[i].ToolSpecification.Name == webSearchToolName {
			injected = &kiro[i]
		}
	}
	if injected == nil {
		t.Fatalf("no injected web_search tool in %+v", kiro)
	}
	raw, _ := json.Marshal(injected.ToolSpecification.InputSchema.JSON)
	var schema map[string]interface{}
	json.Unmarshal(raw, &schema)
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected properties in injected schema, got %s", raw)
	}
	if _, hasQuery := props["query"]; !hasQuery {
		t.Fatalf("expected query property in injected schema, got %s", raw)
	}
}

func TestNonWebSearchToolSchemaUnchanged(t *testing.T) {
	// A regular client tool keeps its own schema behavior (empty object here).
	tools := []ClaudeTool{{Name: "bash"}}
	kiro, _ := convertClaudeTools(tools)
	raw, _ := json.Marshal(kiro[0].ToolSpecification.InputSchema.JSON)
	var schema map[string]interface{}
	json.Unmarshal(raw, &schema)
	if _, hasProps := schema["properties"]; hasProps {
		t.Fatalf("did not expect injected query schema on client tool, got %s", raw)
	}
}
