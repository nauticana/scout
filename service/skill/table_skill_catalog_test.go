package skill

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	requires map[string][][2]string
	origins  map[string][]any
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
			origin := fake.origins[key]
			if origin == nil {
				origin = make([]any, 7)
			}
			return &keelmodel.QueryResult{Rows: [][]any{{row[0], row[1], fake.profiles[row[0].(string)], row[2], row[3], row[4],
				origin[2], origin[3], origin[4], origin[5], origin[6]}}}, nil
		}
	case qCatalogVersionInsert:
		if _, ok := fake.versions[key]; !ok {
			fake.versions[key] = args
		}
	case qCatalogOriginInsert:
		if _, ok := fake.origins[key]; !ok {
			fake.origins[key] = args
		}
	case qCatalogImported:
		result := &keelmodel.QueryResult{}
		for _, origin := range fake.origins {
			if origin[3] == args[0] || origin[5] == args[1] {
				result.Rows = append(result.Rows, origin[:2])
			}
		}
		return result, nil
	case qCatalogToolList:
		return rows(fake.tools[key]), nil
	case qCatalogToolInsert:
		fake.tools[key] = append(fake.tools[key], args[2].(string))
	case qCatalogExampleList:
		return rows(fake.examples[key]), nil
	case qCatalogExampleInsert:
		fake.examples[key] = append(fake.examples[key], args[3].(string))
	case qCatalogRequireList:
		result := &keelmodel.QueryResult{}
		for _, required := range fake.requires[key] {
			result.Rows = append(result.Rows, []any{required[0], required[1]})
		}
		return result, nil
	case qCatalogRequireInsert:
		fake.requires[key] = append(fake.requires[key], [2]string{args[2].(string), args[3].(string)})
	case qCatalogDependents:
		result := &keelmodel.QueryResult{}
		for versionKey, requires := range fake.requires {
			for _, required := range requires {
				if required[0] == args[0].(string) && required[1] == args[1].(string) {
					result.Rows = append(result.Rows, []any{fake.versions[versionKey][0], fake.versions[versionKey][1]})
				}
			}
		}
		return result, nil
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
		examples: map[string][]string{}, requires: map[string][][2]string{}, origins: map[string][]any{}, derived: map[string][][]any{},
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
		"origin": func(skill *domain.SkillDefinition) { skill.DerivedFrom = reference },
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

func TestCatalogOriginIsStoredAndFoundByLocatorOrDigest(t *testing.T) {
	_, catalog := newCatalog()
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("ab", 32)
	imported := catalogSkill()
	imported.Origin = &domain.SkillOrigin{Kind: domain.SkillOriginFile, Locator: " https://example.org/skills/audit/SKILL.md ",
		Revision: "v2.1.0", Digest: digest, License: "Apache-2.0"}
	if err := catalog.Register(ctx, imported); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := catalog.Register(ctx, imported); err != nil {
		t.Fatalf("the same origin again must be a no-op, got %v", err)
	}
	stored, err := catalog.Get(ctx, "audit", "1")
	want := domain.SkillOrigin{Kind: domain.SkillOriginFile, Locator: "https://example.org/skills/audit/SKILL.md",
		Revision: "v2.1.0", Digest: digest, License: "Apache-2.0"}
	if err != nil || stored.Origin == nil || *stored.Origin != want {
		t.Fatalf("stored origin = %+v, %v", stored.Origin, err)
	}
	relicensed := imported
	relicensed.Origin = &domain.SkillOrigin{Kind: want.Kind, Locator: want.Locator, Revision: want.Revision, Digest: want.Digest, License: "MIT"}
	if err := catalog.Register(ctx, relicensed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a changed origin: want ErrConflict, got %v", err)
	}
	local := catalogSkill()
	local.Version = "2"
	if err := catalog.Register(ctx, local); err != nil {
		t.Fatalf("Register without origin: %v", err)
	}
	if stored, err = catalog.Get(ctx, "audit", "2"); err != nil || stored.Origin != nil {
		t.Fatalf("a local version carries no origin, got %+v, %v", stored.Origin, err)
	}
	reference := []domain.SkillReference{{SkillID: "audit", Version: "1"}}
	for name, args := range map[string][2]string{"locator": {want.Locator, ""}, "digest": {"", digest}} {
		if found, err := catalog.Imported(ctx, args[0], args[1]); err != nil || !slices.Equal(found, reference) {
			t.Fatalf("Imported by %s = %+v, %v", name, found, err)
		}
	}
	if found, err := catalog.Imported(ctx, "https://example.org/other", "sha256:"+strings.Repeat("00", 32)); err != nil || len(found) != 0 {
		t.Fatalf("an unknown source = %+v, %v", found, err)
	}
	if _, err := catalog.Imported(ctx, " ", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no locator and no digest: want ErrValidation, got %v", err)
	}
}

func TestCatalogRefusesAnUnpinnedOrMalformedOrigin(t *testing.T) {
	fake, catalog := newCatalog()
	pinned := domain.SkillOrigin{Kind: domain.SkillOriginRepository, Locator: "https://example.org/skills.git", Revision: "3f9c1e2"}
	for name, mutate := range map[string]func(*domain.SkillOrigin){
		"unknown kind":          func(origin *domain.SkillOrigin) { origin.Kind = "X" },
		"blank locator":         func(origin *domain.SkillOrigin) { origin.Locator = " " },
		"no revision or digest": func(origin *domain.SkillOrigin) { origin.Revision = "" },
		"digest without prefix": func(origin *domain.SkillOrigin) { origin.Digest = strings.Repeat("ab", 32) },
		"short digest":          func(origin *domain.SkillOrigin) { origin.Digest = "sha256:abc" },
		"uppercase digest":      func(origin *domain.SkillOrigin) { origin.Digest = "sha256:" + strings.Repeat("AB", 32) },
		"digest with a non-hex": func(origin *domain.SkillOrigin) { origin.Digest = "sha256:" + strings.Repeat("g", 64) },
	} {
		origin := pinned
		mutate(&origin)
		skill := catalogSkill()
		skill.Origin = &origin
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

func TestCatalogRequirementsNameRegisteredCatalogVersions(t *testing.T) {
	_, catalog := newCatalog()
	ctx := context.Background()
	base := domain.SkillReference{SkillID: "base", Version: "1"}
	dependent := catalogSkill()
	dependent.Requires = []domain.SkillReference{base}
	if err := catalog.Register(ctx, dependent); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unregistered requirement: want ErrNotFound, got %v", err)
	}
	baseSkill := catalogSkill()
	baseSkill.SkillID = "base"
	if err := catalog.Register(ctx, baseSkill); err != nil {
		t.Fatalf("Register base: %v", err)
	}
	if err := catalog.Register(ctx, dependent); err != nil {
		t.Fatalf("Register dependent: %v", err)
	}
	if err := catalog.Register(ctx, catalogSkill()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("dropping the requirement: want ErrConflict, got %v", err)
	}
	stored, err := catalog.Get(ctx, "audit", "1")
	if err != nil || !slices.Equal(stored.Requires, []domain.SkillReference{base}) {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	dependents, err := catalog.Dependents(ctx, base)
	if err != nil || !slices.Equal(dependents, []domain.SkillReference{{SkillID: "audit", Version: "1"}}) {
		t.Fatalf("Dependents = %+v, %v", dependents, err)
	}
	if _, err = catalog.Dependents(ctx, domain.SkillReference{SkillID: "base"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a versionless reference: want ErrValidation, got %v", err)
	}
}

func TestCatalogEnsureProfileReservesTheSkillBeforeItsFirstVersion(t *testing.T) {
	fake, catalog := newCatalog()
	ctx := context.Background()
	if err := catalog.EnsureProfile(ctx, "audit", ""); err != nil {
		t.Fatalf("EnsureProfile: %v", err)
	}
	if err := catalog.EnsureProfile(ctx, "audit", "Audit"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another display name: want ErrConflict, got %v", err)
	}
	if err := catalog.EnsureProfile(ctx, "", "Audit"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank id: want ErrValidation, got %v", err)
	}
	if err := catalog.Register(ctx, catalogSkill()); err != nil || len(fake.profiles) != 1 {
		t.Fatalf("Register under the reserved profile: %v, profiles %v", err, fake.profiles)
	}
}
