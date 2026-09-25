package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/toolgateway"
)

type skillTables struct {
	profiles map[string]string
	versions map[string][]any
	tools    map[string][]string
	examples map[string][]string
	bindings map[string]string
	toolIDs  map[string]bool
	// useSkill maps a use_skill version to its input schema.
	useSkill map[string]string
}

func (tables *skillTables) clone() *skillTables {
	copied := &skillTables{
		profiles: map[string]string{}, versions: map[string][]any{}, tools: map[string][]string{},
		examples: map[string][]string{}, bindings: map[string]string{}, toolIDs: tables.toolIDs, useSkill: tables.useSkill,
	}
	for k, v := range tables.profiles {
		copied.profiles[k] = v
	}
	for k, v := range tables.versions {
		copied.versions[k] = v
	}
	for k, v := range tables.tools {
		copied.tools[k] = slices.Clone(v)
	}
	for k, v := range tables.examples {
		copied.examples[k] = slices.Clone(v)
	}
	for k, v := range tables.bindings {
		copied.bindings[k] = v
	}
	return copied
}

// skillTableFake applies writes to a working copy that only Commit publishes.
type skillTableFake struct {
	committed *skillTables
	working   *skillTables
}

func (fake *skillTableFake) tables() *skillTables {
	if fake.working != nil {
		return fake.working
	}
	return fake.committed
}

func rows(values []string) *keelmodel.QueryResult {
	result := &keelmodel.QueryResult{}
	for _, value := range values {
		result.Rows = append(result.Rows, []any{value})
	}
	return result
}

func (fake *skillTableFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	tables := fake.tables()
	key := func(n int) string { return fmt.Sprint(args[:n]...) }
	versionRow := func(versionKey string) []any {
		row := tables.versions[versionKey]
		return []any{row[1], row[2], tables.profiles[fmt.Sprint(row[0], row[1])], row[3], row[4], row[5], row[6], row[7]}
	}
	switch name {
	case qSkillProfileEnsure:
		if _, ok := tables.profiles[key(2)]; !ok {
			tables.profiles[key(2)] = args[2].(string)
		}
	case qSkillProfileLock:
		if displayName, ok := tables.profiles[key(2)]; ok {
			return rows([]string{displayName}), nil
		}
	case qSkillUseToolSchema:
		if schema, ok := tables.useSkill[args[2].(string)]; ok {
			return rows([]string{schema}), nil
		}
	case qSkillToolProfileGet:
		if tables.toolIDs[args[1].(string)] {
			return rows([]string{args[1].(string)}), nil
		}
	case qSkillVersionGet:
		if _, ok := tables.versions[key(3)]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{versionRow(key(3))}}, nil
		}
	case qSkillVersionInsert:
		if _, ok := tables.versions[key(3)]; !ok {
			tables.versions[key(3)] = args
		}
	case qSkillToolList:
		return rows(tables.tools[key(3)]), nil
	case qSkillToolInsert:
		if !slices.Contains(tables.tools[key(3)], args[3].(string)) {
			tables.tools[key(3)] = append(tables.tools[key(3)], args[3].(string))
		}
	case qSkillExampleList:
		return rows(tables.examples[key(3)]), nil
	case qSkillExampleInsert:
		if len(tables.examples[key(3)]) < args[3].(int) {
			tables.examples[key(3)] = append(tables.examples[key(3)], args[4].(string))
		}
	case qSkillBindingGet:
		if version, ok := tables.bindings[key(4)]; ok {
			return rows([]string{version}), nil
		}
	case qSkillBindingInsert:
		if _, ok := tables.bindings[key(4)]; !ok {
			tables.bindings[key(4)] = args[4].(string)
		}
	case qSkillBoundTools, qSkillBoundExamples:
		result := &keelmodel.QueryResult{}
		for _, skillID := range []string{"audit", "rewrite"} {
			version, ok := tables.bindings[fmt.Sprint(args[0], args[1], args[2], skillID)]
			if !ok {
				continue
			}
			values := tables.tools
			if name == qSkillBoundExamples {
				values = tables.examples
			}
			for _, value := range values[fmt.Sprint(args[0], skillID, version)] {
				result.Rows = append(result.Rows, []any{skillID, value})
			}
		}
		return result, nil
	case qSkillBoundList:
		result := &keelmodel.QueryResult{}
		for _, skillID := range []string{"audit", "rewrite"} {
			if version, ok := tables.bindings[fmt.Sprint(args[0], args[1], args[2], skillID)]; ok {
				result.Rows = append(result.Rows, versionRow(fmt.Sprint(args[0], skillID, version)))
			}
		}
		return result, nil
	}
	return &keelmodel.QueryResult{}, nil
}

func (*skillTableFake) GenID() int64 { return 0 }

func (fake *skillTableFake) QueryService(string, map[string]string) keelport.QueryService {
	return fake
}

func (fake *skillTableFake) Commit(context.Context) error {
	fake.committed, fake.working = fake.working, nil
	return nil
}

func (fake *skillTableFake) Rollback(context.Context) error {
	fake.working = nil
	return nil
}

type skillTableDB struct {
	keelport.DatabaseRepository
	fake *skillTableFake
}

func (db skillTableDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.fake
}

func (db skillTableDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	db.fake.working = db.fake.committed.clone()
	return db.fake, nil
}

func newRegistry() (*skillTableFake, *TableSkillRegistry) {
	open, closed := UseSkillTool(), UseSkillTool("audit")
	tables := (&skillTables{
		toolIDs:  map[string]bool{"crawl": true, "propose_edit": true},
		useSkill: map[string]string{open.Version: string(open.InputSchema), closed.Version: string(closed.InputSchema)},
	}).clone()
	fake := &skillTableFake{committed: tables}
	return fake, &TableSkillRegistry{DB: skillTableDB{fake: fake}}
}

func auditSkill(version string) domain.SkillDefinition {
	return domain.SkillDefinition{
		SkillID: "audit", Version: version, Summary: "A page needs a technical audit.",
		Procedure: "1. Run `crawl`.\n2. File findings with `propose_edit`.",
		Tools:     []string{"propose_edit", "crawl", "crawl"},
		Examples:  []string{"Audit the pricing page.", "Why is /about not indexed?"},
		EvalSet:   &domain.GoldenSetReference{GoldenSetID: "rinova", SetVersion: 3},
	}
}

func TestRegisterIsIdempotentAndRejectsChangedContent(t *testing.T) {
	_, registry := newRegistry()
	ctx := context.Background()
	if err := registry.Register(ctx, 1, auditSkill("1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(ctx, 1, auditSkill("1")); err != nil {
		t.Fatalf("the same skill again must be a no-op, got %v", err)
	}
	for name, mutate := range map[string]func(*domain.SkillDefinition){
		"procedure": func(skill *domain.SkillDefinition) { skill.Procedure += "\n3. Report." },
		"tools":     func(skill *domain.SkillDefinition) { skill.Tools = []string{"crawl"} },
		"examples":  func(skill *domain.SkillDefinition) { skill.Examples = skill.Examples[:1] },
		"eval set":  func(skill *domain.SkillDefinition) { skill.EvalSet = nil },
	} {
		changed := auditSkill("1")
		mutate(&changed)
		if err := registry.Register(ctx, 1, changed); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("%s: want ErrConflict, got %v", name, err)
		}
	}
	stored, err := registry.Get(ctx, 1, "audit", "1")
	if err != nil || !slices.Equal(stored.Tools, []string{"crawl", "propose_edit"}) || len(stored.Examples) != 2 || stored.EvalSet.SetVersion != 3 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if _, err = registry.Get(ctx, 2, "audit", "1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's skill must be ErrNotFound, got %v", err)
	}
}

func TestRegisterRejectsInvalidSkillsBeforeWriting(t *testing.T) {
	fake, registry := newRegistry()
	for name, mutate := range map[string]func(*domain.SkillDefinition){
		"no procedure":        func(skill *domain.SkillDefinition) { skill.Procedure = " " },
		"multi-line summary":  func(skill *domain.SkillDefinition) { skill.Summary = "one\ntwo" },
		"lists use_skill":     func(skill *domain.SkillDefinition) { skill.Tools = []string{domain.UseSkillToolID} },
		"empty example":       func(skill *domain.SkillDefinition) { skill.Examples = []string{""} },
		"unversioned evalset": func(skill *domain.SkillDefinition) { skill.EvalSet.SetVersion = 0 },
		"bad input schema":    func(skill *domain.SkillDefinition) { skill.InputSchema = []byte(`{"type":"tuple"}`) },
	} {
		skill := auditSkill("1")
		mutate(&skill)
		if err := registry.Register(context.Background(), 1, skill); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	unknown := auditSkill("1")
	unknown.Tools = []string{"missing"}
	if err := registry.Register(context.Background(), 1, unknown); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unregistered tool must be ErrNotFound, got %v", err)
	}
	if len(fake.committed.profiles) != 0 || len(fake.committed.versions) != 0 {
		t.Fatalf("an invalid skill must write nothing, got %+v", fake.committed)
	}
}

func release(tools []string, skills ...domain.SkillReference) domain.AgentDefinition {
	definition := domain.AgentDefinition{AgentID: "wingmate", Version: "7", Skills: skills}
	for _, toolID := range tools {
		version := "1"
		if toolID == domain.UseSkillToolID {
			version = UseSkillTool().Version
		}
		definition.Tools = append(definition.Tools, domain.ToolReference{ToolID: toolID, Version: version})
	}
	return definition
}

func TestWriteReleaseHonoursAnEnumeratedUseSkill(t *testing.T) {
	fake, registry := newRegistry()
	ctx := context.Background()
	rewrite := auditSkill("1")
	rewrite.SkillID = "rewrite"
	for _, skill := range []domain.SkillDefinition{auditSkill("1"), rewrite} {
		if err := registry.Register(ctx, 1, skill); err != nil {
			t.Fatalf("Register %s: %v", skill.SkillID, err)
		}
	}
	closed := UseSkillTool("audit")
	if closed.Version == UseSkillTool().Version || closed.Version != UseSkillTool("audit", "audit").Version || !strings.Contains(string(closed.InputSchema), `"enum":["audit"]`) {
		t.Fatalf("enumerated contract = %+v", closed)
	}
	definition := release([]string{"crawl", "propose_edit"}, domain.SkillReference{SkillID: "audit", Version: "1"})
	definition.Tools = append(definition.Tools, domain.ToolReference{ToolID: domain.UseSkillToolID, Version: closed.Version})
	if err := registry.WriteRelease(ctx, fake, 1, definition); err != nil {
		t.Fatalf("a listed skill: %v", err)
	}
	definition.Skills = append(definition.Skills, domain.SkillReference{SkillID: "rewrite", Version: "1"})
	if err := registry.WriteRelease(ctx, fake, 1, definition); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unlisted skill: want ErrValidation, got %v", err)
	}
	definition.Tools[len(definition.Tools)-1].Version = "9"
	if err := registry.WriteRelease(ctx, fake, 1, definition); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unregistered use_skill: want ErrNotFound, got %v", err)
	}
}

func TestWriteReleaseRequiresUseSkillAndEveryToolTheSkillUses(t *testing.T) {
	fake, registry := newRegistry()
	ctx := context.Background()
	if err := registry.Register(ctx, 1, auditSkill("1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	audit := domain.SkillReference{SkillID: "audit", Version: "1"}
	if err := registry.WriteRelease(ctx, fake, 1, release([]string{"crawl", "propose_edit"}, audit)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("skills without use_skill: want ErrValidation, got %v", err)
	}
	if err := registry.WriteRelease(ctx, fake, 1, release([]string{domain.UseSkillToolID, "crawl"}, audit)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unbound skill tool: want ErrValidation, got %v", err)
	}
	full := []string{domain.UseSkillToolID, "crawl", "propose_edit"}
	if err := registry.WriteRelease(ctx, fake, 1, release(full, domain.SkillReference{SkillID: "audit", Version: "9"})); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unregistered version: want ErrNotFound, got %v", err)
	}
	if err := registry.WriteRelease(ctx, fake, 1, release(full, audit)); err != nil {
		t.Fatalf("WriteRelease: %v", err)
	}
	bound, err := registry.List(ctx, 1, "wingmate", "7")
	if err != nil || len(bound) != 1 || bound[0].SkillID != "audit" || len(bound[0].Tools) != 2 {
		t.Fatalf("List = %+v, %v", bound, err)
	}
}

func TestUseSkillServesOnlyTheCallersReleaseSkills(t *testing.T) {
	fake, registry := newRegistry()
	ctx := context.Background()
	if err := registry.Register(ctx, 1, auditSkill("1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.WriteRelease(ctx, fake, 1, release([]string{domain.UseSkillToolID, "crawl", "propose_edit"}, domain.SkillReference{SkillID: "audit", Version: "1"})); err != nil {
		t.Fatalf("WriteRelease: %v", err)
	}
	transport := &toolgateway.InProcessTransport{}
	if err := (&UseSkill{Skills: registry}).Register(transport); err != nil {
		t.Fatalf("Register handler: %v", err)
	}
	call := domain.ToolCall{Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "wingmate", TenantID: 1, Release: "7"}, Arguments: []byte(`{"skill":"audit"}`)}
	result, err := transport.Invoke(ctx, call, UseSkillTool(), nil, 0)
	if err != nil {
		t.Fatalf("use_skill: %v", err)
	}
	var loaded struct {
		Skill     string   `json:"skill"`
		Procedure string   `json:"procedure"`
		Tools     []string `json:"tools"`
	}
	if err = json.Unmarshal(result.Output, &loaded); err != nil || loaded.Skill != "audit" || !strings.Contains(loaded.Procedure, "`crawl`") || len(loaded.Tools) != 2 {
		t.Fatalf("output = %s, %v", result.Output, err)
	}
	call.Principal.Release = "6"
	if _, err = transport.Invoke(ctx, call, UseSkillTool(), nil, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a release that binds no such skill: want ErrNotFound, got %v", err)
	}
	if index := Index([]domain.SkillDefinition{auditSkill("1")}); !strings.Contains(index, "- audit: A page needs a technical audit.\n") {
		t.Fatalf("index = %q", index)
	}
}
