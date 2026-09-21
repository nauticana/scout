package modelgateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// FactoryProviderRegistry builds adapters on first use from an AgentProviderFactory
// and reuses them per provider and model. A positive TTL rebuilds an adapter after
// that age, which is how a rotated credential is picked up; zero keeps it for the
// life of the process. Build failures are never cached. It also serves embedding
// adapters when the factory builds them, so one registry covers both halves.
type FactoryProviderRegistry struct {
	Factory contract.AgentProviderFactory
	TTL     time.Duration
	Now     func() time.Time

	models    adapterCache[contract.ModelProvider]
	embedders adapterCache[contract.EmbeddingProvider]
}

var (
	_ contract.ModelProviderRegistry     = (*FactoryProviderRegistry)(nil)
	_ contract.EmbeddingProviderRegistry = (*FactoryProviderRegistry)(nil)
)

func (registry *FactoryProviderRegistry) reference(selection domain.ModelSelection) (domain.ModelReference, error) {
	if registry.Factory == nil {
		return domain.ModelReference{}, fmt.Errorf("factory provider registry: factory is required")
	}
	reference := domain.ModelReference{ProviderID: strings.TrimSpace(selection.Provider), ModelID: strings.TrimSpace(selection.Model)}
	if reference.ProviderID == "" || reference.ModelID == "" {
		return domain.ModelReference{}, fmt.Errorf("%w: model provider and model are required", domain.ErrValidation)
	}
	return reference, nil
}

func (registry *FactoryProviderRegistry) clock() func() time.Time {
	if registry.Now != nil {
		return registry.Now
	}
	return time.Now
}

func (registry *FactoryProviderRegistry) ProviderFor(ctx context.Context, selection domain.ModelSelection) (contract.ModelProvider, error) {
	reference, err := registry.reference(selection)
	if err != nil {
		return nil, err
	}
	return registry.models.adapter(reference, registry.TTL, registry.clock(), func() (contract.ModelProvider, error) {
		provider, _, err := registry.Factory.Build(ctx, reference)
		if err != nil {
			return nil, err
		}
		if provider == nil {
			return nil, fmt.Errorf("%w: model provider %q", domain.ErrNotFound, reference.ProviderID)
		}
		return provider, nil
	})
}

// EmbeddingProviderFor serves the factory's embedding half; a factory that
// builds none is ErrCapabilityUnsupported rather than a silent text fallback.
func (registry *FactoryProviderRegistry) EmbeddingProviderFor(ctx context.Context, selection domain.ModelSelection) (contract.EmbeddingProvider, error) {
	reference, err := registry.reference(selection)
	if err != nil {
		return nil, err
	}
	return registry.embedders.adapter(reference, registry.TTL, registry.clock(), func() (contract.EmbeddingProvider, error) {
		factory, builds := registry.Factory.(contract.EmbeddingProviderFactory)
		if !builds {
			return nil, fmt.Errorf("%w: the provider factory builds no embedding adapters", domain.ErrCapabilityUnsupported)
		}
		embedder, err := factory.BuildEmbedder(ctx, reference)
		if err != nil {
			return nil, err
		}
		if embedder == nil {
			return nil, fmt.Errorf("%w: embedding provider %q", domain.ErrNotFound, reference.ProviderID)
		}
		return embedder, nil
	})
}

// adapterCache holds built adapters of one kind per model reference.
type adapterCache[T any] struct {
	mu      sync.Mutex
	entries map[domain.ModelReference]cachedAdapter[T]
}

type cachedAdapter[T any] struct {
	adapter T
	builtAt time.Time
}

// adapter returns the cached adapter while it is fresh, otherwise builds one.
func (cache *adapterCache[T]) adapter(reference domain.ModelReference, ttl time.Duration, now func() time.Time, build func() (T, error)) (T, error) {
	cache.mu.Lock()
	entry, cached := cache.entries[reference]
	cache.mu.Unlock()
	if cached && (ttl <= 0 || now().Sub(entry.builtAt) < ttl) {
		return entry.adapter, nil
	}
	adapter, err := build()
	if err != nil {
		var zero T
		return zero, err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = make(map[domain.ModelReference]cachedAdapter[T])
	}
	cache.entries[reference] = cachedAdapter[T]{adapter: adapter, builtAt: now()}
	return adapter, nil
}
