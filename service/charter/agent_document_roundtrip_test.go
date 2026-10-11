package charter

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/controlplane"
	"github.com/nauticana/scout/service/scope"
)

// studioRows answers Studio's named queries by name and records every call.
type studioRows struct {
	rows  map[string][][]any
	calls map[string][][]any
}

func (f *studioRows) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	f.calls[name] = append(f.calls[name], args)
	return &keelmodel.QueryResult{Rows: f.rows[name]}, nil
}

func (*studioRows) GenID() int64                   { return 0 }
func (*studioRows) Commit(context.Context) error   { return nil }
func (*studioRows) Rollback(context.Context) error { return nil }

type studioRowsDB struct {
	keelport.DatabaseRepository
	rows *studioRows
}

func (d studioRowsDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return d.rows
}
func (d studioRowsDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return d.rows, nil
}

// An agent exported through the real codec and the HTTP re-encoding imports into another tenant:
// its release and settings digests survive canonicalization, and its references are remapped.
func TestAgentExportImportsIntoAnotherTenantThroughTheCharterDocument(t *testing.T) {
	compiler := &controlplane.PromptCompiler{}
	compiled, err := compiler.Compile("en-US", []domain.PromptSectionSource{{PromptSectionID: 1, Caption: "task", Layers: []domain.PromptLayer{
		{ScopeID: "global", ScopeKind: domain.ScopeKindPlatform, MergeMode: domain.MergeReplace, Instruction: "write <short> answers"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	resources := []domain.EffectiveResource{
		{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(`{"tokens":100,"cost_minor_units":0,"period":"turn"}`)},
		{ResourceKind: domain.ResourceKnowledge, ResourceID: "knowledge", Value: []byte(`["kb-1"]`)},
	}
	var governance any
	_ = json.Unmarshal([]byte(`{"purpose":"write <well>","triggers":["brief"],"weight":1.5}`), &governance)
	section, _ := json.Marshal(governance)
	definition := domain.AgentDefinition{
		AgentID: "writer-a", AgentTypeID: "writer", Version: "3", Enabled: true,
		Models:    domain.AgentModelSelection{Text: &domain.ModelReference{ProviderID: "provider-a", ModelID: "model-a"}},
		Languages: []domain.CompiledPrompt{compiled}, Extension: json.RawMessage(`{"tone": "<formal>", "ratio": 1.25}`),
		Sections: map[string]json.RawMessage{"triggers": section}, SettingsDigest: scope.SettingsDigest(resources),
		PublishedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	if definition.DefinitionDigest, err = compiler.DefinitionDigest(definition); err != nil {
		t.Fatal(err)
	}
	stored, _ := json.Marshal(definition)
	payload, _ := json.Marshal(resources)
	rows := &studioRows{calls: map[string][][]any{}, rows: map[string][][]any{
		"scout_studio_get_draft":            {{"writer", "Writer", "active", true, "provider-a", "model-a", nil, nil, nil, nil, `{"tone": "<formal>"}`, int64(4), int64(2), false}},
		"scout_studio_active_definition":    {{"3", string(stored)}},
		"scout_studio_get_settings":         {{"a:writer-a", string(payload), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), nil}},
		"scout_studio_seed_prompt_sections": {{int64(1), "task"}},
		"scout_studio_seed_knowledge_owned": {{"kb-9"}},
		"scout_studio_lock_type":            {{"writer"}},
		"scout_studio_type_census":          {{int64(0), int64(0)}},
		"scout_studio_create_record":        {{"writer-00000000000000000000"}},
		"scout_studio_list_agents":          {{"writer-00000000000000000000", "writer", "Writer", "draft", false, int64(1), int64(0), false, nil, nil, nil}},
	}}
	studio := &controlplane.StudioService{
		DB: studioRowsDB{rows: rows}, MaxAgentsPerType: 5, Entropy: bytes.NewReader(make([]byte, 16)),
		Principals: fake.PrincipalResolverFunc(func(_ context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
			return domain.Principal{Kind: ref.Kind, ID: ref.ID, TenantID: tenantID}, nil
		}),
		Roles: fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
			return domain.AuthorizationGrant{Allowed: true, LowLimit: "*", BypassScope: true}, nil
		}),
		Scopes: &fake.ScopeRepository{
			ChainFunc: func(_ context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error) {
				return domain.ScopeChain{{TenantID: tenantID, ScopeID: "tenant"}, {TenantID: tenantID, ScopeID: "t:writer", ParentScopeID: "tenant"}, {TenantID: tenantID, ScopeID: scopeID, ParentScopeID: "t:writer"}}, nil
			},
			BindingsFunc: func(_ context.Context, tenantID int64, _ []string, _ time.Time) ([]domain.ScopedBinding, error) {
				if tenantID != 8 {
					return nil, nil
				}
				return []domain.ScopedBinding{
					{ScopeID: "a:writer-a", ResourceKind: domain.ResourcePromptSection, ResourceID: "1/en-US", MergeMode: domain.MergeAppend, Value: []byte(`{"instruction":"be <brief>"}`)},
					{ScopeID: "a:writer-a", ResourceKind: domain.ResourceKnowledge, ResourceID: "knowledge", MergeMode: domain.MergeReplace, Value: []byte(`["kb-1"]`)},
				}, nil
			},
		},
		Sources:   unusedSources{},
		Assembler: &controlplane.PromptDraftAssembler{Compiler: compiler}, Compiler: compiler,
		Catalog: fake.ModelCatalog{
			ListFunc: func(context.Context, int64) ([]domain.StudioModel, error) {
				return []domain.StudioModel{{Reference: domain.ModelReference{ProviderID: "provider-a", ModelID: "model-a"}, Active: true}}, nil
			},
			ValidateFunc: func(context.Context, int64, domain.AgentModelSelection) ([]domain.AgentFieldError, error) {
				return nil, nil
			},
		},
	}
	portability := &controlplane.StudioPortability{Studio: studio, Documents: documents("")}
	person := func(tenantID int64) domain.StudioActor {
		return domain.StudioActor{TenantID: tenantID, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "21"}}
	}

	document, err := portability.Export(context.Background(), person(8), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	transported, err := json.Marshal(struct {
		Document json.RawMessage `json:"document"`
	}{document})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Document json.RawMessage `json:"document"`
	}
	if err = json.Unmarshal(transported, &request); err != nil || bytes.Equal(request.Document, document) {
		t.Fatalf("the transport must re-encode the document for this test to mean anything: %v", err)
	}
	summary, err := portability.Import(context.Background(), person(9), domain.AgentImportRequest{
		Document: request.Document, IdempotencyKey: "import-1",
		References: map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"kb-1": "kb-9"}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.AgentID != "writer-00000000000000000000" {
		t.Fatalf("summary = %+v", summary)
	}
	values := map[string]string{}
	for _, args := range rows.calls["scout_studio_insert_setting"] {
		if args[0] != int64(9) {
			t.Fatalf("an import writes only the importing tenant: %v", args)
		}
		values[args[2].(string)] = args[7].(string)
	}
	var prompt struct{ Instruction string }
	_ = json.Unmarshal([]byte(values["prompt_section"]), &prompt)
	if values["knowledge"] != `["kb-9"]` || prompt.Instruction != "be <brief>" {
		t.Fatalf("values = %v", values)
	}
	origin := rows.calls["scout_studio_seed_put_origin"][0]
	if origin[2] != "AgentDefinition" || origin[3] != "writer-a-v3" || origin[4] != "acme.agents" || !strings.HasPrefix(origin[7].(string), "sha256:") {
		t.Fatalf("origin = %v", origin)
	}
}

type unusedSources struct{ controlplaneSources }

type controlplaneSources interface {
	Resolve(ctx context.Context, tenantID int64, agentID, languageCode string) (domain.ResolvedPrompts, error)
	Languages(ctx context.Context, tenantID int64, agentID string) ([]string, error)
}
