package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

func setting(scopeID string, kind domain.ResourceKind, value string) domain.ScopedBinding {
	return domain.ScopedBinding{ScopeID: scopeID, ResourceKind: kind, ResourceID: string(kind), ResourceVersion: "v1", MergeMode: domain.MergeReplace, Value: []byte(value)}
}

func settingsService(bindings []domain.ScopedBinding) (*StudioService, *studioQueryFake) {
	service, query := lifecycleService(archiveRows("active", false))
	service.Scopes = studioScopes(bindings)
	query.rows[qStudioLockDraft] = [][]any{{int64(4), "writer"}}
	query.rows[qStudioLockAlias] = [][]any{{"writer-a", int64(2)}}
	query.rows[qStudioBumpAlias] = [][]any{{int64(3)}}
	return service, query
}

func toolChange(scopeID, value string) domain.AgentSettingChange {
	return domain.AgentSettingChange{
		AgentID: "writer-a", ScopeID: scopeID, ResourceKind: domain.ResourceTool, ResourceID: "tool",
		Value: json.RawMessage(value), ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2,
	}
}

func TestSaveSettingBindsACanonicalValueBehindTheDraftRevision(t *testing.T) {
	service, query := settingsService([]domain.ScopedBinding{setting("t:writer", domain.ResourceTool, `["crawl","search"]`)})
	settings, err := service.SaveSetting(context.Background(), humanActor(8, 9), toolChange("a:writer-a", `["search","search"]`))
	if err != nil {
		t.Fatalf("SaveSetting: %v", err)
	}
	if settings.AgentID != "writer-a" || !slices.Contains(query.queries, qStudioBumpDraft) || slices.Contains(query.queries, qStudioBumpAlias) {
		t.Fatalf("settings = %+v, queries = %v", settings, query.queries)
	}
	if args := query.args[qStudioInsertSetting]; args[2] != "tool" || args[7] != `["search"]` || args[9] != int64(9) {
		t.Fatalf("insert = %v", args)
	}
	if audit := query.args[qStudioAudit]; audit[2] != "SETTING" {
		t.Fatalf("audit = %v", audit)
	}
}

func TestSaveSettingRefusesABroadeningOrForeignWrite(t *testing.T) {
	inherited := []domain.ScopedBinding{setting("t:writer", domain.ResourceTool, `["search"]`)}
	service, query := settingsService(inherited)
	if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), toolChange("a:writer-a", `["search","delete"]`)); !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("broadening = %v, want authority exceeded", err)
	}
	for name, change := range map[string]domain.AgentSettingChange{
		"root scope": toolChange(RootPromptScopeID, `["search"]`),
		"prompt": {AgentID: "writer-a", ScopeID: "a:writer-a", ResourceKind: domain.ResourcePromptSection, ResourceID: "1/en",
			Value: json.RawMessage(`{"instruction":"x"}`)},
		"malformed": toolChange("a:writer-a", `{"tool":true}`),
		"ambiguous id": {AgentID: "writer-a", ScopeID: "a:writer-a", ResourceKind: domain.ResourceTool,
			ResourceID: "another-tool-set", Value: json.RawMessage(`["search"]`)},
		"second limits id": {AgentID: "writer-a", ScopeID: "a:writer-a", ResourceKind: domain.ResourceLimits,
			ResourceID: "a", Value: json.RawMessage(`{"max_tool_calls":0}`)},
	} {
		if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want validation", name, err)
		}
	}
	if slices.Contains(query.queries, qStudioInsertSetting) {
		t.Fatal("a refused setting must not be written")
	}
}

func TestSaveSettingAtTheTypeScopeUsesThePromptProfileRevision(t *testing.T) {
	service, query := settingsService(nil)
	change := toolChange("t:writer", "")
	change.Remove = true
	if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), change); err != nil {
		t.Fatalf("SaveSetting: %v", err)
	}
	if !slices.Contains(query.queries, qStudioBumpAlias) || !slices.Contains(query.queries, qStudioEndSetting) || slices.Contains(query.queries, qStudioInsertSetting) {
		t.Fatalf("queries = %v", query.queries)
	}
}

func TestSaveSettingChecksBothScopeRevisionsBeforeCompiling(t *testing.T) {
	service, query := settingsService(nil)
	query.rows[qStudioLockAlias] = [][]any{{"writer-a", int64(3)}}
	if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), toolChange("a:writer-a", `[]`)); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("error = %v, want revision conflict", err)
	}
	if slices.Contains(query.queries, qStudioBumpDraft) || slices.Contains(query.queries, qStudioInsertSetting) {
		t.Fatalf("stale setting wrote queries: %v", query.queries)
	}
}

func TestSettingsExplainsTheEffectiveValueOrReportsTheProblem(t *testing.T) {
	service, _ := settingsService([]domain.ScopedBinding{
		setting("t:writer", domain.ResourceTool, `["crawl","search"]`),
		setting("a:writer-a", domain.ResourceTool, `["search"]`),
	})
	settings, err := service.Settings(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || settings.Problem != "" || len(settings.Bindings) != 2 || len(settings.Effective) != 1 {
		t.Fatalf("settings = %+v, error = %v", settings, err)
	}
	if winner := settings.Effective[0]; string(winner.Value) != `["search"]` || winner.Winner.ScopeID != "a:writer-a" || len(winner.Superseded) != 1 {
		t.Fatalf("effective = %+v", winner)
	}
	broken, _ := settingsService([]domain.ScopedBinding{
		setting("t:writer", domain.ResourceTool, `["search"]`),
		setting("a:writer-a", domain.ResourceTool, `["delete"]`),
	})
	settings, err = broken.Settings(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || settings.Problem == "" || len(settings.Bindings) != 2 {
		t.Fatalf("broken settings = %+v, error = %v", settings, err)
	}
}

func TestSingletonSettingsRejectAmbiguousCompiledResources(t *testing.T) {
	resources := []domain.EffectiveResource{
		{ResourceKind: domain.ResourceBudget, ResourceID: "budget"},
		{ResourceKind: domain.ResourceBudget, ResourceID: "other"},
	}
	if err := validateSingletonSettings(resources); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
	limits := []domain.EffectiveResource{
		{ResourceKind: domain.ResourceLimits, ResourceID: "a"},
		{ResourceKind: domain.ResourceLimits, ResourceID: "limits"},
	}
	if err := validateSingletonSettings(limits); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a second limits resource escapes the inherited ceilings: %v", err)
	}
	resources[1].ResourceKind = domain.ResourcePolicy
	resources = append(resources, domain.EffectiveResource{ResourceKind: domain.ResourcePolicy, ResourceID: "second"})
	if err := validateSingletonSettings(resources); err != nil {
		t.Fatalf("multiple policy resources: %v", err)
	}
}

type guardFunc func(context.Context, domain.PublishCandidate) error

func (function guardFunc) CheckPublish(ctx context.Context, candidate domain.PublishCandidate) error {
	return function(ctx, candidate)
}

type staticSection struct{ name, value string }

func (section staticSection) Name() string { return section.name }
func (section staticSection) Section(context.Context, int64, domain.AgentDraft) (json.RawMessage, error) {
	return json.RawMessage(section.value), nil
}

func publishService() (*StudioService, *studioQueryFake) {
	query := &studioQueryFake{rows: map[string][][]any{
		qStudioGetDraft:    {{"writer", "Writer", "active", true, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
		qStudioLockDraft:   {{int64(4), "writer"}},
		qStudioLockAlias:   {{"writer-a", int64(2)}},
		qStudioNextVersion: {{int64(1)}},
	}, args: map[string][]any{}}
	return &StudioService{Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil), DB: studioDBFake{qs: query},
		Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{AgentID: "writer-a", AgentTypeID: "writer", LanguageCode: "en-US",
			Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))}}},
		Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{}, Catalog: studioModelCatalogFake{},
	}, query
}

func TestPublishGuardsRefuseBeforeTheReleaseIsWritten(t *testing.T) {
	service, query := publishService()
	var seen domain.PublishCandidate
	service.Guards = []contract.PublishGuard{guardFunc(func(_ context.Context, candidate domain.PublishCandidate) error {
		seen = candidate
		return domain.ErrForbidden
	})}
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	if _, err := service.Publish(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want the guard's refusal", err)
	}
	if seen.Definition.AgentID != "writer-a" || seen.Actor.Principal.ID != "9" || seen.Definition.DefinitionDigest == "" || slices.Contains(query.queries, qStudioInsertVersion) {
		t.Fatalf("candidate = %+v, queries = %v", seen, query.queries)
	}
}

func TestDefinitionSectionsAreFrozenAndDigested(t *testing.T) {
	plain, _ := publishService()
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	baseline, err := plain.Publish(context.Background(), humanActor(8, 9), request)
	if err != nil {
		t.Fatal(err)
	}
	service, query := publishService()
	service.Sections = []contract.DefinitionSection{staticSection{"governance", `{"purpose": "write",  "accountable":"u-1"}`}, staticSection{"triggers", ""}}
	release, err := service.Publish(context.Background(), humanActor(8, 9), request)
	if err != nil {
		t.Fatal(err)
	}
	encoded := query.args[qStudioInsertVersion][3].(string)
	if !strings.Contains(encoded, `"sections":{"governance":{"accountable":"u-1","purpose":"write"}}`) || release.DefinitionDigest == baseline.DefinitionDigest {
		t.Fatalf("definition = %s", encoded)
	}
	service.Sections = append(service.Sections, staticSection{"governance", `{}`})
	if _, err = service.Publish(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrValidation) && err == nil {
		t.Fatal("duplicate section names must be refused")
	}
}

func TestSaveSettingWorksForATypeWithoutAnAlias(t *testing.T) {
	service, query := settingsService(nil)
	delete(query.rows, qStudioLockAlias)
	change := toolChange("a:writer-a", `[]`)
	change.ExpectedPromptProfileRevision = 0
	if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), change); err != nil {
		t.Fatalf("agent scope without an alias: %v", err)
	}
	change.ScopeID = "t:writer"
	if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("type scope without an alias = %v, want conflict", err)
	}
}

func TestRestoreRefusesAVersionWithoutFrozenSettings(t *testing.T) {
	service, query := publishService()
	query.rows[qStudioGetVersion] = [][]any{{`{"agent_id":"writer-a","version":"1","definition_digest":"d"}`}}
	_, err := service.Restore(context.Background(), humanActor(8, 9), domain.AgentRestoreRequest{AgentID: "writer-a", Version: "1"})
	if !errors.Is(err, domain.ErrConflict) || !slices.Contains(query.queries, qStudioGetSettings) || query.commits != 0 {
		t.Fatalf("error = %v, commits = %d; a release that cannot serve a turn must not be restored", err, query.commits)
	}
}
