package proxy

import (
	"strings"
	"testing"
)

func TestRedactionMasksSecrets(t *testing.T) {
	msgs := []MemoryMessage{
		{Role: "user", Content: "my key is sk-abcdef123456 keep this fact"},
	}
	out := applyMemoryRedaction(msgs, true, true)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if strings.Contains(out[0].Content, "sk-abcdef123456") {
		t.Errorf("secret leaked past redaction: %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker, got %q", out[0].Content)
	}
}

func TestRedactionCanDisableSecretMasking(t *testing.T) {
	msgs := []MemoryMessage{{Role: "user", Content: "token=abcdef123456"}}
	out := applyMemoryRedaction(msgs, false, true)
	if len(out) != 1 || !strings.Contains(out[0].Content, "abcdef123456") {
		t.Errorf("with redactSecrets=false, content should pass through: %+v", out)
	}
}

func TestRedactionDropsDiff(t *testing.T) {
	msgs := []MemoryMessage{
		{Role: "user", Content: "diff --git a/main.go b/main.go\n@@ -1 +1 @@\n-old\n+new"},
	}
	out := applyMemoryRedaction(msgs, true, false)
	if len(out) != 0 {
		t.Errorf("diff must be dropped when storeSourceCode=false, got %+v", out)
	}
}

func TestRedactionDropsEnvDump(t *testing.T) {
	msgs := []MemoryMessage{
		{Role: "user", Content: "DATABASE_URL=postgres://x\nSECRET_TOKEN=abc123def"},
	}
	out := applyMemoryRedaction(msgs, false, false)
	if len(out) != 0 {
		t.Errorf("env dump must be dropped when storeSourceCode=false, got %+v", out)
	}
}

func TestRedactionDropsLargeFencedBlock(t *testing.T) {
	big := strings.Repeat("x := doThing()\n", 40) // > 200 chars
	msgs := []MemoryMessage{{Role: "user", Content: "```go\n" + big + "```"}}
	out := applyMemoryRedaction(msgs, true, false)
	if len(out) != 0 {
		t.Errorf("large fenced code block must be dropped, got %+v", out)
	}
}

func TestRedactionKeepsShortInlineCode(t *testing.T) {
	msgs := []MemoryMessage{{Role: "user", Content: "use `go test` to run tests"}}
	out := applyMemoryRedaction(msgs, true, false)
	if len(out) != 1 {
		t.Errorf("short inline code must be kept, got %+v", out)
	}
}

func TestRedactionStoreSourceCodeAllowsCode(t *testing.T) {
	big := strings.Repeat("x := doThing()\n", 40)
	msgs := []MemoryMessage{{Role: "user", Content: "```go\n" + big + "```"}}
	out := applyMemoryRedaction(msgs, true, true)
	if len(out) != 1 {
		t.Errorf("with storeSourceCode=true, code should be kept, got %d messages", len(out))
	}
}

func TestRedactionDropsEmptiedMessage(t *testing.T) {
	msgs := []MemoryMessage{{Role: "user", Content: "   "}}
	out := applyMemoryRedaction(msgs, true, true)
	if len(out) != 0 {
		t.Errorf("whitespace-only message must be dropped, got %+v", out)
	}
}
