package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestFactoryBuildsConfiguredProviderAdapters(t *testing.T) {
	secrets := &secretProviderStub{values: map[string]string{"custom_openai": "key"}}
	config := FactoryConfig{
		CredentialRefs: map[string]string{OpenAIProviderID: "custom_openai"},
		Temperature:    0, TemperatureConfigured: true,
	}
	factory := NewFactory(secrets, config)
	config.CredentialRefs[OpenAIProviderID] = "mutated"

	model, media, err := factory.Build(context.Background(), domain.ModelReference{ProviderID: OpenAIProviderID, ModelID: "gpt"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	adapter, ok := model.(*OpenAI)
	if !ok || media != adapter || adapter.APIKey != "key" || !adapter.TemperatureConfigured || adapter.Temperature != 0 {
		t.Fatalf("adapters = (%+v, %T)", model, media)
	}
	if len(secrets.references) != 1 || secrets.references[0] != "custom_openai" {
		t.Fatalf("credential references = %v", secrets.references)
	}
}

func TestFactoryBuildsVertexGoogleWithoutAPIKey(t *testing.T) {
	factory := NewFactory(nil, FactoryConfig{GoogleProjectID: "project", GoogleLocation: "us-central1"})
	model, media, err := factory.Build(context.Background(), domain.ModelReference{ProviderID: GoogleProviderID, ModelID: "gemini"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	adapter, ok := model.(*Google)
	if !ok || media != adapter || adapter.UseGeminiAPI || adapter.ProjectID != "project" {
		t.Fatalf("adapters = (%+v, %T)", model, media)
	}
}

func TestFactoryRejectsMissingCredentialsAndUnknownProviders(t *testing.T) {
	factory := NewFactory(&secretProviderStub{}, FactoryConfig{})
	if _, _, err := factory.Build(context.Background(), domain.ModelReference{ProviderID: AnthropicProviderID, ModelID: "claude"}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("missing credential error = %v", err)
	}
	if _, _, err := factory.Build(context.Background(), domain.ModelReference{ProviderID: "other", ModelID: "model"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown provider error = %v", err)
	}
	invalidTemperature := NewFactory(nil, FactoryConfig{Temperature: -1, TemperatureConfigured: true})
	if _, _, err := invalidTemperature.Build(context.Background(), domain.ModelReference{ProviderID: GoogleProviderID, ModelID: "model"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("temperature error = %v", err)
	}
}

type secretProviderStub struct {
	values     map[string]string
	err        error
	references []string
}

func (provider *secretProviderStub) GetSecret(_ context.Context, reference string) (string, error) {
	provider.references = append(provider.references, reference)
	return provider.values[reference], provider.err
}
