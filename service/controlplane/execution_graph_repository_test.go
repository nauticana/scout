package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// graphTableFake is an in-memory execution_graph store that is also a
// transaction offering other catalogs, like keel's.
type graphTableFake struct {
	digests map[string]string
	steps   map[string][][]any
	entries map[string]int64
	edges   map[string][][]any
	nextID  int64
}

func newGraphTableFake() *graphTableFake {
	return &graphTableFake{digests: map[string]string{}, steps: map[string][][]any{}, entries: map[string]int64{}, edges: map[string][][]any{}, nextID: 100}
}

func (fake *graphTableFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	key := fmt.Sprint(args[:min(3, len(args))]...)
	switch name {
	case qGraphInsert:
		if _, exists := fake.digests[key]; exists {
			return &keelmodel.QueryResult{}, nil
		}
		fake.digests[key] = args[3].(string)
		return &keelmodel.QueryResult{Rows: [][]any{{args[2]}}}, nil
	case qGraphStepInsert:
		fake.nextID++
		fake.steps[key] = append(fake.steps[key], []any{fake.nextID, args[3], args[4], args[5]})
		return &keelmodel.QueryResult{Rows: [][]any{{fake.nextID}}}, nil
	case qGraphEntryInsert:
		fake.entries[key] = args[3].(int64)
	case qGraphGet:
		if digest, ok := fake.digests[key]; ok {
			for _, step := range fake.steps[key] {
				if step[0] == fake.entries[key] {
					return &keelmodel.QueryResult{Rows: [][]any{{digest, step[1]}}}, nil
				}
			}
		}
	case qGraphSteps:
		return &keelmodel.QueryResult{Rows: fake.steps[key]}, nil
	case qGraphEdges:
		return &keelmodel.QueryResult{Rows: fake.edges[key]}, nil
	}
	return &keelmodel.QueryResult{}, nil
}

func (*graphTableFake) GenID() int64                   { return 0 }
func (*graphTableFake) Commit(context.Context) error   { return nil }
func (*graphTableFake) Rollback(context.Context) error { return nil }
func (fake *graphTableFake) QueryService(string, map[string]string) keelport.QueryService {
	return fake
}

type graphDBFake struct {
	keelport.DatabaseRepository
	fake *graphTableFake
}

func (db graphDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.fake
}

func (db graphDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.fake, nil
}

func loopDefinition() domain.AgentDefinition {
	return domain.AgentDefinition{
		AgentID: "writer", AgentTypeID: "writer", Version: "3", DefinitionDigest: strings.Repeat("a", 64),
		Tools:    []domain.ToolReference{{ToolID: "search", Version: "1"}},
		ToolLoop: &domain.ToolLoopConfig{MaxIterations: 4},
	}
}

func TestWriteReleasePersistsTheCompiledGraphAndGetReturnsCompiledStepIDs(t *testing.T) {
	fake := newGraphTableFake()
	repository := &TableExecutionGraphRepository{DB: graphDBFake{fake: fake}, Compiler: ToolLoopGraphCompiler{}}
	ctx := context.Background()
	if err := repository.WriteRelease(ctx, fake, 7, loopDefinition()); err != nil {
		t.Fatalf("WriteRelease: %v", err)
	}
	graph, err := repository.Get(ctx, 7, "writer", "3")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if graph.EntryStepID != toolLoopStepID || len(graph.Steps) != 1 || graph.Steps[0].ExecutionStepID <= 0 ||
		graph.Steps[0].Kind != domain.StepKindToolLoop || !strings.Contains(string(graph.Steps[0].Configuration), `"max_iterations":4`) {
		t.Fatalf("graph = %+v", graph)
	}
	if err := repository.WriteRelease(ctx, fake, 7, loopDefinition()); err != nil {
		t.Fatalf("storing the same graph again must be a no-op, got %v", err)
	}
	changed := loopDefinition()
	changed.ToolLoop.MaxIterations = 9
	if err := repository.WriteRelease(ctx, fake, 7, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for a different graph under the same version, got %v", err)
	}
	if _, err := repository.Get(ctx, 8, "writer", "3"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant must see ErrNotFound, got %v", err)
	}
	plain := loopDefinition()
	plain.ToolLoop, plain.Version = nil, "4"
	if err := repository.WriteRelease(ctx, fake, 7, plain); err != nil || len(fake.digests) != 1 {
		t.Fatalf("a definition without a tool loop has no graph: %v, %v", err, fake.digests)
	}
}

func TestPutRejectsAnInconsistentGraph(t *testing.T) {
	repository := &TableExecutionGraphRepository{DB: graphDBFake{fake: newGraphTableFake()}}
	graph := domain.ExecutionGraph{AgentID: "writer", Version: "1", Digest: strings.Repeat("b", 64), EntryStepID: "missing",
		Steps: []domain.ExecutionStep{{StepID: "a", Kind: "model", NextStepIDs: []string{"ghost"}}}}
	if err := repository.Put(context.Background(), 7, graph); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}
