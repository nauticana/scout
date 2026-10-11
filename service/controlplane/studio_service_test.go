package controlplane

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/scope"
)

type studioQueryFake struct {
	rows    map[string][][]any
	queries []string
	args    map[string][]any
	commits int
}

func (f *studioQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	f.queries = append(f.queries, name)
	f.args[name] = args
	return &keelmodel.QueryResult{Rows: f.rows[name]}, nil
}

func (*studioQueryFake) GenID() int64                   { return 0 }
func (f *studioQueryFake) Commit(context.Context) error { f.commits++; return nil }
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
		qStudioGetDraft:        {{"writer", "Writer", "active", true, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
		qStudioSetDraftEnabled: {{int64(5)}},
	}, args: map[string][]any{}}
	service := &StudioService{Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil),
		DB: studioDBFake{qs: query}, Sources: unusedSources{}, Assembler: unusedAssembler{}, Compiler: unusedCompiler{},
	}

	state, err := service.SetEnabled(context.Background(), humanActor(8, 9), domain.AgentSetEnabledRequest{
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

// An agent-only save must not look like a type-level edit, or it would bump the shared profile revision.
func TestDesiredPromptBindingsAreReadPerScope(t *testing.T) {
	draft := domain.AgentDraft{Languages: []domain.AgentLanguageDraft{{LanguageCode: "en-US", Sections: []domain.AgentPromptSection{{
		PromptSectionID: 1, Layers: []domain.PromptLayer{baseLayer("base", ""), agentLayer("", "agent", "")},
	}}}}}
	if !promptBindingsEqual(desiredPromptBindings(draft, "t:writer"), map[promptBindingKey]promptBinding{}) {
		t.Fatal("an agent layer must not be treated as a tenant default")
	}
	agent := desiredPromptBindings(draft, "a:writer-a")
	if got := agent[promptBindingKey{1, "en-US"}]; got.mode != domain.MergeAppend || got.value.Instruction != "agent" {
		t.Fatalf("a Studio layer appends unless it says otherwise: %+v", agent)
	}
}

// Bindings are temporal: an edit ends the open row and binds a new one; an unchanged one is left alone.
func TestSyncPromptBindingsEndsWhatChangedAndBindsTheNewValue(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{}, args: map[string][]any{}}
	kept := promptBinding{mode: domain.MergeAppend, value: promptBindingValue{Instruction: "same"}}
	persisted := map[promptBindingKey]promptBinding{
		{1, "en-US"}: kept,
		{2, "en-US"}: {mode: domain.MergeAppend, value: promptBindingValue{Instruction: "old"}},
		{3, "en-US"}: {mode: domain.MergeAppend, value: promptBindingValue{Instruction: "removed"}},
	}
	desired := map[promptBindingKey]promptBinding{
		{1, "en-US"}: kept,
		{2, "en-US"}: {mode: domain.MergeReplace, sealed: true, value: promptBindingValue{Instruction: "new"}},
	}
	if err := syncPromptBindings(context.Background(), query, humanActor(8, 9), "a:writer-a", desired, persisted); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, name := range query.queries {
		counts[name]++
	}
	if counts[qStudioEndBinding] != 2 || counts[qStudioDropBinding] != 2 || counts[qStudioInsertBinding] != 1 {
		t.Fatalf("queries = %v", query.queries)
	}
	bound := query.args[qStudioInsertBinding]
	if bound[1] != "a:writer-a" || bound[2] != "2/en-US" || bound[4] != "replace" || bound[5] != true || bound[6] != `{"instruction":"new"}` || bound[8] != int64(9) {
		t.Fatalf("insert args = %v", bound)
	}
}

func TestStudioRefusesALayerUnderASealedOne(t *testing.T) {
	company := domain.PromptLayer{ScopeID: "company", ScopeKind: "company", MergeMode: domain.MergeAppend, Sealed: true, Instruction: "never promise a refund"}
	service := &StudioService{Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil), Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{
		LanguageCode: "en-US", Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""), company)},
	}}}
	scopes := domain.PromptScopes{AgentScopeID: "a:writer-a", TypeScopeID: "t:writer"}
	draft := func(layers ...domain.PromptLayer) domain.AgentDraft {
		return domain.AgentDraft{AgentID: "writer-a", Languages: []domain.AgentLanguageDraft{{LanguageCode: "en-US", Sections: []domain.AgentPromptSection{{PromptSectionID: 1, Layers: layers}}}}}
	}
	if err := service.checkSealedLayers(context.Background(), 8, draft(agentLayer(domain.MergeAppend, "but do", "")), scopes); !errors.Is(err, domain.ErrSealed) {
		t.Fatalf("a scope Studio does not edit seals the section, got %v", err)
	}
	// The client cannot unseal it by sending the layer back unsealed: the stored layer is the authority.
	company.Sealed = false
	if err := service.checkSealedLayers(context.Background(), 8, draft(company, agentLayer(domain.MergeAppend, "but do", "")), scopes); !errors.Is(err, domain.ErrSealed) {
		t.Fatalf("want ErrSealed, got %v", err)
	}
	if err := service.checkSealedLayers(context.Background(), 8, draft(company), scopes); err != nil {
		t.Fatalf("a save that writes no layer of the section is fine: %v", err)
	}

	open := &StudioService{Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil), Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{
		LanguageCode: "en-US", Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""))},
	}}}
	sealedType := typeLayer(domain.MergeAppend, "tenant rule", "")
	sealedType.Sealed = true
	if err := open.checkSealedLayers(context.Background(), 8, draft(sealedType, agentLayer(domain.MergeAppend, "agent", "")), scopes); !errors.Is(err, domain.ErrSealed) {
		t.Fatalf("a sealed tenant default refuses the agent layer, got %v", err)
	}
	if err := open.checkSealedLayers(context.Background(), 8, draft(sealedType), scopes); err != nil {
		t.Fatalf("sealing the tenant default itself is allowed: %v", err)
	}
}

func TestStudioPublishFreezesAndDeploysDefaultAgent(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{
		qStudioGetDraft:    {{"writer", "Writer", "active", true, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
		qStudioLockDraft:   {{int64(4), "writer"}},
		qStudioLockAlias:   {{"writer-a", int64(2)}},
		qStudioNextVersion: {{int64(1)}},
	}, args: map[string][]any{}}
	sources := studioSourcesFake{resolved: domain.ResolvedPrompts{
		AgentID: "writer-a", AgentTypeID: "writer", BaselineKey: "global", LanguageCode: "en-US",
		Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))},
	}}
	service := &StudioService{Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil),
		DB: studioDBFake{qs: query}, Sources: sources,
		Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{}, Catalog: studioModelCatalogFake{},
	}

	release, err := service.Publish(context.Background(), humanActor(8, 9), domain.AgentPublishRequest{
		AgentID: "writer-a", ChangeSummary: "initial", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "1" || !release.Active || len(release.Languages) != 1 {
		t.Fatalf("unexpected release: %+v", release)
	}
	for _, name := range []string{qStudioInsertVersion, qStudioGrantModel, qStudioDeployVersion, qStudioAudit} {
		if _, ok := query.args[name]; !ok {
			t.Fatalf("publish did not execute %s: %v", name, query.queries)
		}
	}
	encoded := query.args[qStudioInsertVersion][3].(string)
	if !strings.Contains(encoded, `"agent_id":"writer-a"`) || strings.Contains(encoded, `"AgentID"`) {
		t.Fatalf("definition JSON is not canonical snake case: %s", encoded)
	}
}

type releaseWriterRecorder struct {
	definitions []domain.AgentDefinition
	err         error
}

func (recorder *releaseWriterRecorder) WriteRelease(_ context.Context, _ keelport.TxQueryService, _ int64, definition domain.AgentDefinition) error {
	recorder.definitions = append(recorder.definitions, definition)
	return recorder.err
}

// A release that names tools must bind them in the publish transaction; without
// a writer it would be published with no tools and every call would be denied.
func TestStudioPublishFreezesToolsAndRunsReleaseWritersInTheTransaction(t *testing.T) {
	newService := func(writers ...ReleaseWriter) (*StudioService, *studioQueryFake) {
		query := &studioQueryFake{rows: map[string][][]any{
			qStudioGetDraft:    {{"writer", "Writer", "active", true, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
			qStudioLockDraft:   {{int64(4), "writer"}},
			qStudioLockAlias:   {{"writer-a", int64(2)}},
			qStudioNextVersion: {{int64(1)}},
		}, args: map[string][]any{}}
		sources := studioSourcesFake{resolved: domain.ResolvedPrompts{
			AgentID: "writer-a", AgentTypeID: "writer", BaselineKey: "global", LanguageCode: "en-US",
			Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))},
		}}
		return &StudioService{Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil),
			DB: studioDBFake{qs: query}, Sources: sources, ReleaseWriters: writers,
			Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{}, Catalog: studioModelCatalogFake{},
		}, query
	}
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	typeTools := fake.KindCatalog{GetFunc: func(_ context.Context, kind string) (domain.AgentTypeDescriptor, error) {
		return domain.AgentTypeDescriptor{AgentTypeID: kind, ToolLoop: &domain.ToolLoopConfig{MaxIterations: 4},
			Tools: []domain.ToolReference{{ToolID: "search", Version: "1"}, {ToolID: "crawl", Version: "2"}}}, nil
	}}
	actor := humanActor(8, 9)

	recorder := &releaseWriterRecorder{}
	service, query := newService(recorder)
	service.Kinds = typeTools
	service.Scopes = studioScopes([]domain.ScopedBinding{{ScopeID: "a:writer-a", ResourceKind: domain.ResourceTool,
		ResourceID: "tool", ResourceVersion: "v1", MergeMode: domain.MergeReplace, Value: []byte(`["search"]`)}})
	plain, _ := newService()
	baseline, err := plain.Publish(context.Background(), actor, request)
	if err != nil {
		t.Fatal(err)
	}
	release, err := service.Publish(context.Background(), actor, request)
	if err != nil {
		t.Fatal(err)
	}
	// The type declares search and crawl; the agent's tool setting narrows them to search.
	if len(recorder.definitions) != 1 || recorder.definitions[0].Version != "1" || len(recorder.definitions[0].Tools) != 1 {
		t.Fatalf("writer saw %+v", recorder.definitions)
	}
	if release.DefinitionDigest == baseline.DefinitionDigest {
		t.Fatal("tools, the tool loop, and settings are part of the release and must change its digest")
	}
	encoded := query.args[qStudioInsertVersion][3].(string)
	if !strings.Contains(encoded, `"tools":[{"tool_id":"search","version":"1"}]`) || !strings.Contains(encoded, `"tool_loop":{"max_iterations":4}`) ||
		!strings.Contains(encoded, `"settings_digest":"`) {
		t.Fatalf("definition JSON lacks the frozen tools and settings: %s", encoded)
	}
	if frozen := query.args[qEffectivePutName()]; frozen == nil || frozen[2] != "1" {
		t.Fatalf("effective release was not frozen for version 1: %v", frozen)
	}

	unwired, _ := newService()
	unwired.Kinds = typeTools
	if _, err := unwired.Publish(context.Background(), actor, request); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("want ErrNotReady without a release writer, got %v", err)
	}
	failing, failed := newService(&releaseWriterRecorder{err: domain.ErrNotFound})
	failing.Kinds = typeTools
	if _, err := failing.Publish(context.Background(), actor, request); !errors.Is(err, domain.ErrNotFound) || failed.commits != 0 {
		t.Fatalf("a failed binding must fail the publish before commit: %v, commits %d", err, failed.commits)
	}
}

// A model outside the agent's model setting cannot be published.
func TestStudioPublishRefusesAModelOutsideTheSetting(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{
		qStudioGetDraft:  {{"writer", "Writer", "active", true, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
		qStudioLockDraft: {{int64(4), "writer"}},
	}, args: map[string][]any{}}
	service := &StudioService{Principals: studioPrincipals, Roles: studioRoles, DB: studioDBFake{qs: query},
		Scopes: studioScopes([]domain.ScopedBinding{{ScopeID: "t:writer", ResourceKind: domain.ResourceModel,
			ResourceID: "model", ResourceVersion: "v1", MergeMode: domain.MergeReplace, Value: []byte(`["provider-b/model-b"]`)}}),
		Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{AgentID: "writer-a", AgentTypeID: "writer", LanguageCode: "en-US",
			Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))}}},
		Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{}, Catalog: studioModelCatalogFake{},
	}
	_, err := service.Publish(context.Background(), humanActor(8, 9), domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2})
	if !errors.Is(err, domain.ErrValidation) || query.commits != 0 {
		t.Fatalf("error = %v, commits = %d", err, query.commits)
	}
}

func qEffectivePutName() string {
	for name := range scope.ReleaseWriteQueries {
		return name
	}
	return ""
}

func humanActor(tenantID, userID int64) domain.StudioActor {
	return domain.StudioActor{TenantID: tenantID, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: strconv.FormatInt(userID, 10)}}
}

// studioPrincipals and studioRoles admit every actor with a wildcard grant.
var (
	studioPrincipals = fake.PrincipalResolverFunc(func(_ context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
		return domain.Principal{Kind: ref.Kind, ID: ref.ID, TenantID: tenantID}, nil
	})
	studioRoles = fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{Allowed: true, LowLimit: "*", BypassScope: true}, nil
	})
)

// studioScopes is the base layout's chain for writer-a with the given bindings in force.
func studioScopes(bindings []domain.ScopedBinding) *fake.ScopeRepository {
	return &fake.ScopeRepository{
		ChainFunc: func(_ context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error) {
			return domain.ScopeChain{
				{TenantID: tenantID, ScopeID: RootPromptScopeID, ScopeKind: "tenant"},
				{TenantID: tenantID, ScopeID: "t:writer", ParentScopeID: RootPromptScopeID, ScopeKind: "agent_type"},
				{TenantID: tenantID, ScopeID: scopeID, ParentScopeID: "t:writer", ScopeKind: "agent"},
			}, nil
		},
		BindingsFunc: func(context.Context, int64, []string, time.Time) ([]domain.ScopedBinding, error) {
			return bindings, nil
		},
	}
}
