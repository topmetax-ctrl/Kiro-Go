package proxy

import (
	"context"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// captureTurnAsync stores a completed Q&A turn into memory, in the background,
// so it never adds latency to the response the caller already received. It is a
// no-op unless capture is enabled (MemoryCaptureEnabled: sidecar usable AND a
// non-explicit write mode), and unless both sides of the turn carry text.
//
// Redaction is NOT applied here: Mem0HTTPProvider.Add enforces applyMemoryRedaction
// internally (it cannot be bypassed by callers), so secret masking and the
// source-code guard hold no matter who calls Add.
//
// The goroutine uses context.Background() (with the configured write timeout), NOT
// the request context, which is already cancelled once the response returns. The
// provider is fail-open, so a backend outage is swallowed and logged, never
// surfaced.
func (h *Handler) captureTurnAsync(principal, userText, assistantText string) {
	if !config.MemoryCaptureEnabled() {
		return
	}
	userText = strings.TrimSpace(userText)
	assistantText = strings.TrimSpace(assistantText)
	if userText == "" || assistantText == "" {
		return
	}
	if strings.TrimSpace(principal) == "" {
		principal = anonymousOwner
	}

	mem := h.getMemory()
	writeMs := config.GetMemoryConfig().Timeouts.WriteMs
	if writeMs <= 0 {
		writeMs = config.DefaultMemoryWriteTimeoutMs
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(writeMs)*time.Millisecond)
		defer cancel()
		err := mem.Add(ctx, AddMemoryInput{
			Scope: MemoryScope{Principal: principal},
			Messages: []MemoryMessage{
				{Role: "user", Content: userText},
				{Role: "assistant", Content: assistantText},
			},
		})
		if err != nil {
			// failOpenMemoryProvider already swallows backend errors; this covers a
			// non-fail-open configuration so a capture failure never panics a bare goroutine.
			logger.Warnf("[Memory] capture failed (dropped): %v", err)
		}
	}()
}
