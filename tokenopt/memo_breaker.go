package tokenopt

import (
	"container/list"
	"crypto/sha256"
	"sync"
	"time"
)

// memo remembers the text an engine produced for a tool result, keyed by the
// content, the caller and the engine settings. It exists for one reason: the
// client re-sends the whole conversation, uncompressed, on every turn, so
// without it each turn would re-ask the engine about old results and could get
// a different answer, silently rewriting the cached prefix. Replaying the first
// answer keeps old messages byte-identical from turn to turn, and skips the
// engine call entirely when nothing new arrived.
//
// It is process-local and bounded: a restart costs one recompression, never
// correctness.
type memo struct {
	mu       sync.Mutex
	maxN     int
	maxBytes int
	bytes    int
	ll       *list.List
	m        map[[32]byte]*list.Element
}

type memoEntry struct {
	key [32]byte
	val string
}

func newMemo(maxEntries, maxBytes int) *memo {
	return &memo{maxN: maxEntries, maxBytes: maxBytes, ll: list.New(), m: map[[32]byte]*list.Element{}}
}

// memoKey scopes an entry to the caller (so one API key can never influence
// what another is served) and to the engine settings that shaped the output.
func memoKey(principal, fingerprint, text string) [32]byte {
	h := sha256.New()
	h.Write([]byte(principal))
	h.Write([]byte{0})
	h.Write([]byte(fingerprint))
	h.Write([]byte{0})
	h.Write([]byte(text))
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

func (c *memo) get(k [32]byte) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[k]
	if !ok {
		return "", false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*memoEntry).val, true
}

func (c *memo) put(k [32]byte, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		e := el.Value.(*memoEntry)
		c.bytes += len(v) - len(e.val)
		e.val = v
		c.ll.MoveToFront(el)
	} else {
		c.m[k] = c.ll.PushFront(&memoEntry{key: k, val: v})
		c.bytes += len(v)
	}
	for c.ll.Len() > c.maxN || c.bytes > c.maxBytes {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		e := oldest.Value.(*memoEntry)
		c.ll.Remove(oldest)
		delete(c.m, e.key)
		c.bytes -= len(e.val)
	}
}

func (c *memo) stats() (entries, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len(), c.bytes
}

// breaker stops calling an engine that keeps failing, so a dead sidecar costs
// each request nothing instead of a timeout. It trips after `threshold`
// consecutive failures, stays open for `cooldown`, then lets exactly one trial
// call through (half-open).
type breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	failures int
	openedAt time.Time
	probing  bool
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// allow reports whether a call may proceed. Every true must be followed by
// exactly one of success, failure or release.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openedAt.IsZero() {
		return true
	}
	if !b.probing && b.now().Sub(b.openedAt) >= b.cooldown {
		b.probing = true
		return true
	}
	return false
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures, b.openedAt, b.probing = 0, time.Time{}, false
}

func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.probing = false
	if b.failures >= b.threshold {
		b.openedAt = b.now()
	}
}

// release hands back a permit without judging the engine (canceled or skipped).
func (b *breaker) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

func (b *breaker) state() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.openedAt.IsZero():
		return "closed"
	case b.now().Sub(b.openedAt) >= b.cooldown:
		return "half-open"
	default:
		return "open"
	}
}
