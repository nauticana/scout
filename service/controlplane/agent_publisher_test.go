package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func validDefinition() domain.AgentDefinition {
	return domain.AgentDefinition{AgentID: "agent", Version: "v1", DefinitionDigest: strings.Repeat("a", 64)}
}

func validGraph() domain.ExecutionGraph {
	return domain.ExecutionGraph{AgentID: "agent", Version: "v1", Digest: strings.Repeat("b", 64), EntryStepID: "start"}
}

func TestAgentPublisherCompilesAndStores(t *testing.T) {
	definition := validDefinition()
	graph := validGraph()
	stored := false
	publisher := &AgentPublisher{
		Compiler: fake.AgentCompilerFunc(func(_ context.Context, got domain.AgentDefinition) (domain.ExecutionGraph, error) {
			if got.AgentID != definition.AgentID {
				t.Fatalf("definition = %+v", got)
			}
			return graph, nil
		}),
		Store: fake.AgentPublicationStoreFunc(func(_ context.Context, tenantID int64, gotDefinition domain.AgentDefinition, gotGraph domain.ExecutionGraph) error {
			stored = true
			if tenantID != 7 || gotDefinition.AgentID != definition.AgentID || gotGraph.Digest != graph.Digest {
				t.Fatalf("store arguments = %d %+v %+v", tenantID, gotDefinition, gotGraph)
			}
			return nil
		}),
	}
	if err := publisher.Publish(context.Background(), 7, definition); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !stored {
		t.Fatal("publication was not stored")
	}
}

func TestAgentPublisherStopsAfterCompilerFailure(t *testing.T) {
	want := errors.New("compile failed")
	stored := false
	publisher := &AgentPublisher{
		Compiler: fake.AgentCompilerFunc(func(context.Context, domain.AgentDefinition) (domain.ExecutionGraph, error) {
			return domain.ExecutionGraph{}, want
		}),
		Store: fake.AgentPublicationStoreFunc(func(context.Context, int64, domain.AgentDefinition, domain.ExecutionGraph) error {
			stored = true
			return nil
		}),
	}
	err := publisher.Publish(context.Background(), 7, validDefinition())
	if !errors.Is(err, want) || stored {
		t.Fatalf("error = %v, stored = %v", err, stored)
	}
}

func TestAgentPublisherRejectsInconsistentGraph(t *testing.T) {
	graph := validGraph()
	graph.Version = "other"
	publisher := &AgentPublisher{
		Compiler: fake.AgentCompilerFunc(func(context.Context, domain.AgentDefinition) (domain.ExecutionGraph, error) {
			return graph, nil
		}),
		Store: fake.AgentPublicationStoreFunc(func(context.Context, int64, domain.AgentDefinition, domain.ExecutionGraph) error {
			t.Fatal("store must not be called")
			return nil
		}),
	}
	if err := publisher.Publish(context.Background(), 7, validDefinition()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentPublisherRejectsNonHexDigest(t *testing.T) {
	definition := validDefinition()
	definition.DefinitionDigest = strings.Repeat("z", 64)
	publisher := &AgentPublisher{}
	if err := publisher.Publish(context.Background(), 7, definition); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentPublisherSurfacesStoreFailure(t *testing.T) {
	want := errors.New("store failed")
	publisher := &AgentPublisher{
		Compiler: fake.AgentCompilerFunc(func(context.Context, domain.AgentDefinition) (domain.ExecutionGraph, error) {
			return validGraph(), nil
		}),
		Store: fake.AgentPublicationStoreFunc(func(context.Context, int64, domain.AgentDefinition, domain.ExecutionGraph) error {
			return want
		}),
	}
	if err := publisher.Publish(context.Background(), 7, validDefinition()); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}
