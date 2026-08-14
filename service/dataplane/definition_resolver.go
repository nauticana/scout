package dataplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/singleflight"
)

type definitionKey struct {
	tenantID int64
	agentID  string
	version  string
}

// DefinitionResolver reads immutable graphs through a non-authoritative cache.
type DefinitionResolver struct {
	Repository contract.ExecutionGraphRepository
	Cache      contract.ExecutionGraphCache
	Metrics    contract.RuntimeMetrics

	flights singleflight.Group[definitionKey, domain.ExecutionGraph]
}

// NewDefinitionResolver builds an execution-graph resolver.
func NewDefinitionResolver(repository contract.ExecutionGraphRepository, cache contract.ExecutionGraphCache, metrics contract.RuntimeMetrics) (*DefinitionResolver, error) {
	if repository == nil || cache == nil || metrics == nil {
		return nil, fmt.Errorf("definition resolver: repository, cache, and metrics are required")
	}
	return &DefinitionResolver{Repository: repository, Cache: cache, Metrics: metrics}, nil
}

// Resolve returns a matching cached graph or refreshes it from durable storage.
func (resolver *DefinitionResolver) Resolve(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, error) {
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(version) == "" {
		return domain.ExecutionGraph{}, fmt.Errorf("%w: tenant, agent, and version are required", domain.ErrValidation)
	}
	if resolver.Repository == nil || resolver.Cache == nil || resolver.Metrics == nil {
		return domain.ExecutionGraph{}, fmt.Errorf("definition resolver: repository, cache, and metrics are required")
	}
	graph, found, err := resolver.Cache.Get(ctx, tenantID, agentID, version)
	if err != nil {
		resolver.recordCacheError(ctx, tenantID, "get", err)
	} else if found && graph.AgentID == agentID && graph.Version == version {
		return graph, nil
	} else if found {
		cacheErr := fmt.Errorf("%w: cached execution graph identity mismatch", domain.ErrConflict)
		resolver.recordCacheError(ctx, tenantID, "get", cacheErr)
		if err := resolver.Cache.Invalidate(ctx, tenantID, agentID, version); err != nil {
			resolver.recordCacheError(ctx, tenantID, "invalidate", err)
		}
	}
	return resolver.flights.Do(ctx, definitionKey{tenantID, agentID, version}, func(loadCtx context.Context) (domain.ExecutionGraph, error) {
		graph, err := resolver.Repository.Get(loadCtx, tenantID, agentID, version)
		if err != nil {
			return domain.ExecutionGraph{}, fmt.Errorf("load execution graph %q version %q: %w", agentID, version, err)
		}
		if graph.AgentID != agentID || graph.Version != version {
			return domain.ExecutionGraph{}, fmt.Errorf("%w: durable execution graph identity mismatch", domain.ErrConflict)
		}
		if err := resolver.Cache.Put(loadCtx, tenantID, graph); err != nil {
			resolver.recordCacheError(loadCtx, tenantID, "put", err)
		}
		return graph, nil
	})
}

func (resolver *DefinitionResolver) recordCacheError(ctx context.Context, tenantID int64, operation string, err error) {
	resolver.Metrics.RecordDependency(ctx, tenantID, "execution_graph_cache", operation, domain.Usage{}, err)
}

var _ contract.DefinitionResolver = (*DefinitionResolver)(nil)
