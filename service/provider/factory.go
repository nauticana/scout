package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/keel/secret"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// FactoryConfig contains host-selected provider settings. Credentials remain
// in the injected keystore and are fetched when an adapter is built.
type FactoryConfig struct {
	CredentialRefs        map[string]string
	GoogleProjectID       string
	GoogleLocation        string
	UseGoogleGeminiAPI    bool
	Temperature           float64
	TemperatureConfigured bool
}

// Factory builds Scout's concrete adapters from injected deployment settings
// and credentials.
type Factory struct {
	credentials secret.SecretProvider
	config      FactoryConfig
}

var _ contract.AgentProviderFactory = (*Factory)(nil)

// NewFactory copies configuration so later map mutation cannot change live
// provider composition.
func NewFactory(credentials secret.SecretProvider, config FactoryConfig) *Factory {
	config.CredentialRefs = cloneCredentialRefs(config.CredentialRefs)
	return &Factory{credentials: credentials, config: config}
}

// Build constructs the configured adapters for one model reference.
func (factory *Factory) Build(ctx context.Context, reference domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error) {
	if factory == nil {
		return nil, nil, fmt.Errorf("provider factory is required")
	}
	reference.ProviderID = strings.TrimSpace(reference.ProviderID)
	reference.ModelID = strings.TrimSpace(reference.ModelID)
	if reference.ProviderID == "" || reference.ModelID == "" {
		return nil, nil, fmt.Errorf("%w: provider and model are required", domain.ErrValidation)
	}
	if factory.config.TemperatureConfigured && (factory.config.Temperature < 0 || factory.config.Temperature > 2) {
		return nil, nil, fmt.Errorf("%w: temperature must be between 0 and 2", domain.ErrValidation)
	}

	switch reference.ProviderID {
	case GoogleProviderID:
		return factory.google(ctx)
	case OpenAIProviderID:
		apiKey, err := factory.apiKey(ctx, OpenAIProviderID)
		if err != nil {
			return nil, nil, err
		}
		adapter := &OpenAI{
			APIKey: apiKey, Temperature: factory.config.Temperature,
			TemperatureConfigured: factory.config.TemperatureConfigured,
		}
		return adapter, adapter, nil
	case AnthropicProviderID:
		apiKey, err := factory.apiKey(ctx, AnthropicProviderID)
		if err != nil {
			return nil, nil, err
		}
		return &Anthropic{
			APIKey: apiKey, Temperature: factory.config.Temperature,
			TemperatureConfigured: factory.config.TemperatureConfigured,
		}, nil, nil
	default:
		return nil, nil, fmt.Errorf("%w: model provider %q", domain.ErrNotFound, reference.ProviderID)
	}
}

func (factory *Factory) google(ctx context.Context) (contract.ModelProvider, contract.MediaProvider, error) {
	apiKey := ""
	if factory.config.UseGoogleGeminiAPI {
		var err error
		apiKey, err = factory.apiKey(ctx, GoogleProviderID)
		if err != nil {
			return nil, nil, err
		}
	} else if strings.TrimSpace(factory.config.GoogleProjectID) == "" || strings.TrimSpace(factory.config.GoogleLocation) == "" {
		return nil, nil, fmt.Errorf("%w: google Vertex project and location are required", domain.ErrValidation)
	}
	adapter := &Google{
		ProjectID:             strings.TrimSpace(factory.config.GoogleProjectID),
		Location:              strings.TrimSpace(factory.config.GoogleLocation),
		UseGeminiAPI:          factory.config.UseGoogleGeminiAPI,
		APIKey:                apiKey,
		Temperature:           factory.config.Temperature,
		TemperatureConfigured: factory.config.TemperatureConfigured,
	}
	return adapter, adapter, nil
}

func (factory *Factory) apiKey(ctx context.Context, providerID string) (string, error) {
	if factory.credentials == nil {
		return "", fmt.Errorf("%w: credential provider is required for %s", domain.ErrNotReady, providerID)
	}
	reference := factory.config.CredentialRefs[providerID]
	if reference == "" {
		reference = providerID + "_api_key"
	}
	apiKey, err := factory.credentials.GetSecret(ctx, reference)
	if err != nil {
		return "", fmt.Errorf("provider %q credential %q: %w", providerID, reference, err)
	}
	if strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("%w: provider %q credential %q is empty", domain.ErrNotReady, providerID, reference)
	}
	return apiKey, nil
}

func cloneCredentialRefs(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for providerID, reference := range source {
		cloned[strings.TrimSpace(providerID)] = strings.TrimSpace(reference)
	}
	return cloned
}
