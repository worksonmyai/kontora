package summarizer

import (
	"sync"
	"time"
)

// CacheEntry holds a cached summarization result.
type CacheEntry struct {
	Summary   string
	Error     error
	Source    string
	CreatedAt time.Time
}

// Cache stores summarization results with TTL-based expiry.
type Cache struct {
	mu      sync.Mutex
	entries map[string]CacheEntry
	ttl     time.Duration
}

// NewCache creates a cache with the given TTL.
func NewCache(ttl time.Duration) *Cache {
	return &Cache{
		entries: make(map[string]CacheEntry),
		ttl:     ttl,
	}
}

// Get returns a cached entry if it exists and hasn't expired.
func (c *Cache) Get(key string) (CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return CacheEntry{}, false
	}
	if time.Since(entry.CreatedAt) > c.ttl {
		delete(c.entries, key)
		return CacheEntry{}, false
	}
	return entry, true
}

// Set stores a result in the cache.
func (c *Cache) Set(key, summary, source string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = CacheEntry{
		Summary:   summary,
		Error:     err,
		Source:    source,
		CreatedAt: time.Now(),
	}
}

// Delete removes an entry from the cache.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}
