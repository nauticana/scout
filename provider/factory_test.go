package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestFactoryBuildsConfiguredProviderAdapters(t *testing.T) {
	secrets := &secretProviderStub{values: map[string]string{"custom_openai": "key"}}
	config := FactoryConfig{
		CredentialRefs: map[string]string{OpenAIProviderID: "custom_openai"},
		Temperature:    new(float64), Sampling: samplingModels{"gpt": true},
	}
	factory := NewFactory(secrets, config)
	config.CredentialRefs[OpenAIProviderID] = "mutated"

	model, media, err := factory.Build(context.Background(), domain.ModelReference{ProviderID: OpenAIProviderID, ModelID: "gpt"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	adapter, ok := model.(*OpenAI)
	if !ok || media != adapter || adapter.APIKey != "key" || adapter.Temperature == nil || *adapter.Temperature != 0 {
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
	negative := -1.0
	invalidTemperature := NewFactory(nil, FactoryConfig{Temperature: &negative})
	if _, _, err := invalidTemperature.Build(context.Background(), domain.ModelReference{ProviderID: GoogleProviderID, ModelID: "model"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("temperature error = %v", err)
	}
}

type samplingModels map[string]bool

func (models samplingModels) AcceptsSampling(_ context.Context, reference domain.ModelReference) (bool, error) {
	return models[reference.ModelID], nil
}

// A model that rejects sampling parameters fails every call that carries one,
// so a configured temperature reaches only models the catalog vouches for.
func TestFactoryWithholdsTemperatureFromModelsThatRejectIt(t *testing.T) {
	temperature := 0.4
	secrets := &secretProviderStub{values: map[string]string{"anthropic_api_key": "key"}}
	build := func(config FactoryConfig, model string) *Anthropic {
		adapter, _, err := NewFactory(secrets, config).Build(context.Background(), domain.ModelReference{ProviderID: AnthropicProviderID, ModelID: model})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return adapter.(*Anthropic)
	}
	sampling := samplingModels{"tunable": true}
	if got := build(FactoryConfig{Temperature: &temperature, Sampling: sampling}, "tunable").Temperature; got == nil || *got != 0.4 {
		t.Fatalf("a sampling model must get the configured temperature, got %v", got)
	}
	for name, adapter := range map[string]*Anthropic{
		"model without the capability": build(FactoryConfig{Temperature: &temperature, Sampling: sampling}, "fixed"),
		"no sampling lookup":           build(FactoryConfig{Temperature: &temperature}, "tunable"),
		"nothing configured":           build(FactoryConfig{Sampling: sampling}, "tunable"),
	} {
		if adapter.Temperature != nil {
			t.Fatalf("%s: temperature must be withheld, got %v", name, *adapter.Temperature)
		}
		params, err := adapter.messageParams(domain.ModelSelection{Model: "m"}, domain.ModelRequest{Prompt: []byte("hi")})
		if err != nil || strings.Contains(encoded(t, params), "temperature") {
			t.Fatalf("%s: request must not carry a temperature: %v", name, err)
		}
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

func TestRequestTemperatureOverridesTheConfiguredOne(t *testing.T) {
	configured, requested := 0.7, 0.0
	request := domain.ModelRequest{Prompt: []byte("hi"), Temperature: &requested}
	anthropicParams, err := (&Anthropic{Temperature: &configured}).messageParams(domain.ModelSelection{Model: "m"}, request)
	if err != nil || !strings.Contains(encoded(t, anthropicParams), `"temperature":0`) {
		t.Fatalf("anthropic params = %s, %v", encoded(t, anthropicParams), err)
	}
	openAIParams, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, request)
	if err != nil || !strings.Contains(encoded(t, openAIParams), `"temperature":0`) {
		t.Fatalf("openai params = %s, %v", encoded(t, openAIParams), err)
	}
	_, config, err := (&Google{}).contentParams(request)
	if err != nil || config.Temperature == nil || *config.Temperature != 0 {
		t.Fatalf("google config = %+v, %v", config, err)
	}
}
