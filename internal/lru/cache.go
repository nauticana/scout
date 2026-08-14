// Package lru provides a bounded TTL cache and a sharded variant with a bounded background sweeper.
package lru

import (
	"container/list"
	"sync"
	"time"
)

type entry[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time
}

func (e *entry[K, V]) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

// Cache is a fixed-capacity LRU with per-entry TTL under one lock.
type Cache[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	entries  map[K]*list.Element
	order    *list.List
	now      func() time.Time
}

// New returns a cache of at most capacity entries; nil now uses time.Now.
func New[K comparable, V any](capacity int, now func() time.Time) *Cache[K, V] {
	if capacity <= 0 {
		panic("lru: capacity must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		capacity: capacity,
		entries:  make(map[K]*list.Element, capacity),
		order:    list.New(),
		now:      now,
	}
}

// Get returns a live value and refreshes its recency; expired entries are removed.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	element := c.entries[key]
	if element == nil {
		var zero V
		return zero, false
	}
	item := element.Value.(*entry[K, V])
	if item.expired(now) {
		c.remove(element)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(element)
	return item.value, true
}

// Set inserts or updates a value; a non-positive ttl means no expiration.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	now := c.now()
	expiresAt := time.Time{}
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		item := element.Value.(*entry[K, V])
		item.value = value
		item.expiresAt = expiresAt
		c.order.MoveToFront(element)
		return
	}
	if len(c.entries) >= c.capacity {
		c.remove(c.order.Back())
	}
	c.entries[key] = c.order.PushFront(&entry[K, V]{key: key, value: value, expiresAt: expiresAt})
}

// Delete removes a key without disturbing other entries.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	if element := c.entries[key]; element != nil {
		c.remove(element)
	}
	c.mu.Unlock()
}

// Len returns the resident entry count, including not-yet-swept expired entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Sweep scans up to limit entries from the LRU tail and drops expired ones.
func (c *Cache[K, V]) Sweep(limit int) int {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	scanned := 0
	for element := c.order.Back(); element != nil && scanned < limit; scanned++ {
		previous := element.Prev()
		if element.Value.(*entry[K, V]).expired(now) {
			c.remove(element)
		}
		element = previous
	}
	return scanned
}

func (c *Cache[K, V]) remove(element *list.Element) {
	if element == nil {
		return
	}
	delete(c.entries, element.Value.(*entry[K, V]).key)
	c.order.Remove(element)
}
