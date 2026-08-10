package modelgateway

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ProviderRegistry resolves immutable provider registrations safely across requests.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]contract.ModelProvider
}

// NewProviderRegistry creates an empty model provider registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{providers: make(map[string]contract.ModelProvider)}
}

// Register binds one unique provider id to an adapter.
func (registry *ProviderRegistry) Register(providerID string, provider contract.ModelProvider) error {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" || provider == nil {
		return fmt.Errorf("%w: provider id and adapter are required", domain.ErrValidation)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.providers == nil {
		registry.providers = make(map[string]contract.ModelProvider)
	}
	if _, exists := registry.providers[providerID]; exists {
		return fmt.Errorf("%w: model provider %q is already registered", domain.ErrConflict, providerID)
	}
	registry.providers[providerID] = provider
	return nil
}

// ProviderFor returns the adapter registered for a model selection.
func (registry *ProviderRegistry) ProviderFor(ctx context.Context, selection domain.ModelSelection) (contract.ModelProvider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	providerID := strings.TrimSpace(selection.Provider)
	if providerID == "" {
		return nil, fmt.Errorf("%w: model provider is required", domain.ErrValidation)
	}
	registry.mu.RLock()
	provider := registry.providers[providerID]
	registry.mu.RUnlock()
	if provider == nil {
		return nil, fmt.Errorf("%w: model provider %q", domain.ErrNotFound, providerID)
	}
	return provider, nil
}

var _ contract.ModelProviderRegistry = (*ProviderRegistry)(nil)
