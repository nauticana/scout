package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/secret"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// FactoryConfig contains host-selected provider settings. Credentials remain
// in the injected keystore and are fetched when an adapter is built.
type FactoryConfig struct {
	CredentialRefs  map[string]string
	GoogleProjectID string
	GoogleLocation  string
	// GoogleVideoLocation is the Vertex location of Veo; empty uses GoogleLocation.
	GoogleVideoLocation string
	UseGoogleGeminiAPI  bool
	// Temperature is sent only to models Sampling reports as accepting it; nil sends none.
	Temperature *float64
	// Sampling says which models accept sampling parameters. Without it none do,
	// because a model that rejects them fails every call.
	Sampling contract.SamplingSupport
	// Endpoints resolves a provider id Scout does not adapt natively to a registered
	// OpenAI-compatible endpoint, such as a self-hosted inference server; nil registers none.
	Endpoints contract.ModelEndpointSource
	// EndpointAllowlist holds the host names (public addresses only) and IPs or CIDRs a
	// registered endpoint may reach; empty refuses every endpoint.
	EndpointAllowlist []string
	// EndpointMaxResponseBytes bounds one endpoint response; zero is DefaultEndpointMaxResponseBytes.
	EndpointMaxResponseBytes int64
}

// DefaultEndpointMaxResponseBytes bounds a registered endpoint's response when the config sets none.
const DefaultEndpointMaxResponseBytes int64 = 16 << 20

// Factory builds Scout's concrete adapters from injected deployment settings
// and credentials.
type Factory struct {
	credentials secret.SecretProvider
	config      FactoryConfig

	// slots bound each registered endpoint's calls across adapter rebuilds.
	mu    sync.Mutex
	slots map[string]chan struct{}
}

var (
	_ contract.AgentProviderFactory = (*Factory)(nil)
	_ contract.ModelLister          = (*Factory)(nil)
)

// NewFactory copies configuration so later map mutation cannot change live
// provider composition.
func NewFactory(credentials secret.SecretProvider, config FactoryConfig) *Factory {
	config.CredentialRefs = cloneCredentialRefs(config.CredentialRefs)
	config.EndpointAllowlist = append([]string(nil), config.EndpointAllowlist...)
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

// build constructs the adapters of one provider; a compatible vendor's or a registered
// endpoint's media endpoints are not OpenAI's, so it has no media adapter.
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
		adapter, err := factory.endpointAdapter(ctx, providerID, temperature)
		if err != nil {
			return nil, nil, err
		}
		return adapter, nil, nil
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
		ProjectID:     strings.TrimSpace(factory.config.GoogleProjectID),
		Location:      strings.TrimSpace(factory.config.GoogleLocation),
		VideoLocation: strings.TrimSpace(factory.config.GoogleVideoLocation),
		UseGeminiAPI:  factory.config.UseGoogleGeminiAPI,
		APIKey:        apiKey,
		Temperature:   temperature,
	}, nil
}

// endpointAdapter builds the OpenAI-compatible adapter of a registered endpoint behind its
// egress allowlist, response bound, and concurrency bound; its key is optional.
func (factory *Factory) endpointAdapter(ctx context.Context, providerID string, temperature *float64) (*OpenAI, error) {
	if factory.config.Endpoints == nil {
		return nil, fmt.Errorf("%w: model provider %q", domain.ErrNotFound, providerID)
	}
	endpoint, err := factory.config.Endpoints.ModelEndpoint(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if endpoint.Protocol != domain.ModelProtocolOpenAI || endpoint.MaxConcurrency < 0 {
		return nil, fmt.Errorf("%w: model provider %q is not a valid OpenAI-compatible endpoint", domain.ErrValidation, providerID)
	}
	allowlist, err := newEndpointAllowlist(factory.config.EndpointAllowlist)
	if err != nil {
		return nil, err
	}
	maxBytes := factory.config.EndpointMaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = DefaultEndpointMaxResponseBytes
	}
	client, err := allowlist.client(endpoint.BaseURL, &endpointTransport{slots: factory.endpointSlots(providerID, endpoint.MaxConcurrency), maxBytes: maxBytes})
	if err != nil {
		return nil, fmt.Errorf("model provider %q: %w", providerID, err)
	}
	apiKey := ""
	if endpoint.CredentialRef != "" {
		if apiKey, err = factory.secret(ctx, providerID, endpoint.CredentialRef); err != nil {
			return nil, err
		}
	}
	return &OpenAI{ProviderID: providerID, BaseURL: endpoint.BaseURL, APIKey: apiKey, Temperature: temperature, HTTPClient: client}, nil
}

// endpointSlots returns the provider's shared semaphore, replaced when its bound changes.
func (factory *Factory) endpointSlots(providerID string, bound int) chan struct{} {
	if bound == 0 {
		return nil
	}
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if slots := factory.slots[providerID]; cap(slots) == bound {
		return slots
	}
	if factory.slots == nil {
		factory.slots = make(map[string]chan struct{})
	}
	factory.slots[providerID] = make(chan struct{}, bound)
	return factory.slots[providerID]
}

func (factory *Factory) apiKey(ctx context.Context, providerID string) (string, error) {
	reference := factory.config.CredentialRefs[providerID]
	if reference == "" {
		reference = providerID + "_api_key"
	}
	return factory.secret(ctx, providerID, reference)
}

func (factory *Factory) secret(ctx context.Context, providerID, reference string) (string, error) {
	if factory.credentials == nil {
		return "", fmt.Errorf("%w: credential provider is required for %s", domain.ErrNotReady, providerID)
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
