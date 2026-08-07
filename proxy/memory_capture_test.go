package proxy

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
)

// recordingMemoryProvider captures Add calls so a test can assert what a
// completed turn stored. Search/Delete/Health are no-ops.
type recordingMemoryProvider struct {
	mu    sync.Mutex
	added []AddMemoryInput
	done  chan struct{}
}

func newRecordingMemoryProvider() *recordingMemoryProvider {
	return &recordingMemoryProvider{done: make(chan struct{}, 1)}
}

func (r *recordingMemoryProvider) Search(context.Context, SearchQuery) ([]Memory, error) {
	return nil, nil
}
func (r *recordingMemoryProvider) Add(_ context.Context, in AddMemoryInput) error {
	r.mu.Lock()
	r.added = append(r.added, in)
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return nil
}
func (r *recordingMemoryProvider) Delete(context.Context, MemoryScope) error { return nil }
func (r *recordingMemoryProvider) Health(context.Context) error              { return nil }
func (r *recordingMemoryProvider) Name() string                              { return "recording" }

func (r *recordingMemoryProvider) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.added)
}

// setMemoryConfigForTest initializes a throwaway config and enables the memory
// sidecar with the given write mode, so MemoryEnabled/MemoryCaptureEnabled
// resolve as intended. A base URL is required for MemoryEnabled to be true.
func setMemoryConfigForTest(t *testing.T, writeMode string) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	m := config.GetMemoryConfigRaw()
	m.Enabled = true
	m.BaseURL = "http://127.0.0.1:1" // never dialed; capture uses the injected provider
	m.WriteMode = writeMode
	if err := config.UpdateMemoryConfig(m); err != nil {
		t.Fatalf("UpdateMemoryConfig: %v", err)
	}
}

func TestCaptureTurnAsyncStoresWhenAutomatic(t *testing.T) {
	setMemoryConfigForTest(t, config.MemoryWriteModeAutomatic)
	rec := newRecordingMemoryProvider()
	h := &Handler{memory: rec}

	h.captureTurnAsync("alice", "how do we deploy?", "we deploy on Fridays via CI")

	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatal("capture did not run within timeout")
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 Add, got %d", rec.count())
	}
	in := rec.added[0]
	if in.Scope.Principal != "alice" {
		t.Errorf("expected principal alice, got %q", in.Scope.Principal)
	}
	if len(in.Messages) != 2 || in.Messages[0].Role != "user" || in.Messages[1].Role != "assistant" {
		t.Errorf("expected user+assistant messages, got %+v", in.Messages)
	}
	if in.Messages[0].Content != "how do we deploy?" || in.Messages[1].Content != "we deploy on Fridays via CI" {
		t.Errorf("unexpected captured content: %+v", in.Messages)
	}
}

func TestCaptureTurnAsyncSkipsWhenExplicit(t *testing.T) {
	setMemoryConfigForTest(t, config.MemoryWriteModeExplicit)
	rec := newRecordingMemoryProvider()
	h := &Handler{memory: rec}

	h.captureTurnAsync("alice", "q", "a")

	// Explicit mode must not auto-capture. Give any errant goroutine a chance to fire.
	select {
	case <-rec.done:
		t.Fatal("explicit write mode must not auto-capture")
	case <-time.After(200 * time.Millisecond):
	}
	if rec.count() != 0 {
		t.Fatalf("expected 0 Add in explicit mode, got %d", rec.count())
	}
}

func TestCaptureTurnAsyncSkipsEmptyText(t *testing.T) {
	setMemoryConfigForTest(t, config.MemoryWriteModeAutomatic)
	rec := newRecordingMemoryProvider()
	h := &Handler{memory: rec}

	h.captureTurnAsync("alice", "   ", "answer") // empty user text
	h.captureTurnAsync("alice", "question", "")  // empty assistant text

	select {
	case <-rec.done:
		t.Fatal("must not capture when either side is empty")
	case <-time.After(200 * time.Millisecond):
	}
	if rec.count() != 0 {
		t.Fatalf("expected 0 Add, got %d", rec.count())
	}
}

func TestExtractForwardedAssistantTextAnthropic(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}`)
	if got := extractForwardedAssistantText(body); got != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
}

func TestExtractForwardedAssistantTextOpenAI(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"the answer"}}]}`)
	if got := extractForwardedAssistantText(body); got != "the answer" {
		t.Errorf("expected 'the answer', got %q", got)
	}
}

func TestExtractForwardedAssistantTextSkipsNonText(t *testing.T) {
	// tool_use blocks carry no answer text; result should be empty.
	body := []byte(`{"content":[{"type":"tool_use","name":"x","input":{}}]}`)
	if got := extractForwardedAssistantText(body); got != "" {
		t.Errorf("expected empty for tool-only content, got %q", got)
	}
}

func TestExtractForwardedAssistantTextMalformed(t *testing.T) {
	if got := extractForwardedAssistantText([]byte("{not json")); got != "" {
		t.Errorf("expected empty for malformed body, got %q", got)
	}
}
