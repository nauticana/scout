package skill

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// catalogFake keeps the catalog tables and the tenant versions that name an origin.
type catalogFake struct {
	profiles map[string]string
	versions map[string][]any
	tools    map[string][]string
	examples map[string][]string
	derived  map[string][][]any
}

func (fake *catalogFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	key := fmt.Sprint(args[:min(2, len(args))]...)
	switch name {
	case qCatalogProfileEnsure:
		if _, ok := fake.profiles[args[0].(string)]; !ok {
			fake.profiles[args[0].(string)] = args[1].(string)
		}
	case qCatalogProfileLock:
		if displayName, ok := fake.profiles[args[0].(string)]; ok {
			return rows([]string{displayName}), nil
		}
	case qCatalogVersionGet:
		if row, ok := fake.versions[key]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{{row[0], row[1], fake.profiles[row[0].(string)], row[2], row[3], row[4]}}}, nil
		}
	case qCatalogVersionInsert:
		if _, ok := fake.versions[key]; !ok {
			fake.versions[key] = args
		}
	case qCatalogToolList:
		return rows(fake.tools[key]), nil
	case qCatalogToolInsert:
		fake.tools[key] = append(fake.tools[key], args[2].(string))
	case qCatalogExampleList:
		return rows(fake.examples[key]), nil
	case qCatalogExampleInsert:
		fake.examples[key] = append(fake.examples[key], args[3].(string))
	case qCatalogDerived:
		return &keelmodel.QueryResult{Rows: fake.derived[key]}, nil
	}
	return &keelmodel.QueryResult{}, nil
}

func (*catalogFake) GenID() int64                   { return 0 }
func (*catalogFake) Commit(context.Context) error   { return nil }
func (*catalogFake) Rollback(context.Context) error { return nil }

type catalogDB struct {
	keelport.DatabaseRepository
	fake *catalogFake
}

func (db catalogDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.fake
}

func (db catalogDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.fake, nil
}

func newCatalog() (*catalogFake, *TableSkillCatalog) {
	fake := &catalogFake{
		profiles: map[string]string{}, versions: map[string][]any{}, tools: map[string][]string{},
		examples: map[string][]string{}, derived: map[string][][]any{},
	}
	return fake, &TableSkillCatalog{DB: catalogDB{fake: fake}}
}

func catalogSkill() domain.SkillDefinition {
	skill := auditSkill("1")
	skill.EvalSet = nil
	return skill
}

func TestCatalogRegisterIsIdempotentAndRejectsChangedContent(t *testing.T) {
	_, catalog := newCatalog()
	ctx := context.Background()
	if err := catalog.Register(ctx, catalogSkill()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := catalog.Register(ctx, catalogSkill()); err != nil {
		t.Fatalf("the same skill again must be a no-op, got %v", err)
	}
	changed := catalogSkill()
	changed.Procedure += "\n3. Report."
	if err := catalog.Register(ctx, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed procedure: want ErrConflict, got %v", err)
	}
	stored, err := catalog.Get(ctx, "audit", "1")
	if err != nil || !slices.Equal(stored.Tools, []string{"crawl", "propose_edit"}) || len(stored.Examples) != 2 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if _, err = catalog.Get(ctx, "audit", "2"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unregistered version: want ErrNotFound, got %v", err)
	}
}

func TestCatalogRegisterRefusesTenantOnlyFields(t *testing.T) {
	fake, catalog := newCatalog()
	reference := &domain.SkillReference{SkillID: "base", Version: "1"}
	for name, mutate := range map[string]func(*domain.SkillDefinition){
		"eval set": func(skill *domain.SkillDefinition) {
			skill.EvalSet = &domain.GoldenSetReference{GoldenSetID: "g", SetVersion: 1}
		},
		"origin":      func(skill *domain.SkillDefinition) { skill.DerivedFrom = reference },
		"requirement": func(skill *domain.SkillDefinition) { skill.Requires = []domain.SkillReference{*reference} },
	} {
		skill := catalogSkill()
		mutate(&skill)
		if err := catalog.Register(context.Background(), skill); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	if len(fake.versions) != 0 {
		t.Fatalf("a refused skill must write nothing, got %+v", fake.versions)
	}
}

func TestCatalogDerivedListsTenantCopies(t *testing.T) {
	fake, catalog := newCatalog()
	fake.derived[fmt.Sprint("audit", "1")] = [][]any{{int64(1), "audit", "1"}, {int64(2), "site-audit", "3"}}
	derived, err := catalog.Derived(context.Background(), domain.SkillReference{SkillID: "audit", Version: "1"})
	want := []domain.TenantSkillReference{{TenantID: 1, SkillID: "audit", Version: "1"}, {TenantID: 2, SkillID: "site-audit", Version: "3"}}
	if err != nil || !slices.Equal(derived, want) {
		t.Fatalf("Derived = %+v, %v", derived, err)
	}
	if _, err = catalog.Derived(context.Background(), domain.SkillReference{SkillID: "audit"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a versionless origin: want ErrValidation, got %v", err)
	}
}
