package memcache

import (
	"sync"
	"time"

	"github.com/nauticana/scout/internal/lru"
)

// memoryCache lazily builds one sharded LRU.
type memoryCache[K comparable, V any] struct {
	once  sync.Once
	cache *lru.Sharded[K, V]
}

func (m *memoryCache[K, V]) get(capacity int, ttl time.Duration) *lru.Sharded[K, V] {
	m.once.Do(func() {
		m.cache = lru.NewSharded[K, V](min(8, capacity), capacity, ttl/2, 256, nil)
	})
	return m.cache
}
