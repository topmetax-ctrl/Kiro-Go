// Package cachetrace turns an Anthropic Messages request body into the keyed
// digests recorded on a forward event (metrics.CacheTrace).
//
// Prompt caching is prefix based: the upstream hashes tools, then system, then
// messages up to a breakpoint, and a request only reads what an earlier request
// wrote under the same hash. When a cache write is unexpectedly large, the
// question is which part of that prefix differs from the previous request of the
// same conversation. The digests here answer it without storing any prompt
// text: they are truncated HMAC-SHA256 values under a per-install key, so they
// compare equal across requests and reveal nothing else.
//
// Hashing is linear in the body size and must never fail a request: every
// problem yields a nil trace.
package cachetrace

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"strconv"

	"kiro-go/metrics"
)

const (
	// digestBytes is the digest length kept: 96 bits. A false match between two
	// requests is out of reach at any ring size, and the key makes a digest
	// useless for confirming a guess at the content.
	digestBytes = 12

	// tailCheckpoints is how many of the newest message boundaries are always
	// listed. The next request of a conversation normally extends this one by
	// one to three messages, so its own tail overlaps this request's last
	// checkpoint.
	tailCheckpoints = 4

	// strideStep and maxStrides shape the sparse checkpoints over the older
	// part of the history: one every strideStep messages, widened in whole steps
	// so a very long conversation never lists more than maxStrides of them.
	strideStep = 16
	maxStrides = 24

	// maxMarks bounds the breakpoint list. The API allows four per request.
	maxMarks = 16

	// maxLabel bounds a ttl or scope value copied from the body into a mark.
	maxLabel = 16
)

// domain separates this key from every other use of the secret it derives from.
const domain = "kiro-go/cache-trace/v1"

// Tracer computes cache traces under one key. The zero value and a nil *Tracer
// are valid and produce no traces. It is safe for concurrent use.
type Tracer struct {
	key []byte
}

// New derives a Tracer from a long-lived secret. The tracing key is a keyed
// hash of a fixed label under that secret, so the digests cannot be used to test
// guesses against anything else the secret protects. An empty secret returns nil.
func New(secret []byte) *Tracer {
	if len(secret) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(domain))
	return &Tracer{key: m.Sum(nil)}
}

// Trace digests a Messages request body. policy labels the Token Optimization
// settings in force and is recorded as given. It returns nil when the body is
// not a Messages request with at least one message.
func (t *Tracer) Trace(body []byte, policy string) *metrics.CacheTrace {
	if t == nil || len(t.key) == 0 {
		return nil
	}
	var req struct {
		Model        json.RawMessage   `json:"model"`
		Tools        json.RawMessage   `json:"tools"`
		System       json.RawMessage   `json:"system"`
		Messages     []json.RawMessage `json:"messages"`
		Thinking     json.RawMessage   `json:"thinking"`
		OutputConfig json.RawMessage   `json:"output_config"`
		ToolChoice   json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return nil
	}

	toolsSum := t.sum(stripMarks(req.Tools))
	systemSum := t.sum(stripMarks(req.System))
	tr := &metrics.CacheTrace{
		Msgs:     len(req.Messages),
		Settings: short(t.sum(req.Model, req.Thinking, req.OutputConfig, req.ToolChoice)),
		Policy:   policy,
	}
	if len(req.Tools) > 0 {
		tr.Tools = short(toolsSum)
	}
	if len(req.System) > 0 {
		tr.System = short(systemSum)
	}

	// The prefix chain folds tools, system and each message digest in order, so a
	// checkpoint at k covers exactly what the upstream hashes for a breakpoint
	// after message k. Settings stay out of it: they invalidate cached messages
	// by their own rule, and a separate digest says which one changed.
	chain := hmac.New(sha256.New, t.key)
	chain.Write(toolsSum)
	chain.Write(systemSum)
	var marked []int
	tr.Marks, marked = marks(req.Tools, req.System, req.Messages)
	want := checkpoints(len(req.Messages))
	for _, k := range marked {
		want[k] = true
	}
	for i, m := range req.Messages {
		sum := t.sum(stripMarks(m))
		if i == 0 {
			tr.Conv = short(sum)
		}
		chain.Write(sum)
		if want[i+1] {
			tr.Prefix = append(tr.Prefix, strconv.Itoa(i+1)+":"+short(chainSum(chain)))
		}
	}
	return tr
}

// sum is the keyed digest of parts, each length-prefixed so adjacent parts
// cannot be confused with one longer part.
func (t *Tracer) sum(parts ...[]byte) []byte {
	m := hmac.New(sha256.New, t.key)
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		m.Write(n[:])
		m.Write(p)
	}
	return m.Sum(nil)
}

// chainSum reads the running digest without disturbing it (hash.Hash.Sum
// appends the current state and leaves it intact).
func chainSum(h hash.Hash) []byte { return h.Sum(nil) }

func short(sum []byte) string { return hex.EncodeToString(sum[:digestBytes]) }

// checkpoints picks the message counts k whose prefix digest is recorded: the
// newest few, plus a sparse stride over the rest.
func checkpoints(n int) map[int]bool {
	out := make(map[int]bool)
	for k := n; k > 0 && k > n-tailCheckpoints; k-- {
		out[k] = true
	}
	step := strideStep
	for n/step > maxStrides {
		step += strideStep
	}
	for k := step; k <= n; k += step {
		out[k] = true
	}
	return out
}

var (
	cacheControlKey = []byte(`"cache_control"`)
	nullLiteral     = []byte("null")
)

// stripMarks returns b without its cache_control members, so a block digests the
// same whether or not it carries a breakpoint. Claude Code moves its breakpoint
// to the newest message every turn; if the marker counted, the older message
// would hash differently the turn after, and no two requests would ever match.
//
// Only a member whose value is a flat object (or null) is removed, together with
// exactly one adjacent comma, which is the shape every client sends. Anything
// else is left alone: a digest that then differs is a visible miss, never a
// silent wrong match. The key can only appear unescaped as a JSON key, so text
// that merely mentions it (escaped inside a string) is never touched.
func stripMarks(b []byte) []byte {
	if !bytes.Contains(b, cacheControlKey) {
		return b
	}
	out := make([]byte, 0, len(b))
	from, search := 0, 0
	for {
		rel := bytes.Index(b[search:], cacheControlKey)
		if rel < 0 {
			break
		}
		start := search + rel
		end, ok := memberEnd(b, start+len(cacheControlKey))
		if !ok {
			search = start + len(cacheControlKey)
			continue
		}
		cutStart, cutEnd := start, end
		if k := skipSpaceBack(b, cutStart-1, from); k >= from && b[k] == ',' {
			cutStart = k
		} else if k := skipSpace(b, cutEnd); k < len(b) && b[k] == ',' {
			cutEnd = k + 1
		}
		out = append(out, b[from:cutStart]...)
		from, search = cutEnd, cutEnd
	}
	return append(out, b[from:]...)
}

// memberEnd reports where the value of a "cache_control" member ends, given the
// offset just past the key. It accepts `:null` and `:{...}` with no nesting.
func memberEnd(b []byte, at int) (int, bool) {
	i := skipSpace(b, at)
	if i >= len(b) || b[i] != ':' {
		return 0, false
	}
	i = skipSpace(b, i+1)
	if bytes.HasPrefix(b[i:], nullLiteral) {
		return i + len(nullLiteral), true
	}
	if i >= len(b) || b[i] != '{' {
		return 0, false
	}
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '{':
			return 0, false
		case '"':
			for j++; j < len(b) && b[j] != '"'; j++ {
				if b[j] == '\\' {
					j++
				}
			}
		case '}':
			return j + 1, true
		}
	}
	return 0, false
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

// skipSpaceBack walks left from i over whitespace, stopping at floor. It returns
// the index of the first non-space byte, or floor-1 when there is none.
func skipSpaceBack(b []byte, i, floor int) int {
	for i >= floor && isSpace(b[i]) {
		i--
	}
	return i
}

// marks lists the cache breakpoints on top-level blocks, in the order the
// upstream evaluates them: tools, system, messages. It also returns, for each
// breakpoint on a message, the message count k whose prefix digest covers it, so
// the caller records that checkpoint: a mark names where the upstream caches,
// and the digest at that position names what.
func marks(tools, system json.RawMessage, messages []json.RawMessage) ([]string, []int) {
	var (
		out    []string
		bounds []int
	)
	add := func(prefix string, i int, raw []byte) bool {
		if len(out) >= maxMarks {
			return false
		}
		on, ttl, scope := markOf(raw)
		if !on {
			return false
		}
		label := prefix + strconv.Itoa(i)
		if ttl != "" {
			label += ":" + ttl
		}
		if scope != "" {
			label += "@" + scope
		}
		out = append(out, label)
		return true
	}
	for i, raw := range blocks(tools) {
		add("t", i, raw)
	}
	for i, raw := range blocks(system) {
		add("s", i, raw)
	}
	for i, m := range messages {
		if !bytes.Contains(m, cacheControlKey) {
			continue
		}
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		for _, raw := range blocks(msg.Content) {
			if add("m", i, raw) && (len(bounds) == 0 || bounds[len(bounds)-1] != i+1) {
				bounds = append(bounds, i+1)
			}
		}
	}
	return out, bounds
}

// blocks splits a JSON array into its elements; anything that is not an array
// (a string system prompt, an absent field) has none.
func blocks(raw json.RawMessage) []json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' {
		return nil
	}
	var out []json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// markOf reports whether a block carries a breakpoint, with the ttl and scope it
// names, if any. A null marker is no breakpoint.
func markOf(raw []byte) (on bool, ttl, scope string) {
	if !bytes.Contains(raw, cacheControlKey) {
		return false, "", ""
	}
	var b struct {
		CacheControl *struct {
			TTL   json.RawMessage `json:"ttl"`
			Scope json.RawMessage `json:"scope"`
		} `json:"cache_control"`
	}
	if json.Unmarshal(raw, &b) != nil || b.CacheControl == nil {
		return false, "", ""
	}
	return true, label(b.CacheControl.TTL), label(b.CacheControl.Scope)
}

// label reads a short enum-like string out of a marker. The body is client
// input, so anything that is not a short plain token is recorded as "?": the
// event stays bounded and carries no text the client chose to put there.
func label(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var v string
	if json.Unmarshal(raw, &v) != nil || v == "" {
		return "?"
	}
	if len(v) > maxLabel {
		return "?"
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return "?"
		}
	}
	return v
}
