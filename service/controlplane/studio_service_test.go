package controlplane

import (
	"context"
	"strings"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type studioQueryFake struct {
	rows    map[string][][]any
	queries []string
	args    map[string][]any
}

func (f *studioQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	f.queries = append(f.queries, name)
	f.args[name] = args
	return &keelmodel.QueryResult{Rows: f.rows[name]}, nil
}

func (*studioQueryFake) GenID() int64                   { return 0 }
func (*studioQueryFake) Commit(context.Context) error   { return nil }
func (*studioQueryFake) Rollback(context.Context) error { return nil }

type studioDBFake struct {
	keelport.DatabaseRepository
	qs *studioQueryFake
}

func (f studioDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return f.qs
}
func (f studioDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return f.qs, nil
}

type unusedSources struct {
	contract.PromptSourceRepository
}
type unusedAssembler struct{ contract.PromptDraftAssembler }
type unusedCompiler struct{ contract.PromptCompiler }

type studioSourcesFake struct {
	resolved domain.ResolvedPrompts
}

type studioModelCatalogFake struct{}

func (studioModelCatalogFake) List(context.Context, int64) ([]domain.StudioModel, error) {
	return nil, nil
}

func (studioModelCatalogFake) Validate(context.Context, int64, domain.AgentModelSelection) ([]domain.AgentFieldError, error) {
	return nil, nil
}

func (f studioSourcesFake) Resolve(context.Context, int64, string, string) (domain.ResolvedPrompts, error) {
	return f.resolved, nil
}

func (f studioSourcesFake) Languages(context.Context, int64, string) ([]string, error) {
	return []string{f.resolved.LanguageCode}, nil
}

func TestStudioSetEnabledIsTargeted(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{
		qStudioGetDraft:        {{"writer", "Writer", true, true, false, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
		qStudioSetDraftEnabled: {{int64(5)}},
	}, args: map[string][]any{}}
	service := &StudioService{
		DB: studioDBFake{qs: query}, Sources: unusedSources{}, Assembler: unusedAssembler{}, Compiler: unusedCompiler{},
	}

	state, err := service.SetEnabled(context.Background(), domain.StudioActor{TenantID: 8, ActorID: 9}, domain.AgentSetEnabledRequest{
		AgentID: "writer-a", Enabled: false, ExpectedDraftRevision: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Enabled || state.DraftRevision != 5 {
		t.Fatalf("unexpected enabled state: %+v", state)
	}
	want := []string{qStudioGetDraft, qStudioSetDraftEnabled, qStudioSetProfileActive, qStudioAudit}
	for i := range want {
		if query.queries[i] != want[i] {
			t.Fatalf("queries = %v, want %v", query.queries, want)
		}
	}
	if query.args[qStudioSetProfileActive][0] != string(domain.AgentStateSuspended) {
		t.Fatalf("profile update did not receive the kill-switch state: %v", query.args[qStudioSetProfileActive])
	}
}

func TestPromptDefaultsEqualIgnoresDraftWithoutDefaults(t *testing.T) {
	draft := domain.AgentDraft{Languages: []domain.AgentLanguageDraft{{LanguageCode: "en-US", Sections: []domain.AgentPromptSection{{
		PromptSectionID: 1, AgentOverride: &domain.PromptOverride{PromptValue: domain.PromptValue{Instruction: "agent"}},
	}}}}}
	if !promptDefaultsEqual(desiredPromptDefaults(draft), map[promptDefaultKey]domain.PromptValue{}) {
		t.Fatal("agent overrides must not be treated as tenant defaults")
	}
}

func TestStudioPublishFreezesAndDeploysDefaultAgent(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{
		qStudioGetDraft:    {{"writer", "Writer", true, true, false, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
		qStudioLockDraft:   {{int64(4), "writer"}},
		qStudioLockAlias:   {{"writer-a", int64(2)}},
		qStudioNextVersion: {{int64(1)}},
	}, args: map[string][]any{}}
	sources := studioSourcesFake{resolved: domain.ResolvedPrompts{
		AgentID: "writer-a", AgentTypeID: "writer", BaselineKey: "global", LanguageCode: "en-US",
		Rows: []domain.PromptSourceRow{{
			PromptSectionID: 1, Caption: "task", DisplayOrder: 1,
			SourceLevel: domain.PromptSourceBaseline, SourceKey: "global", Instruction: "write",
		}},
	}}
	service := &StudioService{
		DB: studioDBFake{qs: query}, Sources: sources,
		Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{}, Catalog: studioModelCatalogFake{},
	}

	release, err := service.Publish(context.Background(), domain.StudioActor{TenantID: 8, ActorID: 9}, domain.AgentPublishRequest{
		AgentID: "writer-a", ChangeSummary: "initial", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "1" || !release.Active || len(release.Languages) != 1 {
		t.Fatalf("unexpected release: %+v", release)
	}
	for _, name := range []string{qStudioInsertVersion, qStudioDeployVersion, qStudioAudit} {
		if _, ok := query.args[name]; !ok {
			t.Fatalf("publish did not execute %s: %v", name, query.queries)
		}
	}
	encoded := query.args[qStudioInsertVersion][3].(string)
	if !strings.Contains(encoded, `"agent_id":"writer-a"`) || strings.Contains(encoded, `"AgentID"`) {
		t.Fatalf("definition JSON is not canonical snake case: %s", encoded)
	}
}
