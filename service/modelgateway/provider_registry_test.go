package modelgateway

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func modelProvider() *fake.ModelProvider {
	return &fake.ModelProvider{
		GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			return domain.ModelResult{}, nil
		},
		StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
			return nil, nil
		},
	}
}

func TestProviderRegistryRegistersAndResolves(t *testing.T) {
	registry := NewProviderRegistry()
	provider := modelProvider()
	if err := registry.Register("provider", provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := registry.ProviderFor(context.Background(), domain.ModelSelection{Provider: "provider"})
	if err != nil || got != provider {
		t.Fatalf("provider = %T, error = %v", got, err)
	}
}

func TestProviderRegistryRejectsDuplicateAndMissingProviders(t *testing.T) {
	registry := NewProviderRegistry()
	provider := modelProvider()
	if err := registry.Register("provider", provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register("provider", provider); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := registry.ProviderFor(context.Background(), domain.ModelSelection{Provider: "missing"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestProviderRegistryBuildsTextAndMediaAdapters(t *testing.T) {
	registry := NewProviderRegistry()
	provider := modelProvider()
	media := &mediaProvider{}
	if err := registry.RegisterAdapters("provider", provider, media); err != nil {
		t.Fatalf("RegisterAdapters: %v", err)
	}
	gotProvider, gotMedia, err := registry.Build(context.Background(), domain.ModelReference{ProviderID: "provider", ModelID: "model"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if gotProvider != provider || gotMedia != media {
		t.Fatalf("adapters = (%T, %T)", gotProvider, gotMedia)
	}
	if _, _, err := registry.Build(context.Background(), domain.ModelReference{ProviderID: "provider"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing model error = %v", err)
	}
}

type mediaProvider struct{}

func (*mediaProvider) GenerateImage(context.Context, string, domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	return nil, nil
}

func (*mediaProvider) GenerateVideo(context.Context, string, domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	return nil, nil
}

var _ contract.MediaProvider = (*mediaProvider)(nil)
