package proxy

import (
	"kiro-go/config"
	"testing"
)

func TestResolveForwardWebSearchStrategy(t *testing.T) {
	mk := func(strategy string) config.ResolvedTarget {
		return config.ResolvedTarget{Provider: config.UpstreamProvider{ID: "p1", Name: "p1", WebSearchStrategy: strategy}}
	}
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("")}); got != forwardWebSearchUnspecified {
		t.Fatalf("unset got %v want unspecified", got)
	}
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("local")}); got != forwardWebSearchLocal {
		t.Fatalf("local got %v", got)
	}
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("native")}); got != forwardWebSearchNative {
		t.Fatalf("native got %v", got)
	}
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("unsupported")}); got != forwardWebSearchUnsupported {
		t.Fatalf("unsupported got %v", got)
	}
	if got := resolveForwardWebSearchStrategy(nil); got != forwardWebSearchUnspecified {
		t.Fatalf("nil targets got %v", got)
	}
}

func TestRewriteNativeWebSearchToSynthetic(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"name":"web_search","type":"web_search_20250305"}]}`)
	out, err := rewriteNativeWebSearchToSynthetic(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) == string(body) {
		t.Fatal("must rewrite native")
	}
	body2 := []byte(`{"model":"m","tools":[{"name":"bash","description":"x"}]}`)
	out2, _ := rewriteNativeWebSearchToSynthetic(body2)
	if string(out2) != string(body2) {
		t.Fatal("non-native must be unchanged")
	}
	body3 := []byte(`{"model":"m","messages":[]}`)
	out3, _ := rewriteNativeWebSearchToSynthetic(body3)
	if string(out3) != string(body3) {
		t.Fatal("no-tools must be unchanged")
	}
}

func TestResolveForwardWebSearchStrategy_SkipsPool(t *testing.T) {
	pool := config.ResolvedTarget{Provider: config.UpstreamProvider{ID: config.KiroPoolTargetID, Name: "Kiro Pool"}}
	local := config.ResolvedTarget{Provider: config.UpstreamProvider{ID: "p2", Name: "p2", WebSearchStrategy: "local"}}
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{pool, local}); got != forwardWebSearchLocal {
		t.Fatalf("must skip pool and resolve local, got %v", got)
	}
}
