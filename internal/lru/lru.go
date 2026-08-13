// Package lru provides a bounded TTL cache and a sharded variant with a bounded background sweeper.
package lru

import (
	"container/list"
	"hash/maphash"
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

// Sharded partitions keys across independently locked caches and sweeps them in bounded batches.
type Sharded[K comparable, V any] struct {
	shards    []*Cache[K, V]
	seed      maphash.Seed
	next      int
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewSharded splits totalCapacity across shards; sweepInterval <= 0 disables the sweeper.
func NewSharded[K comparable, V any](shards, totalCapacity int, sweepInterval time.Duration, sweepBatch int, now func() time.Time) *Sharded[K, V] {
	if shards <= 0 || totalCapacity < shards {
		panic("lru: need at least one slot per shard")
	}
	sharded := &Sharded[K, V]{
		shards: make([]*Cache[K, V], shards),
		seed:   maphash.MakeSeed(),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	base, extra := totalCapacity/shards, totalCapacity%shards
	for i := range sharded.shards {
		capacity := base
		if i < extra {
			capacity++
		}
		sharded.shards[i] = New[K, V](capacity, now)
	}
	if sweepInterval > 0 && sweepBatch > 0 {
		go sharded.sweeper(sweepInterval, sweepBatch)
	} else {
		close(sharded.done)
	}
	return sharded
}

func (s *Sharded[K, V]) shard(key K) *Cache[K, V] {
	return s.shards[maphash.Comparable(s.seed, key)%uint64(len(s.shards))]
}

func (s *Sharded[K, V]) Get(key K) (V, bool)                   { return s.shard(key).Get(key) }
func (s *Sharded[K, V]) Set(key K, value V, ttl time.Duration) { s.shard(key).Set(key, value, ttl) }
func (s *Sharded[K, V]) Delete(key K)                          { s.shard(key).Delete(key) }

// Len sums shard lengths without holding two locks at once.
func (s *Sharded[K, V]) Len() int {
	total := 0
	for _, shard := range s.shards {
		total += shard.Len()
	}
	return total
}

// Close stops the sweeper and waits for it to exit; it is idempotent.
func (s *Sharded[K, V]) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
	return nil
}

func (s *Sharded[K, V]) sweeper(interval time.Duration, batch int) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Each tick spends at most one batch, rotating the starting shard for fairness.
			remaining := batch
			for range s.shards {
				if remaining <= 0 {
					break
				}
				remaining -= s.shards[s.next].Sweep(remaining)
				s.next = (s.next + 1) % len(s.shards)
			}
		case <-s.stop:
			return
		}
	}
}
