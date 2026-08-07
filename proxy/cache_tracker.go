package proxy

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultPromptCacheTTL = 5 * time.Minute

// Anthropic requires cached prefixes to reach a minimum token count before
// caching takes effect. Breakpoints below this threshold are excluded from
// matching and storage to avoid reporting unrealistic 100% cache hits on
// short requests. Sonnet caches from 1024 tokens; Opus and Haiku 4.5 require
// 4096. Getting this wrong only skews the synthesized cache numbers the proxy
// reports to clients — upstream caching is unaffected.
const defaultMinCacheableTokens = 1024
const highMinCacheableTokens = 4096

type promptCacheUsage struct {
	CacheCreationInputTokens   int
	CacheReadInputTokens       int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
}

type promptCacheBreakpoint struct {
	Fingerprint      [32]byte
	CumulativeTokens int
	TTL              time.Duration
}

type promptCacheProfile struct {
	Breakpoints      []promptCacheBreakpoint
	TotalInputTokens int
	Model            string
}

func minCacheableTokensForModel(model string) int {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "opus") || strings.Contains(lower, "haiku") {
		return highMinCacheableTokens
	}
	return defaultMinCacheableTokens
}

type promptCacheEntry struct {
	ExpiresAt time.Time
	TTL       time.Duration
}

// defaultPromptCacheCapacity bounds the total number of live cache entries across
// ALL accounts. The account ID is part of the key, so isolation is preserved while
// memory stays bounded regardless of account count (a single global cap, not one
// per account). ~50k entries × ~small-struct is a few MB.
const defaultPromptCacheCapacity = 50000

// promptCacheKey identifies a stored prefix by account and fingerprint. Including the
// account ID guarantees account A's prompt can never hit account B's entry.
type promptCacheKey struct {
	AccountID   string
	Fingerprint [32]byte
}

// lruEntry is the value stored in each list element.
type lruEntry struct {
	key   promptCacheKey
	entry promptCacheEntry
}

// promptCacheMetrics are cumulative counters for observability. They contain no
// prompt text, fingerprints, or token secrets — only counts.
type promptCacheMetrics struct {
	Hits         int64
	Misses       int64
	Creations    int64
	Evictions    int64
	ExpiredEvict int64
}

// promptCacheTracker is a bounded, O(1) LRU of prompt-prefix fingerprints used to
// synthesize Anthropic cache-usage metadata. It is NOT a model/inference cache and
// stores no computation — only (account, fingerprint) -> {expiry, ttl}.
//
// Previously this was a per-account nested map with an O(total entries) prune scan
// on every Compute/Update. It is now a single global map + container/list LRU:
// lookup/insert/update/move-to-front/evict are all O(1) amortized, memory is bounded
// by capacity, and there is no full-map scan on the hot path.
type promptCacheTracker struct {
	mu              sync.Mutex
	items           map[promptCacheKey]*list.Element
	lru             *list.List // front = most-recently-used
	perAccount      map[string]int
	capacity        int
	maxSupportedTTL time.Duration
	metrics         promptCacheMetrics
}

func newPromptCacheTracker(maxTTL time.Duration) *promptCacheTracker {
	return newPromptCacheTrackerWithCapacity(maxTTL, defaultPromptCacheCapacity)
}

func newPromptCacheTrackerWithCapacity(maxTTL time.Duration, capacity int) *promptCacheTracker {
	if maxTTL <= 0 {
		maxTTL = defaultPromptCacheTTL
	}
	if capacity <= 0 {
		capacity = defaultPromptCacheCapacity
	}
	return &promptCacheTracker{
		items:           make(map[promptCacheKey]*list.Element),
		lru:             list.New(),
		perAccount:      make(map[string]int),
		capacity:        capacity,
		maxSupportedTTL: maxTTL,
	}
}

// getLive returns the live (non-expired) entry for key, removing it if expired.
// Callers hold t.mu. A hit is moved to the LRU front.
func (t *promptCacheTracker) getLive(key promptCacheKey, now time.Time) (promptCacheEntry, bool) {
	el, ok := t.items[key]
	if !ok {
		return promptCacheEntry{}, false
	}
	le := el.Value.(*lruEntry)
	if !le.entry.ExpiresAt.After(now) {
		t.removeElementLocked(el)
		t.metrics.ExpiredEvict++
		return promptCacheEntry{}, false
	}
	t.lru.MoveToFront(el)
	return le.entry, true
}

// putLocked inserts or updates key's entry, moving it to the front and evicting the
// LRU tail if over capacity. Callers hold t.mu.
func (t *promptCacheTracker) putLocked(key promptCacheKey, entry promptCacheEntry) {
	if el, ok := t.items[key]; ok {
		le := el.Value.(*lruEntry)
		le.entry = entry
		t.lru.MoveToFront(el)
		return
	}
	el := t.lru.PushFront(&lruEntry{key: key, entry: entry})
	t.items[key] = el
	t.perAccount[key.AccountID]++
	if t.lru.Len() > t.capacity {
		t.evictOldestLocked()
	}
}

func (t *promptCacheTracker) evictOldestLocked() {
	el := t.lru.Back()
	if el == nil {
		return
	}
	t.removeElementLocked(el)
	t.metrics.Evictions++
}

func (t *promptCacheTracker) removeElementLocked(el *list.Element) {
	le := el.Value.(*lruEntry)
	t.lru.Remove(el)
	delete(t.items, le.key)
	if n := t.perAccount[le.key.AccountID]; n <= 1 {
		delete(t.perAccount, le.key.AccountID)
	} else {
		t.perAccount[le.key.AccountID] = n - 1
	}
}

// accountHasLiveEntries reports whether the account has at least one entry (live or
// not-yet-pruned). Because expired entries are pruned lazily on access, a stale
// count can linger; that is acceptable for the "first request" heuristic and never
// causes a cross-account hit (Compute still verifies each breakpoint by key).
func (t *promptCacheTracker) accountHasEntries(accountID string) bool {
	return t.perAccount[accountID] > 0
}

// Metrics returns a snapshot of the cumulative counters plus current size/capacity.
func (t *promptCacheTracker) Metrics() (m promptCacheMetrics, entries, capacity int) {
	if t == nil {
		return promptCacheMetrics{}, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.metrics, t.lru.Len(), t.capacity
}

func (t *promptCacheTracker) BuildClaudeProfile(req *ClaudeRequest, totalInputTokens int) *promptCacheProfile {
	blocks := flattenClaudeCacheBlocks(req)
	if len(blocks) == 0 {
		return nil
	}

	hasher := sha256.New()
	breakpoints := make([]promptCacheBreakpoint, 0)
	cumulativeTokens := 0
	var activeTTL time.Duration

	for _, block := range blocks {
		canonical := canonicalizeCacheValue(block.Value)
		writeHashChunk(hasher, canonical)
		cumulativeTokens += block.Tokens

		// Determine whether this block acts as a cache breakpoint:
		//   1) Explicit cache_control on the block itself.
		//   2) Once any explicit breakpoint has been seen, every message-end
		//      boundary becomes an implicit breakpoint so that multi-turn
		//      conversations can hit earlier stored prefixes.
		breakpointTTL := time.Duration(0)
		if block.TTL > 0 {
			breakpointTTL = block.TTL
			activeTTL = block.TTL
		} else if block.IsMessageEnd && activeTTL > 0 {
			breakpointTTL = activeTTL
		}

		if breakpointTTL <= 0 {
			continue
		}

		var fingerprint [32]byte
		copy(fingerprint[:], hasher.Sum(nil))
		breakpoints = append(breakpoints, promptCacheBreakpoint{
			Fingerprint:      fingerprint,
			CumulativeTokens: cumulativeTokens,
			TTL:              breakpointTTL,
		})
	}

	if len(breakpoints) == 0 {
		return nil
	}

	if totalInputTokens < cumulativeTokens {
		totalInputTokens = cumulativeTokens
	}

	return &promptCacheProfile{
		Breakpoints:      breakpoints,
		TotalInputTokens: totalInputTokens,
		Model:            req.Model,
	}
}

func (t *promptCacheTracker) Compute(accountID string, profile *promptCacheProfile) promptCacheUsage {
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" {
		return promptCacheUsage{}
	}

	minTokens := minCacheableTokensForModel(profile.Model)
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	lastTokens := minInt(last.CumulativeTokens, profile.TotalInputTokens)
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.accountHasEntries(accountID) {
		// First request for this account: no prior prefix to match (a miss).
		t.metrics.Misses++
		effectiveCreation := lastTokens
		if effectiveCreation < minTokens {
			effectiveCreation = 0
		}
		cache5m, cache1h := computePromptCacheTTLBreakdown(profile, 0)
		return promptCacheUsage{
			CacheCreationInputTokens:   effectiveCreation,
			CacheReadInputTokens:       0,
			CacheCreation5mInputTokens: cache5m,
			CacheCreation1hInputTokens: cache1h,
		}
	}

	// Cap cacheable tokens at 85% of total input to ensure a realistic
	// uncached portion. The newest content in a request is never fully
	// served from cache on the current turn.
	maxCacheable := int(float64(profile.TotalInputTokens) * 0.85)
	if lastTokens > maxCacheable {
		lastTokens = maxCacheable
	}

	matchedTokens := 0
	for i := len(profile.Breakpoints) - 1; i >= 0; i-- {
		breakpoint := profile.Breakpoints[i]
		// Skip breakpoints below the minimum cacheable token threshold.
		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		key := promptCacheKey{AccountID: accountID, Fingerprint: breakpoint.Fingerprint}
		entry, ok := t.getLive(key, now)
		if !ok {
			continue
		}
		// Refresh the entry's expiry (a hit extends its life) and keep it hot.
		entry.ExpiresAt = now.Add(entry.TTL)
		t.putLocked(key, entry)
		matchedTokens = minInt(breakpoint.CumulativeTokens, profile.TotalInputTokens)
		if matchedTokens > lastTokens {
			matchedTokens = lastTokens
		}
		break
	}
	if matchedTokens > 0 {
		t.metrics.Hits++
	} else {
		t.metrics.Misses++
	}

	creation := maxInt(lastTokens-matchedTokens, 0)
	cache5m, cache1h := computePromptCacheTTLBreakdown(profile, matchedTokens)
	return promptCacheUsage{
		CacheCreationInputTokens:   creation,
		CacheReadInputTokens:       matchedTokens,
		CacheCreation5mInputTokens: cache5m,
		CacheCreation1hInputTokens: cache1h,
	}
}

func (t *promptCacheTracker) Update(accountID string, profile *promptCacheProfile) {
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" {
		return
	}

	minTokens := minCacheableTokensForModel(profile.Model)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, breakpoint := range profile.Breakpoints {
		// Skip breakpoints below the minimum cacheable token threshold.
		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		key := promptCacheKey{AccountID: accountID, Fingerprint: breakpoint.Fingerprint}
		_, existed := t.items[key]
		t.putLocked(key, promptCacheEntry{
			ExpiresAt: now.Add(breakpoint.TTL),
			TTL:       breakpoint.TTL,
		})
		if !existed {
			t.metrics.Creations++
		}
	}
}

type cacheablePromptBlock struct {
	Value        interface{}
	Tokens       int
	TTL          time.Duration
	IsMessageEnd bool
}

func flattenClaudeCacheBlocks(req *ClaudeRequest) []cacheablePromptBlock {
	blocks := make([]cacheablePromptBlock, 0)
	blocks = append(blocks, buildCachePreludeBlock(req))

	for toolIndex, tool := range req.Tools {
		toolValue := map[string]interface{}{
			"kind":         "tool",
			"tool_index":   toolIndex,
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
		}
		fingerprintValue := stripCachePositionKeys(toolValue)
		blocks = append(blocks, cacheablePromptBlock{
			Value:  fingerprintValue,
			Tokens: estimateApproxTokens(canonicalizeCacheValue(fingerprintValue)),
			TTL:    normalizePromptCacheTTL(extractPromptCacheTTL(tool)),
		})
	}

	appendSystemCacheBlocks(&blocks, req.System)

	for messageIndex, msg := range req.Messages {
		appendMessageCacheBlocks(&blocks, messageIndex, msg)
	}

	return blocks
}

func buildCachePreludeBlock(req *ClaudeRequest) cacheablePromptBlock {
	prelude := map[string]interface{}{
		"kind":        "request_prelude",
		"model":       req.Model,
		"tool_choice": req.ToolChoice,
	}
	return cacheablePromptBlock{
		Value:  prelude,
		Tokens: estimateApproxTokens(canonicalizeCacheValue(prelude)),
	}
}

func appendSystemCacheBlocks(blocks *[]cacheablePromptBlock, system interface{}) {
	switch v := system.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":         "system",
			"system_index": 0,
			"block": map[string]interface{}{
				"type": "text",
				"text": v,
			},
		}, false)
	case []interface{}:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block":        block,
			}, false)
		}
	case []string:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block": map[string]interface{}{
					"type": "text",
					"text": block,
				},
			}, false)
		}
	}
}

func appendMessageCacheBlocks(blocks *[]cacheablePromptBlock, messageIndex int, msg ClaudeMessage) {
	role := msg.Role
	switch content := msg.Content.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":          "message",
			"message_index": messageIndex,
			"role":          role,
			"block_index":   0,
			"block": map[string]interface{}{
				"type": "text",
				"text": content,
			},
		}, true)
	case []interface{}:
		lastIdx := len(content) - 1
		for blockIndex, block := range content {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   blockIndex,
				"block":         block,
			}, blockIndex == lastIdx)
		}
	default:
		if content != nil {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   0,
				"block":         content,
			}, true)
		}
	}
}

func appendPromptBlock(blocks *[]cacheablePromptBlock, wrapper map[string]interface{}, isMessageEnd bool) {
	blockValue := wrapper["block"]
	ttl := normalizePromptCacheTTL(extractPromptCacheTTL(blockValue))

	// Drop volatile billing metadata from the cache fingerprint. Claude Code's
	// x-anthropic-billing-header can drift, appear, or disappear across
	// otherwise identical requests, and it does not change model semantics.
	if isAnthropicBillingHeaderBlock(blockValue) {
		return
	}

	fingerprintValue := stripCachePositionKeys(wrapper)
	canonical := canonicalizeCacheValue(fingerprintValue)
	*blocks = append(*blocks, cacheablePromptBlock{
		Value:        fingerprintValue,
		Tokens:       estimateApproxTokens(canonical),
		TTL:          ttl,
		IsMessageEnd: isMessageEnd,
	})
}

func stripCachePositionKeys(value map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(value))
	for key, item := range value {
		if isCachePositionKey(key) {
			continue
		}
		cloned[key] = item
	}
	return cloned
}

func isAnthropicBillingHeaderBlock(value interface{}) bool {
	blockMap, ok := value.(map[string]interface{})
	if !ok {
		return false
	}

	// Only normalize text blocks (or blocks without an explicit type but containing text).
	if t, ok := blockMap["type"].(string); ok && t != "" && t != "text" {
		return false
	}

	text, ok := blockMap["text"].(string)
	if !ok {
		return false
	}

	trimmed := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(strings.ToLower(trimmed), "x-anthropic-billing-header:")
}

func extractPromptCacheTTL(value interface{}) time.Duration {
	block, ok := value.(map[string]interface{})
	if !ok {
		if raw, err := json.Marshal(value); err == nil {
			var decoded map[string]interface{}
			if json.Unmarshal(raw, &decoded) == nil {
				block = decoded
				ok = true
			}
		}
	}
	if !ok {
		return 0
	}

	rawCache, ok := block["cache_control"]
	if !ok {
		return 0
	}
	cacheControl, ok := rawCache.(map[string]interface{})
	if !ok {
		return 0
	}
	cacheType, _ := cacheControl["type"].(string)
	if !strings.EqualFold(cacheType, "ephemeral") {
		return 0
	}

	if ttl, ok := parsePromptCacheTTLValue(cacheControl["ttl"]); ok {
		return ttl
	}
	return defaultPromptCacheTTL
}

func parsePromptCacheTTLValue(value interface{}) (time.Duration, bool) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(v))
		if trimmed == "" {
			return 0, false
		}
		if d, err := time.ParseDuration(trimmed); err == nil {
			return d, true
		}
		if seconds, err := strconv.Atoi(trimmed); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
	case float64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	}
	return 0, false
}

func normalizePromptCacheTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 0
	}
	if ttl > time.Hour {
		return time.Hour
	}
	if ttl > defaultPromptCacheTTL {
		return time.Hour
	}
	return defaultPromptCacheTTL
}

func computePromptCacheTTLBreakdown(profile *promptCacheProfile, matchedTokens int) (int, int) {
	if profile == nil || len(profile.Breakpoints) == 0 {
		return 0, 0
	}

	cache5m := 0
	cache1h := 0
	previous := matchedTokens
	for _, breakpoint := range profile.Breakpoints {
		current := minInt(breakpoint.CumulativeTokens, profile.TotalInputTokens)
		if current <= previous {
			continue
		}
		delta := current - previous
		if breakpoint.TTL >= time.Hour {
			cache1h += delta
		} else {
			cache5m += delta
		}
		previous = current
	}
	return cache5m, cache1h
}

func billedClaudeInputTokens(inputTokens int, usage promptCacheUsage) int {
	return maxInt(inputTokens-usage.CacheCreationInputTokens-usage.CacheReadInputTokens, 0)
}

func buildClaudeUsageMap(inputTokens, outputTokens int, usage promptCacheUsage, includeCache bool) map[string]interface{} {
	result := map[string]interface{}{
		"input_tokens":  billedClaudeInputTokens(inputTokens, usage),
		"output_tokens": outputTokens,
	}
	if !includeCache {
		return result
	}
	result["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	result["cache_read_input_tokens"] = usage.CacheReadInputTokens
	result["cache_creation"] = map[string]int{
		"ephemeral_5m_input_tokens": usage.CacheCreation5mInputTokens,
		"ephemeral_1h_input_tokens": usage.CacheCreation1hInputTokens,
	}
	return result
}

func canonicalizeCacheValue(value interface{}) string {
	var buf bytes.Buffer
	writeCanonicalJSON(&buf, value)
	return buf.String()
}

func writeCanonicalJSON(buf *bytes.Buffer, value interface{}) {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalJSON(buf, item)
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		buf.WriteByte('{')
		keys := make([]string, 0, len(v))
		for key := range v {
			if key == "cache_control" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			buf.Write(encoded)
			buf.WriteByte(':')
			writeCanonicalJSON(buf, v[key])
		}
		buf.WriteByte('}')
	default:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	}
}

func isCachePositionKey(key string) bool {
	switch key {
	case "tool_index", "system_index", "message_index", "block_index":
		return true
	default:
		return false
	}
}

func writeHashChunk(hasher hashWriter, chunk string) {
	length := strconv.Itoa(len(chunk))
	hasher.Write([]byte(length))
	hasher.Write([]byte{0})
	hasher.Write([]byte(chunk))
	hasher.Write([]byte{0})
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
