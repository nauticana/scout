package policy

import (
	"context"
	"errors"
	"fmt"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/guardrail"
)

// restrictionTables keeps the layer versions and current pointers; key 0 is the platform.
type restrictionTables struct {
	layers  map[string][2]string
	current map[int64]string
	reads   int
}

func (tables *restrictionTables) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	tenant := func() int64 {
		if len(args) > 0 {
			if id, ok := args[0].(int64); ok {
				return id
			}
		}
		return 0
	}
	digestRow := func(key int64) *keelmodel.QueryResult {
		if digest, ok := tables.current[key]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{{digest}}}
		}
		return &keelmodel.QueryResult{}
	}
	switch name {
	case qPlatformLayerInsert:
		tables.layers[fmt.Sprint(0, args[0])] = [2]string{args[1].(string), args[2].(string)}
	case qTenantLayerInsert:
		tables.layers[fmt.Sprint(args[0], args[1])] = [2]string{args[2].(string), args[3].(string)}
	case qPlatformCurrentRead, qTenantCurrentRead:
		tables.reads++
		key := tenant()
		digest, ok := tables.current[key]
		if !ok {
			return &keelmodel.QueryResult{}, nil
		}
		layer := tables.layers[fmt.Sprint(key, digest)]
		return &keelmodel.QueryResult{Rows: [][]any{{digest, layer[0], layer[1]}}}, nil
	case qPlatformCurrentLock, qTenantCurrentLock:
		return digestRow(tenant()), nil
	case qPlatformCurrentInsert, qTenantCurrentInsert:
		key := tenant()
		if _, ok := tables.current[key]; !ok {
			tables.current[key] = args[len(args)-1].(string)
			return digestRow(key), nil
		}
	case qPlatformCurrentSwap, qTenantCurrentSwap:
		var key int64
		if name == qTenantCurrentSwap {
			key = args[1].(int64)
		}
		if tables.current[key] == args[len(args)-1].(string) {
			tables.current[key] = args[0].(string)
			return digestRow(key), nil
		}
	}
	return &keelmodel.QueryResult{}, nil
}

func (*restrictionTables) GenID() int64                   { return 0 }
func (*restrictionTables) Commit(context.Context) error   { return nil }
func (*restrictionTables) Rollback(context.Context) error { return nil }

type restrictionDB struct {
	keelport.DatabaseRepository
	tables *restrictionTables
}

func (db restrictionDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.tables
}

func (db restrictionDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.tables, nil
}

var platformService = domain.Principal{Kind: domain.PrincipalService, ID: "usage-policy"}

func newRestrictions(t *testing.T) (*restrictionTables, *TableRestrictionLayers, *[]domain.DecisionRecord) {
	t.Helper()
	compiler, err := guardrail.NewRuleSetCompiler(guardrail.CompilerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tables := &restrictionTables{layers: map[string][2]string{}, current: map[int64]string{}}
	audits := &[]domain.DecisionRecord{}
	store := &TableRestrictionLayers{DB: restrictionDB{tables: tables}, Rules: compiler, Audit: &fake.AuditSink{
		RecordFunc: func(_ context.Context, record domain.DecisionRecord) error {
			*audits = append(*audits, record)
			return nil
		},
	}}
	return tables, store, audits
}

func denial(id string) domain.PolicyStatement {
	return domain.PolicyStatement{ID: id, Effect: domain.PolicyDeny, Actions: []string{"tool.invoke"}, Resources: []string{"payments.*"}}
}

func noCodename() domain.GuardrailRule {
	return domain.GuardrailRule{ID: "codename", Kind: domain.GuardrailKindExactPhrase, Action: domain.GuardrailActionBlock,
		Severity: domain.GuardrailSeverityHard, Params: []byte(`{"phrases":["BLUEJAY"]}`)}
}

func TestReplaceTenantSwapsByDigestAndAuditsOnlyChanges(t *testing.T) {
	_, store, audits := newRestrictions(t)
	ctx := context.Background()
	layer := domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("b"), denial("a")}, Guardrails: []domain.GuardrailRule{noCodename()}}
	first, err := store.ReplaceTenant(ctx, platformService, 7, layer, "")
	if err != nil || len(first) != 64 {
		t.Fatalf("first write = %q, %v", first, err)
	}
	reordered := domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("a"), denial("b")}, Guardrails: layer.Guardrails}
	if again, err := store.ReplaceTenant(ctx, platformService, 7, reordered, "stale"); err != nil || again != first {
		t.Fatalf("the value in force again must succeed unchanged: %q, %v", again, err)
	}
	if len(*audits) != 1 || (*audits)[0].Category != domain.DecisionCategoryRestriction || (*audits)[0].TenantID != 7 {
		t.Fatalf("audits = %+v", *audits)
	}
	narrower := domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("a")}}
	if current, err := store.ReplaceTenant(ctx, platformService, 7, narrower, ""); !errors.Is(err, domain.ErrConflict) || current != first {
		t.Fatalf("a stale expectation: want ErrConflict at %q, got %q, %v", first, current, err)
	}
	second, err := store.ReplaceTenant(ctx, platformService, 7, narrower, first)
	if err != nil || second == first {
		t.Fatalf("swap = %q, %v", second, err)
	}
	layers, err := store.Layers(ctx, 7)
	if err != nil || layers.Tenant.Digest != second || len(layers.Tenant.Denials) != 1 || layers.Platform.Digest != "" {
		t.Fatalf("own write must be visible at once: %+v, %v", layers, err)
	}
}

func TestReplaceRefusesAnythingButAServiceAddingRestrictions(t *testing.T) {
	_, store, _ := newRestrictions(t)
	ctx := context.Background()
	if _, err := store.ReplacePlatform(ctx, domain.Principal{Kind: domain.PrincipalHuman, ID: "ops"}, domain.RestrictionLayer{}, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a human writer: want ErrForbidden, got %v", err)
	}
	if _, err := store.ReplacePlatform(ctx, domain.Principal{Kind: domain.PrincipalService, ID: " "}, domain.RestrictionLayer{}, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a blank service writer: want ErrForbidden, got %v", err)
	}
	allow := denial("a")
	allow.Effect = domain.PolicyAllow
	flag := noCodename()
	flag.Action = domain.GuardrailActionFlag
	unbounded := denial("a")
	unbounded.Resources = nil
	badConditions := denial("a")
	badConditions.Conditions = []byte(`[]`)
	for name, layer := range map[string]domain.RestrictionLayer{
		"allow":        {Denials: []domain.PolicyStatement{allow}},
		"flag":         {Guardrails: []domain.GuardrailRule{flag}},
		"no resource":  {Denials: []domain.PolicyStatement{unbounded}},
		"conditions":   {Denials: []domain.PolicyStatement{badConditions}},
		"duplicate id": {Denials: []domain.PolicyStatement{denial("a"), denial("a")}},
		"bad rule":     {Guardrails: []domain.GuardrailRule{{ID: "x", Kind: "telepathy", Action: domain.GuardrailActionBlock}}},
	} {
		if _, err := store.ReplacePlatform(ctx, platformService, layer, ""); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	store.Audit = nil
	if _, err := store.ReplacePlatform(ctx, platformService, domain.RestrictionLayer{}, ""); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no audit sink: want ErrNotReady, got %v", err)
	}
}

func TestLayersCachesAndRefusesACorruptLayer(t *testing.T) {
	tables, store, _ := newRestrictions(t)
	ctx := context.Background()
	digest, err := store.ReplacePlatform(ctx, platformService, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("a")}}, "")
	if err != nil {
		t.Fatalf("ReplacePlatform: %v", err)
	}
	for range 3 {
		if layers, err := store.Layers(ctx, 7); err != nil || layers.Platform.Digest != digest {
			t.Fatalf("Layers = %+v, %v", layers, err)
		}
	}
	if tables.reads != 2 {
		t.Fatalf("store reads = %d, want one per layer", tables.reads)
	}
	tables.layers[fmt.Sprint(0, digest)] = [2]string{`[]`, tables.layers[fmt.Sprint(0, digest)][1]}
	other := &TableRestrictionLayers{DB: restrictionDB{tables: tables}}
	if _, err := other.Layers(ctx, 7); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a layer that no longer matches its digest: want ErrConflict, got %v", err)
	}
}
