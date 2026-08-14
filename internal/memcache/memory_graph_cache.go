package memcache

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

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

// Get returns a cached graph and whether it was found.
func (cache *MemoryGraphCache) Get(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.ExecutionGraph{}, false, err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(version) == "" {
		return domain.ExecutionGraph{}, false, fmt.Errorf("%w: tenant, agent, and version are required", domain.ErrValidation)
	}
	graph, ok := cache.inner.get(cache.Capacity, cache.TTL).Get(graphKey{tenantID, agentID, version})
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
	cache.inner.get(cache.Capacity, cache.TTL).Set(graphKey{tenantID, graph.AgentID, graph.Version}, graph, cache.TTL)
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
	cache.inner.get(cache.Capacity, cache.TTL).Delete(graphKey{tenantID, agentID, version})
	return nil
}

// Close stops the background sweeper.
func (cache *MemoryGraphCache) Close() error {
	return cache.inner.get(cache.Capacity, cache.TTL).Close()
}
