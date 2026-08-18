package proxy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func portalUsageJS(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "web", "usage.js"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPortalFrontendRejectsObsoleteContract(t *testing.T) {
	js := portalUsageJS(t)
	if strings.Contains(js, "ttfbMs || 0") || strings.Contains(js, "ttfbMs||0") {
		t.Fatal("unknown TTFB must not coerce to 0")
	}
	if strings.Contains(js, "offset=") {
		t.Fatal("history must not use OFFSET pagination")
	}
	if strings.Contains(js, "source.onmessage") || strings.Contains(js, ".onmessage =") {
		t.Fatal("SSE must listen for named events, not generic onmessage")
	}
	if !strings.Contains(js, "addEventListener('request'") || !strings.Contains(js, "addEventListener('sync_required'") {
		t.Fatal("SSE must handle request and sync_required")
	}
	if !strings.Contains(js, "nextCursor") || !strings.Contains(js, "hasMore") {
		t.Fatal("history must use cursor pagination")
	}
	if strings.Contains(js, "localStorage.setItem('api") || strings.Contains(js, "sessionStorage.setItem") {
		t.Fatal("must not persist API key material in web storage")
	}
}
