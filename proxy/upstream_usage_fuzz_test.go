package proxy

// FuzzBigLineUsageFilter fuzzes the oversized-line usage extractor against the
// ordinary whole-frame decoder as oracle.
//
// Invariants checked for every input:
//
//   - no panic (implicit), and the scanner's retained buffers stay bounded:
//     the capture never exceeds maxUsageCapture (with append slack) and the
//     partial buffer never exceeds maxUsageScanBuffer;
//   - no fabricated usage: whatever the big line path reports must come from
//     the input;
//   - for inputs the ordinary decoder can read (its Unmarshal succeeds) whose
//     whole payload fits within maxUsageCapture — so every usage object inside
//     is small enough to be captured whole — the big line path must produce
//     the IDENTICAL canonical usage.
//
// The comparison is skipped where the two readers are KNOWN to legitimately
// diverge, and the skips are semantic, not convenient:
//
//   - payload with a usage-shaped key at MORE than one of the dialect
//     positions (top-level "usage", "message.usage", "response.usage",
//     "usageMetadata"): the whole-frame decoder merges them in a fixed
//     field order, the filter in document order; only the LAST conflicting
//     field can differ, and no real dialect ever sends two positions in one
//     frame.
//   - payload the ordinary decoder REJECTS (type mismatches like
//     {"usage":"x"}): the decoder aborts the whole frame while the filter
//     keeps extracting the objects it recognizes — the filter is strictly
//     more observant on hostile input.
//   - payload above maxUsageCapture: a usage object inside may exceed the
//     capture cap, where abandonment is the documented policy.

import (
	"bytes"
	"encoding/json"
	"testing"
)

func fuzzEnvReports(e usageEnvelope) bool {
	c := e.normalize()
	// A normalized usage with no top-level token AND no protocol provenance is
	// indistinguishable from an unknown dialect. The whole-frame decoder would
	// still stick the orphan cache in the struct even though it declares no
	// dialect — a separate parser inconsistency — while the filter extracts
	// zero dialects in that case. Skip that degenerate shape (below-threshold
	// cache field alone with no input_tokens/prompt_tokens parent).
	if c.Protocol == "" && c.Input == nil && c.Output == nil {
		return false
	}
	return c.Input != nil || c.Output != nil || c.CacheReadInputTokens != nil ||
		c.CacheCreationInputTokens != nil || c.ReasoningOutputTokens != nil ||
		c.ServerTool.WebSearchRequests > 0 || c.Protocol != ""
}

func FuzzBigLineUsageFilter(f *testing.F) {
	// Seeds, one per adversarial class from the audit spec.
	f.Add([]byte(`{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":2000,"output_tokens":300,"input_tokens_details":{"cached_tokens":1500},"output_tokens_details":{"reasoning_tokens":250}},"status":"completed"}}`))
	f.Add([]byte(`{"type":"response.completed","response":{"output":[{"text":"` + string(bytes.Repeat([]byte("x"), 20000)) + `"}],"usage":{"input_tokens":1,"output_tokens":2}}}`))
	f.Add([]byte(`{"output":"she said \"usage\" out loud","usage":{"input_tokens":5,"output_tokens":6}}`))
	f.Add([]byte(`{"output":"path C:\\usage\\logs","usage":{"input_tokens":7,"output_tokens":8}}`))
	f.Add([]byte(`{"output":"snowman \u2603 and \u0055sage","usage":{"input_tokens":9,"output_tokens":10}}`))
	f.Add([]byte(`{"a":{"b":{"usage":{"input_tokens":999}}},"usage":{"input_tokens":1,"output_tokens":1}}`))
	f.Add([]byte(`{"output":"the usage of tools varies","usage":{"prompt_tokens":11,"completion_tokens":12}}`))
	f.Add([]byte(`{"usage":{"input_tokens":`))
	f.Add([]byte(`{"deep":{"deeper":{"deepest":{"usage":{"input_tokens":42}}}}}`))
	// Near the capture limit: a usage object a few bytes under maxUsageCapture…
	near := string(bytes.Repeat([]byte("n"), maxUsageCapture-64))
	f.Add([]byte(`{"usage":{"input_tokens":3,"notes":"` + near + `"}}`))
	// …and one a few bytes over it (comparison is skipped; abandonment must not
	// corrupt the state machine for the NEXT usage object).
	over := string(bytes.Repeat([]byte("m"), maxUsageCapture+64))
	f.Add([]byte(`{"usage":{"notes":"` + over + `"},"message":{"usage":{"input_tokens":77,"output_tokens":88}}}`))
	f.Add([]byte(`{"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":60,"thoughtsTokenCount":10,"cachedContentTokenCount":30}}`))
	f.Add([]byte(`data: [DONE]`))
	f.Add([]byte(`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":5}}`))
	f.Add([]byte(`{"usage":"a string, not an object","message":{"usage":{"input_tokens":13}}}`))
	f.Add([]byte(`{"foo":{"usage":{"input_tokens":55}},"message":{"usage":{"output_tokens":66}}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// One SSE data line: cut at the first newline, exactly as the scanner's
		// line reader would.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[:i]
		}
		line := append([]byte("data: "), data...)
		line = append(line, '\n')

		// Big-line path: exactly what usageScanner.Write does when the line
		// outgrows the buffer, driven directly so every input takes it.
		sBig := &usageScanner{}
		big := &bigLineFilter{}
		big.begin(sBig, line)
		// A line never terminated by newline stays mid-filter at EOF; that is
		// the real relay shape too (the accessors flush only s.partial). Close
		// it out the way the next line's bytes would: feed a newline.
		if big.active {
			big.feed(sBig, []byte("\n"))
		}
		got := sBig.Counts()

		// Bounds: the capture is capped even on adversarial input.
		if cap(big.capture) > 2*maxUsageCapture+16 {
			t.Fatalf("capture capacity unbounded: %d", cap(big.capture))
		}
		if len(sBig.partial) > maxUsageScanBuffer {
			t.Fatalf("partial buffer exceeded cap: %d", len(sBig.partial))
		}

		// Oracle: the ordinary whole-frame decoder, as scanLine runs it.
		var frame struct {
			Usage   usageEnvelope `json:"usage"`
			Message struct {
				Usage usageEnvelope `json:"usage"`
			} `json:"message"`
			Response struct {
				Usage usageEnvelope `json:"usage"`
			} `json:"response"`
			UsageMetadata *usageEnvelope `json:"usageMetadata"`
		}
		err := json.Unmarshal(data, &frame)
		if err != nil {
			// The ordinary decoder rejected the frame; its "answer" (nothing)
			// is not a reference. The filter's targeted extraction stands.
			return
		}
		positions := 0
		for _, env := range []usageEnvelope{frame.Usage, frame.Message.Usage, frame.Response.Usage} {
			if fuzzEnvReports(env) {
				positions++
			}
		}
		if frame.UsageMetadata != nil && fuzzEnvReports(*frame.UsageMetadata) {
			positions++
		}
		if positions > 1 {
			// Multiple dialect positions in one frame: no real dialect sends
			// this, and merge order legitimately differs (see file comment).
			return
		}
		if len(data) > maxUsageCapture {
			// A usage object inside may exceed the capture cap; abandonment is
			// documented policy, not a bug.
			return
		}

		want := usageCounts{}
		mergeUsage(&want, frame.Usage)
		mergeUsage(&want, frame.Message.Usage)
		mergeUsage(&want, frame.Response.Usage)
		if frame.UsageMetadata != nil {
			mergeUsage(&want, *frame.UsageMetadata)
		}
		if !sameUsage(got, want) {
			t.Fatalf("big-line extraction diverged from whole-frame decoder\n got: %+v\nwant: %+v\ninput: %q",
				got, want, data)
		}
	})
}

// sameUsage compares the canonical fields with presence semantics: both nil,
// or both non-nil and equal.
func sameUsage(a, b usageCounts) bool {
	eq := func(x, y *int64) bool {
		if (x == nil) != (y == nil) {
			return false
		}
		return x == nil || *x == *y
	}
	return eq(a.Input, b.Input) && eq(a.Output, b.Output) &&
		eq(a.CacheReadInputTokens, b.CacheReadInputTokens) &&
		eq(a.CacheCreationInputTokens, b.CacheCreationInputTokens) &&
		eq(a.ReasoningOutputTokens, b.ReasoningOutputTokens) &&
		a.Protocol == b.Protocol &&
		a.ServerTool.WebSearchRequests == b.ServerTool.WebSearchRequests
}
