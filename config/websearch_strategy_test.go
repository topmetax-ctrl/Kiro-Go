package config

import "testing"

func TestParseProviderWebSearchStrategy(t *testing.T) {
	cases := []struct {
		raw  string
		want ProviderWebSearchStrategy
	}{
		{"", ProviderWebSearchStrategyUnspecified},
		{"   ", ProviderWebSearchStrategyUnspecified},
		{"native", ProviderWebSearchStrategyNative},
		{" Native ", ProviderWebSearchStrategyNative},
		{"NATIVE", ProviderWebSearchStrategyNative},
		{"local", ProviderWebSearchStrategyLocal},
		{" LOCAL ", ProviderWebSearchStrategyLocal},
		{"unsupported", ProviderWebSearchStrategyUnsupported},
		{"disabled", ProviderWebSearchStrategyUnsupported},
		{"auto", ProviderWebSearchStrategyUnspecified},
		{"typo", ProviderWebSearchStrategyUnspecified},
		{"nativ", ProviderWebSearchStrategyUnspecified},
	}
	for _, c := range cases {
		if got := ParseProviderWebSearchStrategy(c.raw); got != c.want {
			t.Fatalf("Parse(%q)=%v want %v", c.raw, got, c.want)
		}
	}
}

func TestUpstreamProvider_WebSearchStrategyResolved(t *testing.T) {
	p := UpstreamProvider{}
	if p.WebSearchStrategyResolved() != ProviderWebSearchStrategyUnspecified {
		t.Fatal("unset must be Unspecified")
	}
	p.WebSearchStrategy = "local"
	if p.WebSearchStrategyResolved() != ProviderWebSearchStrategyLocal {
		t.Fatal("local")
	}
	p.WebSearchStrategy = "native"
	if p.WebSearchStrategyResolved() != ProviderWebSearchStrategyNative {
		t.Fatal("native")
	}
	p.WebSearchStrategy = "unsupported"
	if p.WebSearchStrategyResolved() != ProviderWebSearchStrategyUnsupported {
		t.Fatal("unsupported")
	}
}

func TestProviderWebSearchStrategy_String(t *testing.T) {
	if ProviderWebSearchStrategyNative.String() != "native" {
		t.Fatal("native String")
	}
	if ProviderWebSearchStrategyLocal.String() != "local" {
		t.Fatal("local String")
	}
	if ProviderWebSearchStrategyUnsupported.String() != "unsupported" {
		t.Fatal("unsupported String")
	}
	if ProviderWebSearchStrategyUnspecified.String() != "" {
		t.Fatal("unspecified String must be empty")
	}
}
