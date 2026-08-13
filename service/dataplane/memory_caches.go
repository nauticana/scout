package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/lru"
)

// memoryCache lazily builds one sharded LRU with shared defaults.
type memoryCache[K comparable, V any] struct {
	once  sync.Once
	cache *lru.Sharded[K, V]
}

func (m *memoryCache[K, V]) get(capacity int, ttl time.Duration) *lru.Sharded[K, V] {
	m.once.Do(func() {
		if capacity <= 0 {
			capacity = 4096
		}
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		m.cache = lru.NewSharded[K, V](min(8, capacity), capacity, ttl/2, 256, nil)
	})
	return m.cache
}

type sessionKey struct {
	tenantID       int64
	conversationID string
}

// MemorySessionCache is an in-process HotSessionCache; losing it costs latency, never correctness.
type MemorySessionCache struct {
	Capacity int
	TTL      time.Duration
	inner    memoryCache[sessionKey, domain.SessionSnapshot]
}

var _ contract.HotSessionCache = (*MemorySessionCache)(nil)

func (cache *MemorySessionCache) ttl() time.Duration {
	if cache.TTL > 0 {
		return cache.TTL
	}
	return 5 * time.Minute
}

// Get returns a cached session and whether it was found.
func (cache *MemorySessionCache) Get(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.SessionSnapshot{}, false, err
	}
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" {
		return domain.SessionSnapshot{}, false, fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	snapshot, ok := cache.inner.get(cache.Capacity, cache.ttl()).Get(sessionKey{tenantID, conversationID})
	return snapshot, ok, nil
}

// Put caches a durable session snapshot.
func (cache *MemorySessionCache) Put(ctx context.Context, tenantID int64, snapshot domain.SessionSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(snapshot.ConversationID) == "" {
		return fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	cache.inner.get(cache.Capacity, cache.ttl()).Set(sessionKey{tenantID, snapshot.ConversationID}, snapshot, cache.ttl())
	return nil
}

// Invalidate removes a stale conversation snapshot.
func (cache *MemorySessionCache) Invalidate(ctx context.Context, tenantID int64, conversationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" {
		return fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	cache.inner.get(cache.Capacity, cache.ttl()).Delete(sessionKey{tenantID, conversationID})
	return nil
}

// Close stops the background sweeper.
func (cache *MemorySessionCache) Close() error {
	return cache.inner.get(cache.Capacity, cache.ttl()).Close()
}

type graphKey struct {
	tenantID int64
	agentID  string
	version  string
}

// MemoryGraphCache is an in-process ExecutionGraphCache for immutable compiled graphs.
type MemoryGraphCache struct {
	Capacity int
	TTL      time.Duration
	inner    memoryCache[graphKey, domain.ExecutionGraph]
}

var _ contract.ExecutionGraphCache = (*MemoryGraphCache)(nil)

func (cache *MemoryGraphCache) ttl() time.Duration {
	if cache.TTL > 0 {
		return cache.TTL
	}
	return 15 * time.Minute
}

// Get returns a cached graph and whether it was found.
func (cache *MemoryGraphCache) Get(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.ExecutionGraph{}, false, err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(version) == "" {
		return domain.ExecutionGraph{}, false, fmt.Errorf("%w: tenant, agent, and version are required", domain.ErrValidation)
	}
	graph, ok := cache.inner.get(cache.Capacity, cache.ttl()).Get(graphKey{tenantID, agentID, version})
	return graph, ok, nil
}

// Put caches a graph under its immutable version.
func (cache *MemoryGraphCache) Put(ctx context.Context, tenantID int64, graph domain.ExecutionGraph) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(graph.AgentID) == "" || strings.TrimSpace(graph.Version) == "" {
		return fmt.Errorf("%w: tenant, agent, and version are required", domain.ErrValidation)
	}
	cache.inner.get(cache.Capacity, cache.ttl()).Set(graphKey{tenantID, graph.AgentID, graph.Version}, graph, cache.ttl())
	return nil
}

// Invalidate removes cached graphs matching an immutable version.
func (cache *MemoryGraphCache) Invalidate(ctx context.Context, tenantID int64, agentID, version string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(version) == "" {
		return fmt.Errorf("%w: tenant, agent, and version are required", domain.ErrValidation)
	}
	cache.inner.get(cache.Capacity, cache.ttl()).Delete(graphKey{tenantID, agentID, version})
	return nil
}

// Close stops the background sweeper.
func (cache *MemoryGraphCache) Close() error {
	return cache.inner.get(cache.Capacity, cache.ttl()).Close()
}
