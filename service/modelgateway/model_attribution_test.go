package modelgateway

import (
	"context"
	"reflect"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func attributionGateway(t *testing.T, provider *fake.ModelProvider) *Gateway {
	t.Helper()
	registry := NewProviderRegistry()
	if err := registry.Register("anthropic", provider); err != nil {
		t.Fatal(err)
	}
	return &Gateway{
		RateLimiter: &fake.TenantRateLimiter{},
		Providers:   registry,
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(context.Context, domain.Usage) error { return nil }}, nil
		}),
	}
}

func TestGatewayAttributesAGenerationToTheModelThatServedIt(t *testing.T) {
	provider := &fake.ModelProvider{
		GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			usage := domain.Usage{InputTokens: 3, OutputTokens: 4, ToolCalls: 1, SearchQueries: 2, CostMinorUnits: 5, Currency: "USD"}
			usage.Models = []domain.ModelSpend{{Model: domain.ModelReference{ProviderID: "spoofed", ModelID: "other"}, InputTokens: 3}}
			return domain.ModelResult{Output: []byte("ok"), Usage: usage}, nil
		},
	}
	result, err := attributionGateway(t, provider).Generate(context.Background(), domain.ModelSelection{Provider: "anthropic", Model: "claude"}, validModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ModelSpend{{Model: domain.ModelReference{ProviderID: "anthropic", ModelID: "claude"}, InputTokens: 3, OutputTokens: 4, SearchQueries: 2, CostMinorUnits: 5}}
	if !reflect.DeepEqual(result.Usage.Models, want) || result.Usage.ToolCalls != 1 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestGatewayAttributesEveryStreamFrameToTheServingModel(t *testing.T) {
	chunks := []domain.ModelChunk{
		{Sequence: 1, Payload: []byte("a"), Usage: domain.Usage{InputTokens: 2, Currency: "USD"}},
		{Sequence: 2, Payload: []byte("b")},
		{Sequence: 3, FinishReason: "stop", Usage: domain.Usage{OutputTokens: 3, CostMinorUnits: 4, Currency: "USD"}},
	}
	index := 0
	provider := &fake.ModelProvider{
		StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
			return &fake.ModelStream{
				ReceiveFunc: func(context.Context) (domain.ModelChunk, error) {
					index++
					return chunks[index-1], nil
				},
				CloseFunc: func() error { return nil },
			}, nil
		},
	}
	stream, err := attributionGateway(t, provider).Stream(context.Background(), domain.ModelSelection{Provider: "anthropic", Model: "claude"}, validModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	served := domain.ModelReference{ProviderID: "anthropic", ModelID: "claude"}
	var spends [][]domain.ModelSpend
	for range chunks {
		chunk, err := stream.Receive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		spends = append(spends, chunk.Usage.Models)
	}
	want := [][]domain.ModelSpend{
		{{Model: served, InputTokens: 2}},
		nil,
		{{Model: served, OutputTokens: 3, CostMinorUnits: 4}},
	}
	if !reflect.DeepEqual(spends, want) {
		t.Fatalf("frame attribution = %+v", spends)
	}
}
