package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/internal/fake"
)

func TestDefinitionResolverReturnsCacheHit(t *testing.T) {
	want := domain.ExecutionGraph{AgentID: "agent", Version: "v1"}
	resolver := &DefinitionResolver{
		Repository: &fake.ExecutionGraphRepository{GetFunc: func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
			t.Fatal("repository must not be called")
			return domain.ExecutionGraph{}, nil
		}},
		Cache: &fake.ExecutionGraphCache{GetFunc: func(context.Context, int64, string, string) (domain.ExecutionGraph, bool, error) {
			return want, true, nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	got, err := resolver.Resolve(context.Background(), 7, "agent", "v1")
	if err != nil || got.AgentID != want.AgentID {
		t.Fatalf("graph = %+v, error = %v", got, err)
	}
}

func TestDefinitionResolverFallsBackAndReportsCacheFailure(t *testing.T) {
	cacheErr := errors.New("cache unavailable")
	want := domain.ExecutionGraph{AgentID: "agent", Version: "v1"}
	put := false
	var reported error
	resolver := &DefinitionResolver{
		Repository: &fake.ExecutionGraphRepository{GetFunc: func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
			return want, nil
		}},
		Cache: &fake.ExecutionGraphCache{
			GetFunc: func(context.Context, int64, string, string) (domain.ExecutionGraph, bool, error) {
				return domain.ExecutionGraph{}, false, cacheErr
			},
			PutFunc: func(context.Context, int64, domain.ExecutionGraph) error {
				put = true
				return nil
			},
		},
		Metrics: &fake.RuntimeMetrics{RecordDependencyFunc: func(_ context.Context, _ int64, dependency, _ string, _ domain.Usage, err error) {
			if dependency != "execution_graph_cache" {
				t.Fatalf("dependency = %q", dependency)
			}
			reported = err
		}},
	}
	got, err := resolver.Resolve(context.Background(), 7, "agent", "v1")
	if err != nil || got.AgentID != want.AgentID || !put || !errors.Is(reported, cacheErr) {
		t.Fatalf("graph = %+v, put = %v, reported = %v, error = %v", got, put, reported, err)
	}
}

func TestDefinitionResolverRejectsDurableIdentityMismatch(t *testing.T) {
	resolver := &DefinitionResolver{
		Repository: &fake.ExecutionGraphRepository{GetFunc: func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
			return domain.ExecutionGraph{AgentID: "other", Version: "v1"}, nil
		}},
		Cache: &fake.ExecutionGraphCache{GetFunc: func(context.Context, int64, string, string) (domain.ExecutionGraph, bool, error) {
			return domain.ExecutionGraph{}, false, nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	_, err := resolver.Resolve(context.Background(), 7, "agent", "v1")
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}
