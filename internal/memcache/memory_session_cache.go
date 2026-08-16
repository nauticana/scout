package memcache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MemorySessionCache is an in-process HotSessionCache; losing it costs latency,
// never correctness. Entries carry their durable Revision and a Put never
// replaces a newer cached revision with an older one.
type MemorySessionCache struct {
	Capacity int
	TTL      time.Duration
	inner    memoryCache[domain.SessionKey, domain.SessionSnapshot]
	puts     sync.Mutex
}

var _ contract.HotSessionCache = (*MemorySessionCache)(nil)

// Get returns a cached session and whether it was found.
func (cache *MemorySessionCache) Get(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.SessionSnapshot{}, false, err
	}
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" {
		return domain.SessionSnapshot{}, false, fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	snapshot, ok := cache.inner.get(cache.Capacity, cache.TTL).Get(domain.SessionKey{TenantID: tenantID, ConversationID: conversationID})
	return snapshot, ok, nil
}

// Put caches a durable session snapshot unless a newer revision is already cached.
func (cache *MemorySessionCache) Put(ctx context.Context, tenantID int64, snapshot domain.SessionSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(snapshot.ConversationID) == "" || snapshot.Revision < 0 {
		return fmt.Errorf("%w: tenant, conversation, and non-negative revision are required", domain.ErrValidation)
	}
	key := domain.SessionKey{TenantID: tenantID, ConversationID: snapshot.ConversationID}
	inner := cache.inner.get(cache.Capacity, cache.TTL)
	cache.puts.Lock()
	defer cache.puts.Unlock()
	if current, ok := inner.Get(key); ok && current.Revision > snapshot.Revision {
		return nil
	}
	inner.Set(key, snapshot, cache.TTL)
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
	cache.inner.get(cache.Capacity, cache.TTL).Delete(domain.SessionKey{TenantID: tenantID, ConversationID: conversationID})
	return nil
}

// Close stops the background sweeper.
func (cache *MemorySessionCache) Close() error {
	return cache.inner.get(cache.Capacity, cache.TTL).Close()
}
