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

func TestResolveForwardWebSearchStrategy_BestAvailable(t *testing.T) {
	mk := func(id, strategy string) config.ResolvedTarget {
		return config.ResolvedTarget{Provider: config.UpstreamProvider{ID: id, Name: id, WebSearchStrategy: strategy}}
	}
	// Primary governs: native+local -> native (raw passthrough, failover rewrites local).
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("a", "native"), mk("b", "local")}); got != forwardWebSearchNative {
		t.Fatalf("native+local got %v want native", got)
	}
	// Primary governs: local+native -> local (pinned loop, native backup unused).
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("a", "local"), mk("b", "native")}); got != forwardWebSearchLocal {
		t.Fatalf("local+native got %v want local", got)
	}
	// Unsupported primary rescued by capable backup.
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("a", "unsupported"), mk("b", "native")}); got != forwardWebSearchNative {
		t.Fatalf("unsupported+native got %v want native", got)
	}
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("a", "unsupported"), mk("b", "local")}); got != forwardWebSearchLocal {
		t.Fatalf("unsupported+local got %v want local", got)
	}
	// Unsupported alone stays unsupported (explicit error, not silent Kiro pool).
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("a", "unsupported")}); got != forwardWebSearchUnsupported {
		t.Fatalf("unsupported-only got %v want unsupported", got)
	}
	// Unsupported primary + another unsupported backup + capable native later: native.
	if got := resolveForwardWebSearchStrategy([]config.ResolvedTarget{mk("a", "unsupported"), mk("b", "unsupported"), mk("c", "native")}); got != forwardWebSearchNative {
		t.Fatalf("unsupported+unsupported+native got %v want native", got)
	}
}
