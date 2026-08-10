package modelgateway

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/internal/fake"
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
