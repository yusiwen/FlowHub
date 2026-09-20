// Package dedupe implements the in-memory idempotency cache for webhook
// deliveries.
//
// The published Webhook Triggers app sends no delivery id and never retries, so
// duplicates can still appear through network level retries or through nginx
// proxying a request twice. The cache is the only thing standing between a
// repeated delivery and a repeated agent run.
package dedupe

import (
	"sync"
	"time"
)

// Cache is a TTL map of seen idempotency keys.
type Cache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]time.Time
	now     func() time.Time
}

// New returns a cache that remembers keys for ttl. A non-positive ttl means
// nothing is ever remembered.
func New(ttl time.Duration) *Cache {
	return &Cache{
		ttl:     ttl,
		entries: make(map[string]time.Time),
		now:     time.Now,
	}
}

// CheckAndAdd reports whether key was already seen and records it otherwise.
// It is safe for concurrent use.
func (c *Cache) CheckAndAdd(key string) bool {
	if c.ttl <= 0 {
		return false
	}
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	if seenAt, ok := c.entries[key]; ok && now.Sub(seenAt) < c.ttl {
		return true
	}
	c.entries[key] = now
	return false
}

// Sweep drops expired entries and returns how many were removed.
func (c *Cache) Sweep() int {
	if c.ttl <= 0 {
		return 0
	}
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	for key, seenAt := range c.entries {
		if now.Sub(seenAt) >= c.ttl {
			delete(c.entries, key)
			removed++
		}
	}
	return removed
}

// Len returns the number of retained keys.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
