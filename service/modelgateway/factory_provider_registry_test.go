package modelgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type countingFactory struct {
	builds int
	err    error
}

type builtProvider struct{ contract.ModelProvider }

func (f *countingFactory) Build(context.Context, domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error) {
	f.builds++
	if f.err != nil {
		return nil, nil, f.err
	}
	return &builtProvider{}, nil, nil
}

func TestFactoryProviderRegistryBuildsOncePerModelAndRebuildsAfterTTL(t *testing.T) {
	factory := &countingFactory{}
	at := time.Unix(1000, 0)
	registry := &FactoryProviderRegistry{Factory: factory, TTL: time.Minute, Now: func() time.Time { return at }}
	ctx := context.Background()
	first, err := registry.ProviderFor(ctx, domain.ModelSelection{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatalf("ProviderFor: %v", err)
	}
	second, _ := registry.ProviderFor(ctx, domain.ModelSelection{Provider: " p ", Model: "m", Region: "eu"})
	if first != second || factory.builds != 1 {
		t.Fatalf("one model must reuse its adapter, builds = %d", factory.builds)
	}
	if _, _ = registry.ProviderFor(ctx, domain.ModelSelection{Provider: "p", Model: "other"}); factory.builds != 2 {
		t.Fatalf("a second model builds its own adapter, builds = %d", factory.builds)
	}
	at = at.Add(time.Minute)
	if rebuilt, _ := registry.ProviderFor(ctx, domain.ModelSelection{Provider: "p", Model: "m"}); rebuilt == first || factory.builds != 3 {
		t.Fatalf("an expired adapter must be rebuilt, builds = %d", factory.builds)
	}
}

func TestFactoryProviderRegistryNeverCachesAFailure(t *testing.T) {
	factory := &countingFactory{err: domain.ErrNotReady}
	registry := &FactoryProviderRegistry{Factory: factory}
	selection := domain.ModelSelection{Provider: "p", Model: "m"}
	if _, err := registry.ProviderFor(context.Background(), selection); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("want the factory error, got %v", err)
	}
	factory.err = nil
	if _, err := registry.ProviderFor(context.Background(), selection); err != nil || factory.builds != 2 {
		t.Fatalf("a failed build must be retried: %v, builds = %d", err, factory.builds)
	}
	if _, err := registry.ProviderFor(context.Background(), domain.ModelSelection{Provider: "p"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	if _, err := (&FactoryProviderRegistry{}).ProviderFor(context.Background(), selection); err == nil {
		t.Fatal("a registry without a factory must fail")
	}
}
