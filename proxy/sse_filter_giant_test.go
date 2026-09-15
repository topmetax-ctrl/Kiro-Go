package proxy

// ssePublicFilter giant-frame policy and the observer's bounded-memory
// properties.
//
// The filter must never damage a legitimate giant content frame (Responses'
// response.completed/response.incomplete carry the whole generated output),
// while still bounding its own buffer against garbage that never terminates a
// frame. These tests pin both sides, plus the chunk-boundary property: the
// canonical usage must be identical no matter how the relay's reads split the
// bytes.

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"kiro-go/providererr"
)

// captureWriter collects everything written, like a client ResponseWriter.
type captureWriter struct {
	b bytes.Buffer
}

func (w *captureWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

// writeFilterInChunks feeds p to the filter in the given chunk size, returning
// everything the "client" received.
func writeFilterInChunks(t *testing.T, f *ssePublicFilter, p []byte, chunk int) []byte {
	t.Helper()
	for len(p) > 0 {
		n := chunk
		if n > len(p) {
			n = len(p)
		}
		if _, err := f.Write(p[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		p = p[n:]
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return f.dst.(*captureWriter).b.Bytes()
}

func newTestFilter() *ssePublicFilter {
	return &ssePublicFilter{
		dst:     &captureWriter{},
		flusher: nopFlusher{},
		dialect: providererr.DialectResponses,
	}
}

type nopFlusher struct{}

func (nopFlusher) Flush() {}

// A legit giant response.completed frame passes through BYTE-FOR-BYTE. This is
// the regression test for the old wholesale replacement, which spliced a
// public error into the middle of a healthy stream.
func TestSSEFilterGiantCompletedFrameVerbatim(t *testing.T) {
	frame := `event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"output":[{"text":"` +
		strings.Repeat("x", 100*1024) + `"}],"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"
	want := []byte(frame)
	for _, chunk := range []int{16 * 1024, 1024, 7} {
		got := writeFilterInChunks(t, newTestFilter(), want, chunk)
		if !bytes.Equal(got, want) {
			t.Fatalf("chunk=%d: giant completed frame corrupted: got %d bytes, want %d (contains public error: %v)",
				chunk, len(got), len(want), bytes.Contains(got, []byte(`"error"`)))
		}
	}
}

// Same for the data-only form: the filter must look past "data:" at the frame's
// root type, not just at the event: line.
func TestSSEFilterGiantDataOnlyCompletedVerbatim(t *testing.T) {
	frame := `data: {"type":"response.completed","response":{"output":"` +
		strings.Repeat("x", 100*1024) + `","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"
	got := writeFilterInChunks(t, newTestFilter(), []byte(frame), 16*1024)
	if !bytes.Equal(got, []byte(frame)) {
		t.Fatalf("data-only giant completed frame corrupted: got %d bytes, want %d", len(got), len(frame))
	}
}

// An oversized frame whose head names response.failed keeps the replacement:
// the client still gets the public error, upstream detail stays suppressed,
// and the buffer bound still holds.
func TestSSEFilterGiantFailedFrameReplaced(t *testing.T) {
	marker := "upstream-secret-detail-should-not-leak"
	frame := `event: response.failed` + "\n" + `data: {"type":"response.failed","response":{"error":{"message":"` +
		marker + `","x":"` + strings.Repeat("y", 100*1024) + `"}}}` + "\n\n"
	got := writeFilterInChunks(t, newTestFilter(), []byte(frame), 16*1024)
	if !bytes.Contains(got, []byte(`"response.failed"`)) || !bytes.Contains(got, []byte(`"status":"failed"`)) {
		t.Errorf("giant error frame must be replaced with the public Responses error, got %.120s", got)
	}
	if bytes.Contains(got, []byte(marker)) {
		t.Error("oversized error frame leaked upstream detail")
	}
}

// Garbage that never terminates a frame keeps the old replacement — the memory
// bound is what that branch exists for.
func TestSSEFilterGiantGarbageReplaced(t *testing.T) {
	frame := strings.Repeat("junk ", 30*1024) // no data:/event:, no \n\n
	got := writeFilterInChunks(t, newTestFilter(), []byte(frame), 16*1024)
	if !bytes.Contains(got, []byte(`"error"`)) {
		t.Errorf("unterminated garbage must be replaced with the public error, got %.80s", got)
	}
}

// The bypass must end when the giant frame's terminator arrives — split across
// writes, even — and normal frame inspection resumes after it.
func TestSSEFilterBypassEndsAndInspectionResumes(t *testing.T) {
	giant := `data: {"type":"response.completed","response":{"output":"` +
		strings.Repeat("x", 100*1024) + `"}}` + "\n\n"
	smallErr := `event: error` + "\n" + `data: {"type":"error","error":{"type":"overloaded_error"}}` + "\n\n"
	stream := "event: response.created\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" + giant + smallErr

	f := newTestFilter()
	// Deliver the giant frame's FINAL '\n' alone, so its terminator "\n\n" is
	// split across two writes mid-bypass.
	giantLastNL := strings.Index(stream, giant) + len(giant) - 1
	p := []byte(stream)
	for i := 0; i < len(p); {
		n := 16 * 1024
		if i == giantLastNL {
			n = 1
		} else if i < giantLastNL && giantLastNL < i+n {
			n = giantLastNL - i // end the chunk right before the isolated byte
		}
		if n > len(p)-i {
			n = len(p) - i
		}
		if _, err := f.Write(p[i : i+n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		i += n
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := f.dst.(*captureWriter).b.Bytes()
	if !bytes.Contains(got, []byte("event: response.created")) {
		t.Error("pre-giant frames lost")
	}
	if !bytes.Contains(got, []byte(`"response.completed"`)) {
		t.Error("giant completed frame lost")
	}
	if !bytes.Contains(got, []byte(`"type":"response.failed"`)) {
		t.Error("the small error frame AFTER the giant frame must still be rewritten to the public shape")
	}
	if bytes.Contains(got, []byte("overloaded_error")) {
		t.Error("upstream error type leaked after bypass ended")
	}
}

// One-byte chunks through a whole mixed stream must reproduce the input
// byte-for-byte: every pendNL/carry edge in bypassWrite, every state-machine
// boundary in the scanner, exercised at once.
func TestSSEFilterOneByteChunksVerbatim(t *testing.T) {
	stream := "event: response.created\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"output":[{"text":"` +
		strings.Repeat("x", 70*1024) + `"}],"usage":{"input_tokens":9,"output_tokens":8}}}` + "\n\n" +
		"data: {\"type\":\"response.output_text.done\"}\n\n"
	got := writeFilterInChunks(t, newTestFilter(), []byte(stream), 1)
	if !bytes.Equal(got, []byte(stream)) {
		t.Fatalf("1-byte chunking corrupted the stream: got %d bytes, want %d", len(got), len(stream))
	}
}

// Chunk-boundary property: however the relay's reads split the bytes, the
// scanner's canonical usage and terminal verdict are identical.
func TestUsageScannerChunkBoundaryProperty(t *testing.T) {
	stream := "event: response.created\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n" +
		responsesCompletedFrame(80*1024)
	want := (&usageScanner{}).Counts()
	{
		s := &usageScanner{}
		_, _ = s.Write([]byte(stream))
		want = s.Counts()
		if want.Input == nil || *want.Input != 2000 {
			t.Fatalf("reference run broken: %+v", want)
		}
	}
	for _, chunk := range []int{1, 2, 3, 7, 64, 511, 1500, 8192, 16384, 65536} {
		s := &usageScanner{}
		p := []byte(stream)
		for len(p) > 0 {
			n := chunk
			if n > len(p) {
				n = len(p)
			}
			_, _ = s.Write(p[:n])
			p = p[n:]
		}
		got := s.Counts()
		if !reflect.DeepEqual(got.Input, want.Input) || !reflect.DeepEqual(got.Output, want.Output) ||
			!reflect.DeepEqual(got.CacheReadInputTokens, want.CacheReadInputTokens) ||
			!reflect.DeepEqual(got.ReasoningOutputTokens, want.ReasoningOutputTokens) ||
			got.Protocol != want.Protocol {
			t.Errorf("chunk=%d: usage diverged: got %+v want %+v", chunk, got, want)
		}
		if s.Terminal() != terminalCompleted {
			t.Errorf("chunk=%d: Terminal() = %v, want completed", chunk, s.Terminal())
		}
	}
}

// Degradation counters: every way the observer gives up on usage it saw
// evidence of must leave an aggregate signal. Both cases pad the frame past
// the scan buffer, and both are fed in relay-sized chunks — a line that
// arrives complete in one write goes down the small-frame path, which has no
// give-up points to count.
func TestUsageObserverDegradeCounters(t *testing.T) {
	before := UsageObserverDegradeTotals()
	pad := strings.Repeat("x", 70*1024)

	feed := func(t *testing.T, sse string) *usageScanner {
		t.Helper()
		s := &usageScanner{}
		p := []byte(sse)
		for len(p) > 0 {
			n := 16 * 1024
			if n > len(p) {
				n = len(p)
			}
			if _, err := s.Write(p[:n]); err != nil {
				t.Fatalf("Write: %v", err)
			}
			p = p[n:]
		}
		s.Counts()
		return s
	}

	// oversized: a usage object larger than maxUsageCapture is abandoned whole.
	huge := `data: {"type":"response.completed","response":{"output":[{"text":"` + pad +
		`"}],"usage":{"input_tokens":1,"notes":"` + strings.Repeat("n", maxUsageCapture+1024) + `"}}}` + "\n\n"
	feed(t, huge)

	// malformed: a captured object whose bytes do not decode as JSON.
	bad := `data: {"type":"response.completed","response":{"output":[{"text":"` + pad +
		`"}],"usage":{"input_tokens":1 "broken":2}}}` + "\n\n"
	feed(t, bad)

	after := UsageObserverDegradeTotals()
	if after[degradeOversizedUsage] != before[degradeOversizedUsage]+1 {
		t.Errorf("oversized counter: before=%d after=%d, want +1", before[degradeOversizedUsage], after[degradeOversizedUsage])
	}
	if after[degradeMalformedUsage] != before[degradeMalformedUsage]+1 {
		t.Errorf("malformed counter: before=%d after=%d, want +1", before[degradeMalformedUsage], after[degradeMalformedUsage])
	}
}
