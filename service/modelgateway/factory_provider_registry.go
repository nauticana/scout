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
// life of the process. Build failures are never cached.
type FactoryProviderRegistry struct {
	Factory contract.AgentProviderFactory
	TTL     time.Duration
	Now     func() time.Time

	mu      sync.Mutex
	entries map[domain.ModelReference]factoryProviderEntry
}

type factoryProviderEntry struct {
	provider contract.ModelProvider
	builtAt  time.Time
}

var _ contract.ModelProviderRegistry = (*FactoryProviderRegistry)(nil)

func (registry *FactoryProviderRegistry) ProviderFor(ctx context.Context, selection domain.ModelSelection) (contract.ModelProvider, error) {
	if registry.Factory == nil {
		return nil, fmt.Errorf("factory provider registry: factory is required")
	}
	reference := domain.ModelReference{ProviderID: strings.TrimSpace(selection.Provider), ModelID: strings.TrimSpace(selection.Model)}
	if reference.ProviderID == "" || reference.ModelID == "" {
		return nil, fmt.Errorf("%w: model provider and model are required", domain.ErrValidation)
	}
	now := time.Now
	if registry.Now != nil {
		now = registry.Now
	}
	registry.mu.Lock()
	entry, cached := registry.entries[reference]
	registry.mu.Unlock()
	if cached && (registry.TTL <= 0 || now().Sub(entry.builtAt) < registry.TTL) {
		return entry.provider, nil
	}
	provider, _, err := registry.Factory.Build(ctx, reference)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("%w: model provider %q", domain.ErrNotFound, reference.ProviderID)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[domain.ModelReference]factoryProviderEntry)
	}
	registry.entries[reference] = factoryProviderEntry{provider: provider, builtAt: now()}
	return provider, nil
}
