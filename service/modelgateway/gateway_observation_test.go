package modelgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func fullSelection() domain.ModelSelection {
	return domain.ModelSelection{Provider: "provider", Model: "model", ModelVersion: "2026-01", Region: "eu",
		RouteID: "eu-1", RoutingGeneration: 77, Reason: "preferred"}
}

// observedGateway wires a gateway whose provider echoes the selection it received.
func observedGateway(t *testing.T, provider *fake.ModelProvider, clock *time.Time) (*Gateway, *[]domain.Observation, *fake.ServingSignalObserver) {
	t.Helper()
	registry := NewProviderRegistry()
	if err := registry.Register("provider", provider); err != nil {
		t.Fatal(err)
	}
	observations := &[]domain.Observation{}
	signals := &fake.ServingSignalObserver{}
	gateway := &Gateway{
		RateLimiter: &fake.TenantRateLimiter{},
		Providers:   registry,
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(context.Context, domain.Usage) error { return nil }}, nil
		}),
		Observer: &fake.ObservationRecorder{RecordObservationFunc: func(_ context.Context, observation domain.Observation) {
			*observations = append(*observations, observation)
		}},
		Signals: signals,
		Now:     func() time.Time { return *clock },
	}
	return gateway, observations, signals
}

func TestGatewayPropagatesSelectionAndObservesGenerate(t *testing.T) {
	clock := routerNow
	var seen domain.ModelSelection
	provider := &fake.ModelProvider{GenerateFunc: func(_ context.Context, selection domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
		seen = selection
		clock = clock.Add(2 * time.Second)
		return domain.ModelResult{Usage: domain.Usage{InputTokens: 5, OutputTokens: 9, Currency: "USD"}}, nil
	}}
	gateway, observations, signals := observedGateway(t, provider, &clock)
	request := validModelRequest()
	request.TenantContext.Tier = "gold"
	request.TenantContext.Region = "eu"
	request.Prompt = []byte("12345678")

	if _, err := gateway.Generate(context.Background(), fullSelection(), request); err != nil {
		t.Fatal(err)
	}
	want := fullSelection()
	want.CapacityPool = "shared"
	if seen != want {
		t.Fatalf("provider selection = %+v, want %+v", seen, want)
	}
	if len(*observations) != 1 {
		t.Fatalf("observations = %+v", *observations)
	}
	observation := (*observations)[0]
	if observation.Selection != want || observation.Stage != domain.StageModel || observation.Outcome != domain.OutcomeOK ||
		observation.Versions.Model != "2026-01" || observation.TenantTier != "gold" || observation.Usage.OutputTokens != 9 ||
		observation.Duration != 2*time.Second {
		t.Fatalf("observation = %+v", observation)
	}
	if len(signals.Samples) != 2 || signals.Samples[0].CapacityOutcome != CapacityOutcomeGranted ||
		signals.Samples[0].PrefillTokens != 2 || signals.Samples[0].DecodeTokens != 100 ||
		signals.Samples[1].CapacityOutcome != CapacityOutcomeCompleted {
		t.Fatalf("samples = %+v", signals.Samples)
	}
}

func TestGatewayObservesAdmissionRejection(t *testing.T) {
	clock := routerNow
	gateway, observations, signals := observedGateway(t, &fake.ModelProvider{}, &clock)
	gateway.RateLimiter = &fake.TenantRateLimiter{AllowModelCallFunc: func(context.Context, domain.ModelRequest) error { return domain.ErrRateLimited }}

	if _, err := gateway.Generate(context.Background(), fullSelection(), validModelRequest()); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("error = %v", err)
	}
	if len(signals.Samples) != 1 || !signals.Samples[0].AdmissionRejected || signals.Samples[0].CapacityOutcome != CapacityOutcomeRejected {
		t.Fatalf("samples = %+v", signals.Samples)
	}
	if len(*observations) != 1 || (*observations)[0].Outcome != domain.OutcomeRejected || (*observations)[0].ErrorClass != ErrorClassRateLimited {
		t.Fatalf("observations = %+v", *observations)
	}
}

func TestGatewayStreamObservesTimeToFirstTokenOnce(t *testing.T) {
	clock := routerNow
	frames := []domain.ModelChunk{
		{Sequence: 1, Payload: []byte("a"), Usage: domain.Usage{OutputTokens: 1}},
		{Sequence: 2, Payload: []byte("b"), FinishReason: "stop", Usage: domain.Usage{OutputTokens: 2}},
	}
	index := 0
	provider := &fake.ModelProvider{StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
		return &fake.ModelStream{
			ReceiveFunc: func(context.Context) (domain.ModelChunk, error) {
				clock = clock.Add(time.Second)
				chunk := frames[index]
				index++
				return chunk, nil
			},
			CloseFunc: func() error { return nil },
		}, nil
	}}
	gateway, observations, _ := observedGateway(t, provider, &clock)

	stream, err := gateway.Stream(context.Background(), fullSelection(), validModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	for range frames {
		if _, err := stream.Receive(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if len(*observations) != 1 {
		t.Fatalf("observations = %+v", *observations)
	}
	observation := (*observations)[0]
	if observation.TimeToFirst != time.Second || observation.TimePerOutput != 500*time.Millisecond || observation.Outcome != domain.OutcomeOK {
		t.Fatalf("observation = %+v", observation)
	}
	if observation.Selection.CapacityPool != "shared" || observation.Selection.RouteID != "eu-1" || observation.Selection.RoutingGeneration != 77 {
		t.Fatalf("selection = %+v", observation.Selection)
	}
}

func TestGatewayStreamCloseBeforeFinishIsCanceled(t *testing.T) {
	clock := routerNow
	provider := &fake.ModelProvider{StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
		return &fake.ModelStream{
			ReceiveFunc: func(context.Context) (domain.ModelChunk, error) {
				return domain.ModelChunk{Sequence: 1, Payload: []byte("a")}, nil
			},
			CloseFunc: func() error { return nil },
		}, nil
	}}
	gateway, observations, signals := observedGateway(t, provider, &clock)
	stream, err := gateway.Stream(context.Background(), fullSelection(), validModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	if len(*observations) != 1 || (*observations)[0].Outcome != domain.OutcomeCanceled {
		t.Fatalf("observations = %+v", *observations)
	}
	if len(signals.Samples) != 2 || signals.Samples[1].CapacityOutcome != CapacityOutcomeCanceled {
		t.Fatalf("samples = %+v", signals.Samples)
	}
}
