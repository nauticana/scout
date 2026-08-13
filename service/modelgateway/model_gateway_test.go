package modelgateway

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func validModelRequest() domain.ModelRequest {
	return domain.ModelRequest{
		TenantContext:   domain.TenantContext{TenantID: 7},
		RequestID:       "request",
		ConversationID:  "conversation",
		MaxOutputTokens: 100,
	}
}

func TestGatewayGenerateAppliesControlsAndReleasesCapacity(t *testing.T) {
	var calls []string
	registry := NewProviderRegistry()
	provider := &fake.ModelProvider{
		GenerateFunc: func(_ context.Context, selection domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
			calls = append(calls, "generate")
			if selection.CapacityPool != "shared" {
				t.Fatalf("capacity pool = %q", selection.CapacityPool)
			}
			return domain.ModelResult{Usage: domain.Usage{InputTokens: 3, Currency: "USD"}}, nil
		},
		StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
			return nil, nil
		},
	}
	if err := registry.Register("provider", provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	gateway := &Gateway{
		RateLimiter: &fake.TenantRateLimiter{AllowModelCallFunc: func(context.Context, domain.ModelRequest) error {
			calls = append(calls, "rate")
			return nil
		}},
		Providers: registry,
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			calls = append(calls, "capacity")
			return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(_ context.Context, usage domain.Usage) error {
				calls = append(calls, "release")
				if usage.InputTokens != 3 {
					t.Fatalf("usage = %+v", usage)
				}
				return nil
			}}, nil
		}),
	}
	_, err := gateway.Generate(context.Background(), domain.ModelSelection{Provider: "provider", Model: "model"}, validModelRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := []string{"rate", "capacity", "generate", "release"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestGatewayGenerateJoinsProviderAndReleaseErrors(t *testing.T) {
	providerErr := errors.New("provider failed")
	releaseErr := errors.New("release failed")
	registry := NewProviderRegistry()
	provider := &fake.ModelProvider{
		GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			return domain.ModelResult{}, providerErr
		},
		StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
			return nil, nil
		},
	}
	_ = registry.Register("provider", provider)
	gateway := &Gateway{
		RateLimiter: &fake.TenantRateLimiter{},
		Providers:   registry,
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(context.Context, domain.Usage) error {
				return releaseErr
			}}, nil
		}),
	}
	_, err := gateway.Generate(context.Background(), domain.ModelSelection{Provider: "provider", Model: "model"}, validModelRequest())
	if !errors.Is(err, providerErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("error = %v", err)
	}
}

func TestGatewayStreamAggregatesUsageAndReleasesOnFinalChunk(t *testing.T) {
	registry := NewProviderRegistry()
	chunks := []domain.ModelChunk{
		{Sequence: 1, Payload: []byte("a"), Usage: domain.Usage{InputTokens: 2, Currency: "USD"}},
		{Sequence: 2, Payload: []byte("b"), FinishReason: "stop", Usage: domain.Usage{OutputTokens: 3, CostMinorUnits: 4, Currency: "USD"}},
	}
	index := 0
	closed := 0
	provider := &fake.ModelProvider{
		GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			return domain.ModelResult{}, nil
		},
		StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
			return &fake.ModelStream{
				ReceiveFunc: func(context.Context) (domain.ModelChunk, error) {
					chunk := chunks[index]
					index++
					return chunk, nil
				},
				CloseFunc: func() error {
					closed++
					return nil
				},
			}, nil
		},
	}
	_ = registry.Register("provider", provider)
	var released domain.Usage
	releaseCount := 0
	gateway := &Gateway{
		RateLimiter: &fake.TenantRateLimiter{},
		Providers:   registry,
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(_ context.Context, usage domain.Usage) error {
				releaseCount++
				released = usage
				return nil
			}}, nil
		}),
	}
	stream, err := gateway.Stream(context.Background(), domain.ModelSelection{Provider: "provider", Model: "model"}, validModelRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if _, err := stream.Receive(context.Background()); err != nil {
		t.Fatalf("Receive first: %v", err)
	}
	if _, err := stream.Receive(context.Background()); err != nil {
		t.Fatalf("Receive final: %v", err)
	}
	if _, err := stream.Receive(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Receive closed: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if releaseCount != 1 || closed != 1 || released.InputTokens != 2 || released.OutputTokens != 3 || released.CostMinorUnits != 4 {
		t.Fatalf("release count = %d, closed = %d, usage = %+v", releaseCount, closed, released)
	}
}

func TestGatewayStopsAtRateLimit(t *testing.T) {
	want := domain.ErrRateLimited
	gateway := &Gateway{
		RateLimiter: &fake.TenantRateLimiter{AllowModelCallFunc: func(context.Context, domain.ModelRequest) error { return want }},
		Providers:   NewProviderRegistry(),
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			t.Fatal("capacity must not be called")
			return nil, nil
		}),
	}
	_, err := gateway.Generate(context.Background(), domain.ModelSelection{Provider: "provider", Model: "model"}, validModelRequest())
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}
