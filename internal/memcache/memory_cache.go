package memcache

import (
	"sync"
	"time"

	keelcache "github.com/nauticana/keel/cache"
)

// memoryCache lazily builds one sharded LRU.
type memoryCache[K comparable, V any] struct {
	once  sync.Once
	cache *keelcache.ShardedLRU[K, V]
}

func (m *memoryCache[K, V]) get(capacity int, ttl time.Duration) *keelcache.ShardedLRU[K, V] {
	m.once.Do(func() {
		m.cache = keelcache.NewShardedLRU[K, V](min(8, capacity), capacity, ttl/2, 256, nil)
	})
	return m.cache
}
