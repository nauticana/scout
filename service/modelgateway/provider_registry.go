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
	mu             sync.RWMutex
	providers      map[string]contract.ModelProvider
	mediaProviders map[string]contract.MediaProvider
}

// NewProviderRegistry creates an empty model provider registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers:      make(map[string]contract.ModelProvider),
		mediaProviders: make(map[string]contract.MediaProvider),
	}
}

// Register binds one unique provider id to an adapter.
func (registry *ProviderRegistry) Register(providerID string, provider contract.ModelProvider) error {
	return registry.RegisterAdapters(providerID, provider, nil)
}

// RegisterAdapters binds one unique provider id to its text and optional media
// adapters.
func (registry *ProviderRegistry) RegisterAdapters(providerID string, provider contract.ModelProvider, media contract.MediaProvider) error {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" || provider == nil {
		return fmt.Errorf("%w: provider id and adapter are required", domain.ErrValidation)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.providers == nil {
		registry.providers = make(map[string]contract.ModelProvider)
	}
	if registry.mediaProviders == nil {
		registry.mediaProviders = make(map[string]contract.MediaProvider)
	}
	if _, exists := registry.providers[providerID]; exists {
		return fmt.Errorf("%w: model provider %q is already registered", domain.ErrConflict, providerID)
	}
	registry.providers[providerID] = provider
	if media != nil {
		registry.mediaProviders[providerID] = media
	}
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

// Build returns the registered adapters for a published model reference.
func (registry *ProviderRegistry) Build(ctx context.Context, reference domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error) {
	if strings.TrimSpace(reference.ModelID) == "" {
		return nil, nil, fmt.Errorf("%w: model is required", domain.ErrValidation)
	}
	provider, err := registry.ProviderFor(ctx, domain.ModelSelection{Provider: reference.ProviderID, Model: reference.ModelID})
	if err != nil {
		return nil, nil, err
	}
	registry.mu.RLock()
	media := registry.mediaProviders[strings.TrimSpace(reference.ProviderID)]
	registry.mu.RUnlock()
	return provider, media, nil
}

var _ contract.ModelProviderRegistry = (*ProviderRegistry)(nil)
var _ contract.AgentProviderFactory = (*ProviderRegistry)(nil)
