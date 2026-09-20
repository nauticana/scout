package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type toolTables struct {
	profiles map[string]string
	versions map[string][]any
	bindings map[string]string
}

func newToolTables() *toolTables {
	return &toolTables{profiles: map[string]string{}, versions: map[string][]any{}, bindings: map[string]string{}}
}

func (tables *toolTables) clone() *toolTables {
	copied := newToolTables()
	for k, v := range tables.profiles {
		copied.profiles[k] = v
	}
	for k, v := range tables.versions {
		copied.versions[k] = v
	}
	for k, v := range tables.bindings {
		copied.bindings[k] = v
	}
	return copied
}

// toolTableFake applies writes to a working copy that only Commit publishes, so
// a rolled-back registration leaves nothing behind.
type toolTableFake struct {
	committed *toolTables
	working   *toolTables
	failOn    string
}

func (fake *toolTableFake) tables() *toolTables {
	if fake.working != nil {
		return fake.working
	}
	return fake.committed
}

func (fake *toolTableFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if name == fake.failOn {
		return nil, errors.New("write failed")
	}
	tables := fake.tables()
	key := func(n int) string { return fmt.Sprint(args[:n]...) }
	versionRow := func(versionKey string) []any {
		row := tables.versions[versionKey]
		return []any{row[1], row[2], tables.profiles[fmt.Sprint(row[0], row[1])], row[3], row[4], row[5], row[6], row[7], row[8], row[9]}
	}
	switch name {
	case qToolProfileEnsure:
		if _, ok := tables.profiles[key(2)]; !ok {
			tables.profiles[key(2)] = args[2].(string)
		}
	case qToolProfileGet:
		if displayName, ok := tables.profiles[key(2)]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{{displayName}}}, nil
		}
	case qToolVersionGet:
		if _, ok := tables.versions[key(3)]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{versionRow(key(3))}}, nil
		}
	case qToolVersionInsert:
		if _, ok := tables.versions[key(3)]; !ok {
			tables.versions[key(3)] = args
		}
	case qToolBindingGet:
		if version, ok := tables.bindings[key(4)]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{{version}}}, nil
		}
	case qToolBindingInsert:
		if _, ok := tables.bindings[key(4)]; !ok {
			tables.bindings[key(4)] = args[4].(string)
		}
	case qToolBoundList:
		result := &keelmodel.QueryResult{}
		for _, tool := range []string{"alpha", "beta"} {
			if version, ok := tables.bindings[fmt.Sprint(args[0], args[1], args[2], tool)]; ok {
				result.Rows = append(result.Rows, versionRow(fmt.Sprint(args[0], tool, version)))
			}
		}
		return result, nil
	}
	return &keelmodel.QueryResult{}, nil
}

func (*toolTableFake) GenID() int64 { return 0 }

func (fake *toolTableFake) Commit(context.Context) error {
	fake.committed, fake.working = fake.working, nil
	return nil
}

func (fake *toolTableFake) Rollback(context.Context) error {
	fake.working = nil
	return nil
}

type toolTableDB struct {
	keelport.DatabaseRepository
	fake *toolTableFake
}

func (db toolTableDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.fake
}

func (db toolTableDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	db.fake.working = db.fake.committed.clone()
	return db.fake, nil
}

func newTableRegistry() (*toolTableFake, *TableToolRegistry) {
	fake := &toolTableFake{committed: newToolTables()}
	return fake, &TableToolRegistry{DB: toolTableDB{fake: fake}, DefaultTimeout: time.Second, DefaultMaxAttempts: 2}
}

func searchTool(toolID, version string) domain.ToolDefinition {
	return domain.ToolDefinition{
		ToolID: toolID, Version: version, Endpoint: "https://tools.example/" + toolID,
		InputSchema:  []byte(`{"type":"object","required":["q"],"properties":{"q":{"type":"string"}}}`),
		OutputSchema: []byte(`{"type":"object"}`),
	}
}

func TestRegisterIsIdempotentAndRejectsChangedContent(t *testing.T) {
	_, registry := newTableRegistry()
	ctx := context.Background()
	if err := registry.Register(ctx, 1, searchTool("alpha", "1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reformatted := searchTool("alpha", "1")
	reformatted.InputSchema = []byte(`{ "properties": {"q": {"type": "string"}}, "required": ["q"], "type": "object" }`)
	if err := registry.Register(ctx, 1, reformatted); err != nil {
		t.Fatalf("the same contract in another layout must be a no-op, got %v", err)
	}
	changed := searchTool("alpha", "1")
	changed.Endpoint = "https://elsewhere.example/alpha"
	if err := registry.Register(ctx, 1, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for changed content, got %v", err)
	}
	verified := searchTool("alpha", "1")
	verified.VerifyEffect = true
	if err := registry.Register(ctx, 1, verified); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("effect verification is part of the contract, got %v", err)
	}
	unverified := searchTool("gamma", "1")
	unverified.RetryWhenEffectAbsent = true
	if err := registry.Register(ctx, 1, unverified); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("resending an absent effect requires verification, got %v", err)
	}
	renamed := searchTool("alpha", "2")
	renamed.DisplayName = "renamed"
	if err := registry.Register(ctx, 1, renamed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for changed profile content, got %v", err)
	}
	stored, err := registry.Get(ctx, 1, "alpha", "1")
	if err != nil || stored.Endpoint != "https://tools.example/alpha" || stored.Timeout != time.Second || stored.MaxAttempts != 2 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

func TestRegisterRejectsInvalidSchemasBeforeWriting(t *testing.T) {
	fake, registry := newTableRegistry()
	for name, mutate := range map[string]func(*domain.ToolDefinition){
		"missing input schema":    func(tool *domain.ToolDefinition) { tool.InputSchema = nil },
		"malformed output schema": func(tool *domain.ToolDefinition) { tool.OutputSchema = []byte(`{"type":`) },
		"unsupported type":        func(tool *domain.ToolDefinition) { tool.InputSchema = []byte(`{"type":"tuple"}`) },
	} {
		tool := searchTool("alpha", "1")
		mutate(&tool)
		if err := registry.Register(context.Background(), 1, tool); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	if len(fake.committed.profiles) != 0 || len(fake.committed.versions) != 0 {
		t.Fatalf("an invalid contract must write nothing, got %+v", fake.committed)
	}
}

func TestGetCannotCrossTenants(t *testing.T) {
	_, registry := newTableRegistry()
	ctx := context.Background()
	if err := registry.Register(ctx, 1, searchTool("alpha", "1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := registry.Get(ctx, 2, "alpha", "1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant must see ErrNotFound, got %v", err)
	}
}

func TestFailedVersionInsertLeavesNoProfile(t *testing.T) {
	fake, registry := newTableRegistry()
	fake.failOn = qToolVersionInsert
	if err := registry.Register(context.Background(), 1, searchTool("alpha", "1")); err == nil {
		t.Fatal("want the insert failure")
	}
	if len(fake.committed.profiles) != 0 {
		t.Fatalf("a profile must not outlive its failed version, got %v", fake.committed.profiles)
	}
}

func TestListReturnsOnlyVersionsBoundToThePinnedAgentVersion(t *testing.T) {
	_, registry := newTableRegistry()
	ctx := context.Background()
	for _, tool := range []domain.ToolDefinition{searchTool("alpha", "1"), searchTool("alpha", "2"), searchTool("beta", "1")} {
		if err := registry.Register(ctx, 1, tool); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	if err := registry.Bind(ctx, 1, "writer", "v1", []domain.ToolReference{{ToolID: "alpha", Version: "2"}}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	bound, err := registry.List(ctx, 1, "writer", "v1")
	if err != nil || len(bound) != 1 || bound[0].ToolID != "alpha" || bound[0].Version != "2" {
		t.Fatalf("bound = %+v, %v", bound, err)
	}
	if other, _ := registry.List(ctx, 1, "writer", "v2"); len(other) != 0 {
		t.Fatalf("an unbound agent version must list nothing, got %+v", other)
	}
}

func TestBindIsAtomicAndImmutable(t *testing.T) {
	fake, registry := newTableRegistry()
	ctx := context.Background()
	if err := registry.Register(ctx, 1, searchTool("alpha", "1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	missing := []domain.ToolReference{{ToolID: "alpha", Version: "1"}, {ToolID: "beta", Version: "9"}}
	if err := registry.Bind(ctx, 1, "writer", "v1", missing); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound for an unregistered version, got %v", err)
	}
	if len(fake.committed.bindings) != 0 {
		t.Fatalf("a failed bind must leave no partial binding, got %v", fake.committed.bindings)
	}
	if err := registry.Register(ctx, 1, searchTool("alpha", "2")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Bind(ctx, 1, "writer", "v1", missing[:1]); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.Bind(ctx, 1, "writer", "v1", missing[:1]); err != nil {
		t.Fatalf("rebinding the same version must be a no-op, got %v", err)
	}
	if err := registry.Bind(ctx, 1, "writer", "v1", []domain.ToolReference{{ToolID: "alpha", Version: "2"}}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict when repinning a bound tool, got %v", err)
	}
}
