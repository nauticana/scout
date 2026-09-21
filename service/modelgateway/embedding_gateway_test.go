package modelgateway

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func embeddingRoute(dimensions int) *fake.ModelCandidateCatalog {
	return &fake.ModelCandidateCatalog{Set: domain.ModelCandidateSet{Candidates: []domain.ModelCandidate{{
		Provider: "openai", Model: "embed", RouteID: "openai/embed",
		Capabilities: []string{domain.CapabilityEmbeddings}, EmbeddingDimensions: dimensions,
	}}}}
}

func vectors(count, dimensions int, tokens int64) []domain.Embedding {
	embeddings := make([]domain.Embedding, count)
	for index := range embeddings {
		embeddings[index] = domain.Embedding{Values: make([]float32, dimensions), Usage: domain.Usage{InputTokens: tokens}}
	}
	return embeddings
}

func embedderUnder(t *testing.T, catalog contract.ModelCandidateCatalog, provider contract.EmbeddingProvider, recorder *budgetRecorder) (*GovernedEmbedder, *[]domain.Usage) {
	t.Helper()
	var released []domain.Usage
	embedder, err := NewGovernedEmbedder(domain.ModelSelection{Provider: "openai", Model: "embed"},
		&fake.EmbeddingProviderRegistry{Provider: provider}, &fake.TenantRateLimiter{},
		fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(_ context.Context, usage domain.Usage) error {
				released = append(released, usage)
				return nil
			}}, nil
		}), recorder.manager(), fixedPricer(), catalog)
	if err != nil {
		t.Fatalf("NewGovernedEmbedder: %v", err)
	}
	return embedder, &released
}

func TestGovernedEmbedderEmbedsAtTheRouteWidthAndSettlesItsUsage(t *testing.T) {
	recorder := newBudgetRecorder()
	var seen domain.EmbeddingRequest
	provider := fake.EmbeddingProviderFunc(func(_ context.Context, _ domain.ModelSelection, request domain.EmbeddingRequest) ([]domain.Embedding, error) {
		seen = request
		return vectors(len(request.Inputs), request.Dimensions, 6), nil
	})
	embedder, released := embedderUnder(t, embeddingRoute(768), provider, recorder)
	contents := [][]byte{[]byte("first chunk"), []byte("second chunk")}
	embeddings, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{TenantID: 7}, contents)
	if err != nil || len(embeddings) != 2 {
		t.Fatalf("EmbedBatch = %d vectors, %v", len(embeddings), err)
	}
	if seen.Dimensions != 768 || len(seen.Inputs) != 2 {
		t.Fatalf("the route's width must reach the provider, got %+v", seen)
	}
	reservations, settled, releasedBudget := recorder.snapshot()
	if len(reservations) != 1 || len(releasedBudget) != 0 || len(settled) != 1 {
		t.Fatalf("reserved %v, settled %v, released %v", reservations, settled, releasedBudget)
	}
	for _, usage := range settled {
		if usage.InputTokens != 12 || usage.CostMinorUnits != 20 {
			t.Fatalf("the settled usage must be the reported tokens, priced: %+v", usage)
		}
	}
	if len(*released) != 1 || (*released)[0].InputTokens != 12 {
		t.Fatalf("the capacity lease must be released with the same usage, got %+v", *released)
	}
}

func TestGovernedEmbedderRefusesAVectorOfTheWrongWidth(t *testing.T) {
	recorder := newBudgetRecorder()
	provider := fake.EmbeddingProviderFunc(func(_ context.Context, _ domain.ModelSelection, request domain.EmbeddingRequest) ([]domain.Embedding, error) {
		return vectors(len(request.Inputs), 512, 6), nil
	})
	embedder, _ := embedderUnder(t, embeddingRoute(768), provider, recorder)
	_, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{TenantID: 7}, [][]byte{[]byte("chunk")})
	if !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("a mismatched width must never reach an index, err = %v", err)
	}
	if _, settled, _ := recorder.snapshot(); len(settled) != 1 {
		t.Fatalf("a call the provider ran is billed even when its vectors are unusable, settled = %v", settled)
	}
}

func TestGovernedEmbedderRefusesNonFiniteVector(t *testing.T) {
	recorder := newBudgetRecorder()
	provider := fake.EmbeddingProviderFunc(func(_ context.Context, _ domain.ModelSelection, _ domain.EmbeddingRequest) ([]domain.Embedding, error) {
		return []domain.Embedding{{Values: []float32{1, float32(math.NaN())}, Usage: domain.Usage{InputTokens: 2}}}, nil
	})
	embedder, _ := embedderUnder(t, embeddingRoute(2), provider, recorder)
	if _, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{TenantID: 7}, [][]byte{[]byte("chunk")}); !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("err = %v, want ErrInvalidModelOutput", err)
	}
}

func TestGovernedEmbedderRefusesNegativeUsage(t *testing.T) {
	recorder := newBudgetRecorder()
	provider := fake.EmbeddingProviderFunc(func(_ context.Context, _ domain.ModelSelection, _ domain.EmbeddingRequest) ([]domain.Embedding, error) {
		return []domain.Embedding{{Values: []float32{1, 2}, Usage: domain.Usage{InputTokens: -1}}}, nil
	})
	embedder, _ := embedderUnder(t, embeddingRoute(2), provider, recorder)
	if _, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{TenantID: 7}, [][]byte{[]byte("chunk")}); !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("err = %v, want ErrInvalidModelOutput", err)
	}
}

func TestGovernedEmbedderRefusesARouteWithoutTheCapability(t *testing.T) {
	recorder := newBudgetRecorder()
	called := false
	provider := fake.EmbeddingProviderFunc(func(context.Context, domain.ModelSelection, domain.EmbeddingRequest) ([]domain.Embedding, error) {
		called = true
		return nil, nil
	})
	catalog := embeddingRoute(768)
	catalog.Set.Candidates[0].Capabilities = []string{domain.CapabilityTools}
	embedder, _ := embedderUnder(t, catalog, provider, recorder)
	_, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{TenantID: 7}, [][]byte{[]byte("chunk")})
	if !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("a route without the capability must not be called")
	}
	if reserved, _, _ := recorder.snapshot(); len(reserved) != 0 {
		t.Fatalf("no budget is held for a refused route, reserved = %v", reserved)
	}
}

func TestGovernedEmbedderSettlesAVendorThatReportsNoTokens(t *testing.T) {
	recorder := newBudgetRecorder()
	provider := fake.EmbeddingProviderFunc(func(_ context.Context, _ domain.ModelSelection, request domain.EmbeddingRequest) ([]domain.Embedding, error) {
		return vectors(len(request.Inputs), 256, 0), nil
	})
	embedder, _ := embedderUnder(t, embeddingRoute(0), provider, recorder)
	content := []byte("a chunk of text to embed")
	if _, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{TenantID: 7}, [][]byte{content}); err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	_, settled, released := recorder.snapshot()
	if len(released) != 0 || len(settled) != 1 {
		t.Fatalf("settled %v, released %v", settled, released)
	}
	for _, usage := range settled {
		if usage.InputTokens != EstimatePromptTokens(content) {
			t.Fatalf("a vendor reporting no tokens is billed at the estimate, got %+v", usage)
		}
	}
}

func TestGovernedEmbedderValidatesItsCompositionAndInput(t *testing.T) {
	recorder := newBudgetRecorder()
	provider := fake.EmbeddingProviderFunc(func(context.Context, domain.ModelSelection, domain.EmbeddingRequest) ([]domain.Embedding, error) {
		return nil, nil
	})
	embedder, _ := embedderUnder(t, embeddingRoute(0), provider, recorder)
	if _, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{}, [][]byte{[]byte("x")}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a batch without a tenant = %v", err)
	}
	if _, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{TenantID: 7}, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an empty batch = %v", err)
	}
	if _, err := embedder.EmbedBatch(context.Background(), domain.TenantContext{TenantID: 7}, [][]byte{nil}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an empty input = %v", err)
	}
	if _, err := NewGovernedEmbedder(domain.ModelSelection{}, nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("an incomplete composition must be refused")
	}
}
