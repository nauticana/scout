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
	CredentialRefs     map[string]string
	GoogleProjectID    string
	GoogleLocation     string
	UseGoogleGeminiAPI bool
	// Temperature is sent only to models Sampling reports as accepting it; nil sends none.
	Temperature *float64
	// Sampling says which models accept sampling parameters. Without it none do,
	// because a model that rejects them fails every call.
	Sampling contract.SamplingSupport
}

// Factory builds Scout's concrete adapters from injected deployment settings
// and credentials.
type Factory struct {
	credentials secret.SecretProvider
	config      FactoryConfig
}

var (
	_ contract.AgentProviderFactory = (*Factory)(nil)
	_ contract.ModelLister          = (*Factory)(nil)
)

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
	temperature, err := factory.temperatureFor(ctx, reference)
	if err != nil {
		return nil, nil, err
	}

	return factory.build(ctx, reference.ProviderID, temperature)
}

// compatibleVendors are the OpenAI-compatible vendors the factory builds.
// Perplexity grounds every Sonar call; xAI serves web search only through its
// Responses API, so its Chat Completions route refuses a grounded call.
var compatibleVendors = map[string]OpenAI{
	PerplexityProviderID: {ProviderID: PerplexityProviderID, BaseURL: "https://api.perplexity.ai"},
	XAIProviderID:        {ProviderID: XAIProviderID, BaseURL: "https://api.x.ai/v1"},
}

// build constructs the adapters of one provider; a compatible vendor's media
// endpoints are not OpenAI's, so it has no media adapter.
func (factory *Factory) build(ctx context.Context, providerID string, temperature *float64) (contract.ModelProvider, contract.MediaProvider, error) {
	switch providerID {
	case GoogleProviderID:
		adapter, err := factory.googleAdapter(ctx, temperature)
		if err != nil {
			return nil, nil, err
		}
		return adapter, adapter, nil
	case AnthropicProviderID:
		apiKey, err := factory.apiKey(ctx, providerID)
		if err != nil {
			return nil, nil, err
		}
		return &Anthropic{APIKey: apiKey, Temperature: temperature}, nil, nil
	}
	vendor, compatible := compatibleVendors[providerID]
	if !compatible && providerID != OpenAIProviderID {
		return nil, nil, fmt.Errorf("%w: model provider %q", domain.ErrNotFound, providerID)
	}
	apiKey, err := factory.apiKey(ctx, providerID)
	if err != nil {
		return nil, nil, err
	}
	vendor.APIKey, vendor.Temperature = apiKey, temperature
	if compatible {
		return &vendor, nil, nil
	}
	return &vendor, &vendor, nil
}

// ListModels returns the model ids the provider serves now. Perplexity
// publishes no model list for its Sonar models.
func (factory *Factory) ListModels(ctx context.Context, providerID string) ([]domain.ModelReference, error) {
	if factory == nil {
		return nil, fmt.Errorf("provider factory is required")
	}
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil, fmt.Errorf("%w: provider is required", domain.ErrValidation)
	}
	if providerID == PerplexityProviderID {
		return nil, fmt.Errorf("%w: %s publishes no model list", domain.ErrCapabilityUnsupported, providerID)
	}
	model, _, err := factory.build(ctx, providerID, nil)
	if err != nil {
		return nil, err
	}
	lister, lists := model.(interface {
		listModels(context.Context) ([]string, error)
	})
	if !lists {
		return nil, fmt.Errorf("%w: %s publishes no model list", domain.ErrCapabilityUnsupported, providerID)
	}
	ids, err := lister.listModels(ctx)
	if err != nil {
		return nil, err
	}
	references := make([]domain.ModelReference, len(ids))
	for i, id := range ids {
		references[i] = domain.ModelReference{ProviderID: providerID, ModelID: id}
	}
	return references, nil
}

// temperatureFor returns the configured temperature only for a model that accepts one.
func (factory *Factory) temperatureFor(ctx context.Context, reference domain.ModelReference) (*float64, error) {
	configured := factory.config.Temperature
	if configured == nil {
		return nil, nil
	}
	if *configured < 0 || *configured > 2 {
		return nil, fmt.Errorf("%w: temperature must be between 0 and 2", domain.ErrValidation)
	}
	if factory.config.Sampling == nil {
		return nil, nil
	}
	accepts, err := factory.config.Sampling.AcceptsSampling(ctx, reference)
	if err != nil {
		return nil, fmt.Errorf("sampling support of %s/%s: %w", reference.ProviderID, reference.ModelID, err)
	}
	if !accepts {
		return nil, nil
	}
	value := *configured
	return &value, nil
}

func (factory *Factory) googleAdapter(ctx context.Context, temperature *float64) (*Google, error) {
	apiKey := ""
	if factory.config.UseGoogleGeminiAPI {
		var err error
		apiKey, err = factory.apiKey(ctx, GoogleProviderID)
		if err != nil {
			return nil, err
		}
	} else if strings.TrimSpace(factory.config.GoogleProjectID) == "" || strings.TrimSpace(factory.config.GoogleLocation) == "" {
		return nil, fmt.Errorf("%w: google Vertex project and location are required", domain.ErrValidation)
	}
	return &Google{
		ProjectID:    strings.TrimSpace(factory.config.GoogleProjectID),
		Location:     strings.TrimSpace(factory.config.GoogleLocation),
		UseGeminiAPI: factory.config.UseGoogleGeminiAPI,
		APIKey:       apiKey,
		Temperature:  temperature,
	}, nil
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
