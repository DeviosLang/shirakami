package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

const (
	defaultCacheMaxSize = 500           // max cached entries per task
	defaultCacheTTL     = 30 * time.Minute // entry lifetime
)

// toolCache is a thread-safe fixed-size cache for tool execution results.
// Entries expire after TTL or when the cache is full (LRU eviction).
// Designed to live for one analysis task's lifetime and then be discarded.
type toolCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	order   []string // insertion/access order; front = oldest, back = most-recent
	maxSize int
	ttl     time.Duration
}

type cacheEntry struct {
	result    string
	expiresAt time.Time
}

// newToolCache creates a toolCache with the given capacity and TTL.
func newToolCache(maxSize int, ttl time.Duration) *toolCache {
	return &toolCache{
		entries: make(map[string]*cacheEntry, maxSize),
		order:   make([]string, 0, maxSize),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// cacheKey returns a 16-byte hex digest of "toolName:args".
func cacheKey(name string, args json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte(":"))
	h.Write(args)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// get returns the cached result for key, or ("", false) on miss / expiry.
func (c *toolCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		// expired — remove eagerly
		delete(c.entries, key)
		c.removeFromOrder(key)
		return "", false
	}
	// promote to back (most-recently used)
	c.removeFromOrder(key)
	c.order = append(c.order, key)
	return e.result, true
}

// set stores a result, evicting stale/LRU entries as needed.
func (c *toolCache) set(key string, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If already present, just update value and promote.
	if _, ok := c.entries[key]; ok {
		c.entries[key] = &cacheEntry{result: value, expiresAt: time.Now().Add(c.ttl)}
		c.removeFromOrder(key)
		c.order = append(c.order, key)
		return
	}

	// Evict if at capacity.
	if len(c.entries) >= c.maxSize {
		c.evictExpiredOrLRU()
	}

	c.entries[key] = &cacheEntry{result: value, expiresAt: time.Now().Add(c.ttl)}
	c.order = append(c.order, key)
}

// evictExpiredOrLRU clears all expired entries first; if still at capacity,
// removes the least-recently-used (front of order slice) entry.
// Caller must hold c.mu.
func (c *toolCache) evictExpiredOrLRU() {
	now := time.Now()

	// Pass 1: collect expired keys.
	expired := make([]string, 0)
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			expired = append(expired, k)
		}
	}
	for _, k := range expired {
		delete(c.entries, k)
		c.removeFromOrder(k)
	}

	// Pass 2: if still full, evict LRU.
	if len(c.entries) >= c.maxSize && len(c.order) > 0 {
		lru := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, lru)
	}
}

// removeFromOrder removes key from the order slice (linear scan; order is
// bounded by maxSize which is typically ≤ 500, so this is acceptable).
// Caller must hold c.mu.
func (c *toolCache) removeFromOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}
