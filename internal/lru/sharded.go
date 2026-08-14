package lru

import (
	"hash/maphash"
	"sync"
	"time"
)

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
