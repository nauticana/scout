package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/guardrail"
	"github.com/nauticana/scout/service/scope"
)

// seedDB records every query with its arguments and answers rows per query name; byArgs answers
// rows for one query and argument list first.
type seedDB struct {
	rows      map[string][][]any
	byArgs    map[string]func(args []any) ([][]any, bool)
	fail      map[string]error
	calls     []seedCall
	commits   int
	rollbacks int
}

type seedCall struct {
	name string
	args []any
}

func newSeedDB(rows map[string][][]any) *seedDB {
	return &seedDB{rows: rows, byArgs: map[string]func([]any) ([][]any, bool){}, fail: map[string]error{}}
}

func (f *seedDB) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	f.calls = append(f.calls, seedCall{name, args})
	if err := f.fail[name]; err != nil {
		return nil, err
	}
	if answer := f.byArgs[name]; answer != nil {
		if rows, ok := answer(args); ok {
			return &keelmodel.QueryResult{Rows: rows}, nil
		}
	}
	return &keelmodel.QueryResult{Rows: f.rows[name]}, nil
}

func (*seedDB) GenID() int64                     { return 0 }
func (f *seedDB) Commit(context.Context) error   { f.commits++; return nil }
func (f *seedDB) Rollback(context.Context) error { f.rollbacks++; return nil }
func (f *seedDB) repository() seedRepository     { return seedRepository{db: f} }
func (f *seedDB) count(name string) int          { return len(f.args(name)) }
func (f *seedDB) last(name string) []any         { all := f.args(name); return all[len(all)-1] }
func (f *seedDB) names() []string                { return f.namesFrom(0) }
func (f *seedDB) namesFrom(start int) (out []string) {
	for _, call := range f.calls[start:] {
		out = append(out, call.name)
	}
	return out
}

func (f *seedDB) args(name string) [][]any {
	var out [][]any
	for _, call := range f.calls {
		if call.name == name {
			out = append(out, call.args)
		}
	}
	return out
}

type seedRepository struct {
	keelport.DatabaseRepository
	db *seedDB
}

func (r seedRepository) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return r.db
}
func (r seedRepository) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return r.db, nil
}

// portableCodec stands in for the Charter codec: it keeps what Encode got and decodes what a test sets.
type portableCodec struct {
	encoded *domain.PortableAgent
	decoded domain.PortableAgent
	err     error
}

func (c *portableCodec) Encode(_ context.Context, _ int64, agent domain.PortableAgent) (json.RawMessage, string, error) {
	c.encoded = &agent
	raw, err := json.Marshal(agent)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (c *portableCodec) Decode(context.Context, json.RawMessage) (domain.PortableAgent, error) {
	return c.decoded, c.err
}

var (
	typeDigest  = strings.Repeat("e", 64)
	nine        = int64(9)
	searchTool  = domain.ToolReference{ToolID: "search", Version: "1"}
	crawlTool   = domain.ToolReference{ToolID: "crawl", Version: "1"}
	writerKinds = fake.KindCatalog{
		GetFunc: func(_ context.Context, kind string) (domain.AgentTypeDescriptor, error) {
			if kind != "writer" {
				return domain.AgentTypeDescriptor{}, domain.ErrNotFound
			}
			return domain.AgentTypeDescriptor{AgentTypeID: kind, Tools: []domain.ToolReference{searchTool, crawlTool}, Skills: []domain.SkillReference{{SkillID: "outline", Version: "1"}}}, nil
		},
		ListFunc: func(context.Context) ([]domain.AgentTypeDescriptor, error) { return nil, nil },
	}
	writerModels = fake.ModelCatalog{
		ListFunc: func(context.Context, int64) ([]domain.StudioModel, error) {
			return []domain.StudioModel{{Reference: domain.ModelReference{ProviderID: "provider-a", ModelID: "model-a"}, Active: true}}, nil
		},
		ValidateFunc: func(_ context.Context, _ int64, selection domain.AgentModelSelection) ([]domain.AgentFieldError, error) {
			if selection.Text != nil && selection.Text.ModelID != "model-a" {
				return []domain.AgentFieldError{{Field: "models.text", Message: "not available"}}, nil
			}
			return nil, nil
		},
	}
)

// publishedRelease is a release as publication freezes it: compiled prompts with their provenance,
// the settings digest of its frozen resources, a canonical governance section, and its digest.
func publishedRelease(t *testing.T) (domain.AgentDefinition, []domain.EffectiveResource) {
	t.Helper()
	compiler := &PromptCompiler{}
	compiled, err := compiler.Compile("en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""), agentLayer(domain.MergeAppend, "be brief", ""))})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Sections[0].Source.ScopeID == "" {
		t.Fatal("the fixture needs scope provenance to prove export strips it")
	}
	compiled.Sections[0].Source.Approver = &nine
	resources := []domain.EffectiveResource{
		{ResourceKind: domain.ResourceTool, ResourceID: "tool", Value: []byte(`["search"]`), Source: domain.Provenance{ScopeID: "a:writer-a", Approver: &nine}},
		{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(`{"tokens":100,"cost_minor_units":0,"period":"turn"}`)},
	}
	section, err := canonicalSection(json.RawMessage(`{"purpose":"write <well>","triggers":["brief"]}`))
	if err != nil {
		t.Fatal(err)
	}
	definition := domain.AgentDefinition{
		AgentID: "writer-a", AgentTypeID: "writer", Version: "3", Enabled: true, Models: domain.AgentModelSelection{Text: modelReference("provider-a", "model-a")},
		Languages: []domain.CompiledPrompt{compiled}, Extension: json.RawMessage(`{"tone":"formal"}`), Tools: []domain.ToolReference{searchTool},
		Sections: map[string]json.RawMessage{domain.GovernanceSection: section}, SettingsDigest: scope.SettingsDigest(resources),
		DraftRevision: 4, PromptProfileRevision: 2, PublishedBy: &nine, PublishedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	if definition.DefinitionDigest, err = compiler.DefinitionDigest(definition); err != nil {
		t.Fatal(err)
	}
	return definition, resources
}

func agentScopeBindings() []domain.ScopedBinding {
	return []domain.ScopedBinding{
		{ScopeID: "a:writer-a", ResourceKind: domain.ResourcePromptSection, ResourceID: "1/en-US", MergeMode: domain.MergeAppend, Value: []byte(`{"instruction":"be brief"}`), BoundBy: &nine},
		{ScopeID: "a:writer-a", ResourceKind: domain.ResourceKnowledge, ResourceID: "knowledge", MergeMode: domain.MergeReplace, Value: []byte(`["kb-1"]`), BoundBy: &nine},
		{ScopeID: "a:writer-a", ResourceKind: domain.ResourceEntitlement, ResourceID: "entitlement", MergeMode: domain.MergeReplace, Value: []byte(`["label-a"]`)},
		{ScopeID: "a:writer-a", ResourceKind: domain.ResourceTool, ResourceID: "tool", MergeMode: domain.MergeReplace, Value: []byte(`["search"]`)},
		{ScopeID: "t:writer", ResourceKind: domain.ResourceBudget, ResourceID: "budget", MergeMode: domain.MergeReplace, Value: []byte(`{"tokens":100,"cost_minor_units":0,"period":"turn"}`)},
	}
}

type portabilityHarness struct {
	db          *seedDB
	studio      *StudioService
	portability *StudioPortability
	codec       *portableCodec
	denied      map[domain.StudioAction]bool
	bindings    []domain.ScopedBinding
}

func newPortabilityHarness(t *testing.T) *portabilityHarness {
	t.Helper()
	definition, resources := publishedRelease(t)
	encodedDefinition, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	draft := []any{"writer", "Writer", "active", true, "provider-a", "model-a", nil, nil, nil, nil, `{"tone":"formal"}`, int64(4), int64(2), false}
	h := &portabilityHarness{denied: map[domain.StudioAction]bool{}, bindings: agentScopeBindings(), codec: &portableCodec{}}
	h.db = newSeedDB(map[string][][]any{
		qStudioGetDraft:         {draft},
		qStudioActiveDefinition: {{"3", string(encodedDefinition)}},
		qStudioGetSettings:      {{"a:writer-a", string(payload), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), int64(9)}},
		qSeedAgentType:          {{"1", typeDigest}},
		qSeedPromptSections:     {{int64(1), "task"}, {int64(2), "tone"}},
		qSeedTypeVersion:        {{typeDigest}},
		qSeedKnowledgeOwned:     {{"kb-9"}},
		qStudioLockType:         {{"writer"}},
		qStudioTypeCensus:       {{int64(0), int64(0)}},
		qStudioCreateRecord:     {{"writer-00000000000000000000"}},
		qStudioLockDraft:        {{int64(4), "writer"}},
		qStudioLockAlias:        {{"writer-a", int64(2)}},
		qStudioUpdateDraft:      {{int64(5)}},
		qStudioListAgents: {
			{"writer-00000000000000000000", "writer", "Imported", "draft", false, int64(1), int64(0), false, nil, nil, nil},
			{"writer-a", "writer", "Writer", "active", true, int64(5), int64(2), false, nil, nil, nil},
		},
	})
	h.studio = &StudioService{
		DB: h.db.repository(), Principals: studioPrincipals, MaxAgentsPerType: 5, Entropy: bytes.NewReader(make([]byte, 64)),
		Roles: fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, _, action, _ string) (domain.AuthorizationGrant, error) {
			return domain.AuthorizationGrant{Allowed: !h.denied[domain.StudioAction(action)], LowLimit: "*"}, nil
		}),
		Scopes: &fake.ScopeRepository{
			ChainFunc: studioScopes(nil).ChainFunc,
			BindingsFunc: func(_ context.Context, _ int64, ids []string, _ time.Time) ([]domain.ScopedBinding, error) {
				return slices.DeleteFunc(slices.Clone(h.bindings), func(b domain.ScopedBinding) bool { return !slices.Contains(ids, b.ScopeID) }), nil
			},
		},
		Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{AgentID: "writer-a", AgentTypeID: "writer", LanguageCode: "en-US",
			Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""), agentLayer(domain.MergeAppend, "be brief", ""))}}},
		Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{},
		Kinds: writerKinds, Catalog: writerModels,
	}
	h.portability = &StudioPortability{Studio: h.studio, Documents: h.codec}
	return h
}

// exported runs an export and returns what the codec was asked to render.
func (h *portabilityHarness) exported(t *testing.T) domain.PortableAgent {
	t.Helper()
	if _, err := h.portability.Export(context.Background(), humanActor(8, 9), "writer-a"); err != nil {
		t.Fatal(err)
	}
	return *h.codec.encoded
}

// importable is the export as a decode would return it.
func (h *portabilityHarness) importable(t *testing.T) domain.PortableAgent {
	t.Helper()
	agent := h.exported(t)
	agent.Document = domain.CharterObjectRef{Kind: domain.DerivedFromDefinitionKind, ID: "writer-a-v3", Namespace: "acme.test", External: true}
	agent.Digest = "sha256:" + strings.Repeat("d", 64)
	return agent
}

var portableReferences = map[domain.ResourceKind]map[string]string{
	domain.ResourceKnowledge: {"kb-1": "kb-9"}, domain.ResourceEntitlement: {"label-a": "label-z"},
}

func (h *portabilityHarness) importNew(agent domain.PortableAgent, references map[domain.ResourceKind]map[string]string) (domain.AgentSummary, error) {
	h.codec.decoded = agent
	return h.portability.Import(context.Background(), humanActor(9, 21), domain.AgentImportRequest{
		Document: json.RawMessage(`{}`), DisplayName: "Imported", IdempotencyKey: "import-1", References: references,
	})
}

func TestStudioExportRendersTheActiveReleaseWithoutAuthorsOrOtherScopes(t *testing.T) {
	h := newPortabilityHarness(t)
	agent := h.exported(t)

	if agent.AgentTypeID != "writer" || agent.TypeVersion != "1" || agent.TypeDigest != typeDigest || agent.DisplayName != "Writer" ||
		string(agent.Extension) != `{"tone":"formal"}` || agent.Models.Text.ModelID != "model-a" {
		t.Fatalf("agent = %+v", agent)
	}
	if len(agent.Bindings) != 4 {
		t.Fatalf("only the agent's own scope travels, got %+v", agent.Bindings)
	}
	prompt := agent.Bindings[slices.IndexFunc(agent.Bindings, func(b domain.PortableBinding) bool { return b.ResourceKind == domain.ResourcePromptSection })]
	if prompt.Caption != "task" || prompt.Language != "en-US" || prompt.ResourceID != "" {
		t.Fatalf("a prompt binding travels by caption, got %+v", prompt)
	}
	release := agent.Release
	if release.PublishedBy != nil || release.DraftRevision != 0 || release.PromptProfileRevision != 0 || release.Languages[0].Sections[0].Source != (domain.Provenance{}) {
		t.Fatalf("authors and scope provenance must be stripped: %+v", release)
	}
	if digest, err := (&PromptCompiler{}).DefinitionDigest(release); err != nil || digest != release.DefinitionDigest {
		t.Fatalf("the sanitized release must still verify: %v", err)
	}
	if len(agent.Settings) != 2 || scope.SettingsDigest([]domain.EffectiveResource{
		{ResourceKind: agent.Settings[0].ResourceKind, ResourceID: agent.Settings[0].ResourceID, Value: agent.Settings[0].Value},
		{ResourceKind: agent.Settings[1].ResourceKind, ResourceID: agent.Settings[1].ResourceID, Value: agent.Settings[1].Value},
	}) != release.SettingsDigest {
		t.Fatalf("frozen settings travel byte for byte in frozen order: %+v", agent.Settings)
	}
	encoded, _ := json.Marshal(agent)
	for _, leak := range []string{`"Approver":9`, `"approver":9`, `"published_by"`, `"BoundBy"`, "a:writer-a", "t:writer"} {
		if strings.Contains(string(encoded), leak) {
			t.Fatalf("the export leaks %s: %s", leak, encoded)
		}
	}
	allowed := map[string]bool{qStudioGetDraft: true, qStudioActiveDefinition: true, qStudioGetSettings: true, qSeedAgentType: true,
		qSeedPromptSections: true, qGuardrailDraftGet: true, qSeedDraftOrigin: true, qStudioAudit: true}
	for _, name := range h.db.names() {
		if !allowed[name] {
			t.Fatalf("export read %s; it may read only the draft, its own bindings, guardrails, origin, and the active release", name)
		}
	}
	audit := h.db.last(qStudioAudit)
	if audit[0] != int64(8) || audit[2] != "EXPORT" || !strings.HasPrefix(audit[3].(string), "version 3 sha256:") {
		t.Fatalf("audit = %v", audit)
	}
}

func TestStudioExportRefusals(t *testing.T) {
	h := newPortabilityHarness(t)
	h.denied[domain.StudioView] = true
	if _, err := h.portability.Export(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrForbidden) || len(h.db.calls) != 0 {
		t.Fatalf("VIEW denied must refuse before any read: %v, %v", err, h.db.names())
	}

	h = newPortabilityHarness(t)
	h.db.rows[qStudioGetDraft] = nil
	if _, err := h.portability.Export(context.Background(), humanActor(7, 9), "writer-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's agent is not found, got %v", err)
	}

	h = newPortabilityHarness(t)
	h.db.rows[qStudioActiveDefinition] = nil
	if _, err := h.portability.Export(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an unpublished agent cannot export, got %v", err)
	}

	h = newPortabilityHarness(t)
	h.bindings = append(h.bindings, domain.ScopedBinding{ScopeID: "a:writer-a", ResourceKind: "crm_account", ResourceID: "crm", Value: []byte(`"secret"`)})
	if _, err := h.portability.Export(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a product kind refuses the export, got %v", err)
	}

	h = newPortabilityHarness(t)
	h.db.fail[qStudioAudit] = errors.New("audit down")
	if _, err := h.portability.Export(context.Background(), humanActor(8, 9), "writer-a"); err == nil {
		t.Fatal("an unaudited export must fail")
	}

	if _, err := (&StudioPortability{}).Export(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("an uncomposed service is not ready, got %v", err)
	}
}

func TestStudioImportCreatesADisabledAgentWithMappedReferences(t *testing.T) {
	h := newPortabilityHarness(t)
	agent := h.importable(t)
	start := len(h.db.calls)

	summary, err := h.importNew(agent, portableReferences)
	if err != nil {
		t.Fatal(err)
	}
	if summary.AgentID != "writer-00000000000000000000" || h.db.commits != 1 {
		t.Fatalf("summary = %+v, commits %d", summary, h.db.commits)
	}
	inserted := h.db.args(qStudioInsertSetting)
	if len(inserted) != 4 {
		t.Fatalf("every agent-scope binding is planted: %v", inserted)
	}
	for _, args := range inserted {
		if args[0] != int64(9) || args[1] != "a:writer-00000000000000000000" {
			t.Fatalf("bindings land in the importing tenant's new scope: %v", args)
		}
		value := args[7].(string)
		if strings.Contains(value, "kb-1") || strings.Contains(value, "label-a") {
			t.Fatalf("a source-tenant reference was kept: %v", args)
		}
	}
	values := map[string]string{}
	for _, args := range inserted {
		values[args[2].(string)] = args[7].(string)
	}
	if values["knowledge"] != `["kb-9"]` || values["entitlement"] != `["label-z"]` || values["prompt_section"] != `{"instruction":"be brief"}` {
		t.Fatalf("values = %v", values)
	}
	if h.db.last(qSeedPinTypeVersion)[0] != "1" {
		t.Fatalf("the type version is pinned: %v", h.db.last(qSeedPinTypeVersion))
	}
	origin := h.db.last(qSeedPutOrigin)
	if origin[2] != "AgentDefinition" || origin[3] != "writer-a-v3" || origin[4] != "acme.test" || origin[6] != "3" || origin[7] != agent.Digest || origin[8] != nil {
		t.Fatalf("origin = %v", origin)
	}
	draft := h.db.last(qStudioCreateDraft)
	if draft[2] != "provider-a" || draft[3] != "model-a" || draft[8] != `{"tone":"formal"}` {
		t.Fatalf("draft = %v", draft)
	}
	audit := h.db.last(qStudioAudit)
	if audit[2] != "IMPORT" || !strings.HasPrefix(audit[3].(string), "acme.test/writer-a-v3 version 3 sha256:dddd") {
		t.Fatalf("audit = %v", audit)
	}
	order := h.db.namesFrom(start)
	if slices.Index(order, qStudioLockType) > slices.Index(order, qStudioCreateReplay) || slices.Index(order, qStudioCreateReplay) > slices.Index(order, qSeedKnowledgeOwned) {
		t.Fatalf("the replay lookup precedes every tenant-state check: %v", order)
	}
}

func TestStudioImportReplaysByKey(t *testing.T) {
	h := newPortabilityHarness(t)
	agent := h.importable(t)
	if _, err := h.importNew(agent, portableReferences); err != nil {
		t.Fatal(err)
	}
	digest := h.db.last(qStudioCreateRecord)[4]
	h.db.rows[qStudioCreateReplay] = [][]any{{digest, "writer-00000000000000000000"}}
	h.db.rows[qStudioTypeCensus] = [][]any{{int64(5), int64(0)}}
	h.db.rows[qSeedKnowledgeOwned] = nil
	profiles := h.db.count(qProvisionProfile)
	if summary, err := h.importNew(agent, portableReferences); err != nil || summary.AgentID != "writer-00000000000000000000" || h.db.count(qProvisionProfile) != profiles {
		t.Fatalf("a replay returns the first agent even past the cap and with changed tenant state: %+v %v", summary, err)
	}
	other := agent
	other.Digest = "sha256:" + strings.Repeat("f", 64)
	if _, err := h.importNew(other, portableReferences); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the same key with another document is a conflict, got %v", err)
	}
}

func TestStudioImportRefusesWhatWouldWidenOrLeak(t *testing.T) {
	type change func(*portabilityHarness, *domain.PortableAgent, *map[domain.ResourceKind]map[string]string)
	withBinding := func(binding domain.PortableBinding) change {
		return func(_ *portabilityHarness, agent *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			agent.Bindings = append(agent.Bindings, binding)
		}
	}
	typeBinding := func(kind domain.ResourceKind, value string, sealed bool) change {
		return func(h *portabilityHarness, _ *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			h.bindings = append(h.bindings, domain.ScopedBinding{ScopeID: "t:writer", ResourceKind: kind, ResourceID: string(kind), MergeMode: domain.MergeReplace, Sealed: sealed, Value: []byte(value)})
		}
	}
	both := func(changes ...change) change {
		return func(h *portabilityHarness, agent *domain.PortableAgent, references *map[domain.ResourceKind]map[string]string) {
			for _, c := range changes {
				c(h, agent, references)
			}
		}
	}
	replaceBinding := func(kind domain.ResourceKind, value string) change {
		return func(_ *portabilityHarness, agent *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			for index := range agent.Bindings {
				if agent.Bindings[index].ResourceKind == kind {
					agent.Bindings[index].Value = json.RawMessage(value)
				}
			}
		}
	}
	cases := map[string]struct {
		change change
		want   error
	}{
		"unmapped knowledge": {func(_ *portabilityHarness, _ *domain.PortableAgent, r *map[domain.ResourceKind]map[string]string) {
			*r = map[domain.ResourceKind]map[string]string{domain.ResourceEntitlement: {"label-a": "label-z"}}
		}, domain.ErrValidation},
		"unused mapping": {func(_ *portabilityHarness, _ *domain.PortableAgent, r *map[domain.ResourceKind]map[string]string) {
			*r = map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"kb-1": "kb-9", "kb-2": "kb-9"}, domain.ResourceEntitlement: {"label-a": "label-z"}}
		}, domain.ErrValidation},
		"mapping of another kind": {func(_ *portabilityHarness, _ *domain.PortableAgent, r *map[domain.ResourceKind]map[string]string) {
			(*r)[domain.ResourceTool] = map[string]string{"search": "crawl"}
		}, domain.ErrValidation},
		"unowned knowledge target": {func(h *portabilityHarness, _ *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			h.db.rows[qSeedKnowledgeOwned] = nil
		}, domain.ErrValidation},
		"tool outside the type": {replaceBinding(domain.ResourceTool, `["search","shell"]`), domain.ErrValidation},
		"release tool not offered": {func(_ *portabilityHarness, a *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			a.Release.Tools = []domain.ToolReference{{ToolID: "shell"}}
		}, domain.ErrValidation},
		"model binding not offered": {withBinding(domain.PortableBinding{ResourceKind: domain.ResourceModel, ResourceID: "model", MergeMode: domain.MergeReplace, Value: json.RawMessage(`["provider-b/model-b"]`)}), domain.ErrValidation},
		"draft model not offered": {func(_ *portabilityHarness, a *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			a.Models.Text = modelReference("provider-a", "model-z")
		}, domain.ErrValidation},
		"unknown caption":  {withBinding(domain.PortableBinding{ResourceKind: domain.ResourcePromptSection, Caption: "secret_sauce", Language: "en-US", MergeMode: domain.MergeAppend, Value: json.RawMessage(`{"instruction":"x"}`)}), domain.ErrValidation},
		"intersect prompt": {withBinding(domain.PortableBinding{ResourceKind: domain.ResourcePromptSection, Caption: "tone", Language: "en-US", MergeMode: domain.MergeIntersect, Value: json.RawMessage(`{"instruction":"x"}`)}), domain.ErrValidation},
		"missing type version": {func(h *portabilityHarness, _ *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			h.db.rows[qSeedTypeVersion] = nil
		}, domain.ErrValidation},
		"other type digest": {func(_ *portabilityHarness, a *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			a.TypeDigest = strings.Repeat("0", 64)
		}, domain.ErrValidation},
		"definition digest": {func(_ *portabilityHarness, a *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			a.Release.Enabled = false
		}, domain.ErrValidation},
		"version longer than an origin holds": {func(_ *portabilityHarness, a *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			a.Release.Version = strings.Repeat("9", 61)
		}, domain.ErrValidation},
		"namespace longer than an origin holds": {func(_ *portabilityHarness, a *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			a.Document.Namespace = strings.Repeat("n", 201)
		}, domain.ErrValidation},
		"settings digest": {func(_ *portabilityHarness, a *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			a.Settings[1].Value = []byte(`{"tokens":100000,"cost_minor_units":0,"period":"turn"}`)
		}, domain.ErrValidation},
		"wider tool set": {both(typeBinding(domain.ResourceTool, `["search"]`, false), replaceBinding(domain.ResourceTool, `["crawl","search"]`)), domain.ErrAuthorityExceeded},
		"extra policy allow": {both(typeBinding(domain.ResourcePolicy, `[{"id":"read","effect":"allow","actions":["read"],"resources":["*"]}]`, false),
			withBinding(domain.PortableBinding{ResourceKind: domain.ResourcePolicy, ResourceID: "policy", MergeMode: domain.MergeReplace, Value: json.RawMessage(`[{"id":"write","effect":"allow","actions":["write"],"resources":["*"]}]`)})), domain.ErrAuthorityExceeded},
		"sealed type value": {typeBinding(domain.ResourceTool, `["search"]`, true), domain.ErrSealed},
		"guardrails without the feature": {func(_ *portabilityHarness, a *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			a.Guardrails = []domain.GuardrailRule{ownPhrase}
		}, domain.ErrValidation},
		"product validator": {func(h *portabilityHarness, _ *domain.PortableAgent, _ *map[domain.ResourceKind]map[string]string) {
			h.studio.Validators = []contract.AgentDraftValidator{fake.DraftValidator{ValidateFunc: func(_ context.Context, _ int64, draft domain.AgentDraft, _ domain.ValidationPhase) ([]domain.AgentFieldError, error) {
				if string(draft.Extension) == `{"tone":"formal"}` {
					return []domain.AgentFieldError{{Field: "extension", Message: "foreign extension"}}, nil
				}
				return nil, nil
			}}}
		}, domain.ErrValidation},
	}
	for name, test := range cases {
		h := newPortabilityHarness(t)
		agent := h.importable(t)
		references := map[domain.ResourceKind]map[string]string{
			domain.ResourceKnowledge: {"kb-1": "kb-9"}, domain.ResourceEntitlement: {"label-a": "label-z"},
		}
		test.change(h, &agent, &references)
		_, err := h.importNew(agent, references)
		if !errors.Is(err, test.want) {
			t.Errorf("%s: want %v, got %v", name, test.want, err)
		}
		if h.db.count(qStudioInsertSetting) != 0 || h.db.count(qProvisionProfile) != 0 || h.db.commits != 0 {
			t.Errorf("%s: a refused import wrote %v", name, h.db.names())
		}
	}
}

// Budget and autonomy merge to the tighter of what a scope binds and inherits, as a saved setting
// does, so a higher imported value cannot widen what the agent runs with.
func TestStudioImportCannotRaiseAClampedCeiling(t *testing.T) {
	h := newPortabilityHarness(t)
	h.bindings = append(h.bindings, domain.ScopedBinding{ScopeID: "t:writer", ResourceKind: domain.ResourceAutonomy, ResourceID: "autonomy", MergeMode: domain.MergeReplace, Value: []byte(`{"mode":"draft"}`)})
	adopted, err := h.studio.adopt(context.Background(), humanActor(9, 21), "writer", "document.bindings", []domain.PortableBinding{
		{ResourceKind: domain.ResourceBudget, ResourceID: "budget", MergeMode: domain.MergeReplace, Value: json.RawMessage(`{"tokens":500,"cost_minor_units":0,"period":"turn"}`)},
		{ResourceKind: domain.ResourceAutonomy, ResourceID: "autonomy", MergeMode: domain.MergeReplace, Value: json.RawMessage(`{"mode":"bounded_autonomous"}`)},
	}, nil, adoptImport, nil)
	if err != nil {
		t.Fatal(err)
	}
	placed := adopted.scoped
	for index := range placed {
		placed[index].ScopeID = "a:writer-new"
	}
	release, err := h.studio.compileOver(context.Background(), seededScopes{ScopeRepository: h.studio.Scopes, scopeID: "a:writer-new", bindings: placed}, 9, "a:writer-new", "writer-new", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range release.Resources {
		if resource.ResourceKind == domain.ResourceBudget && !strings.Contains(string(resource.Value), `"tokens":100`) ||
			resource.ResourceKind == domain.ResourceAutonomy && !strings.Contains(string(resource.Value), `"draft"`) {
			t.Fatalf("%s widened to %s", resource.ResourceKind, resource.Value)
		}
	}
}

func TestStudioImportChecksGuardrailsWhereComposed(t *testing.T) {
	h := newPortabilityHarness(t)
	compiler, err := guardrail.NewRuleSetCompiler(guardrail.CompilerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	h.studio.Kinds = fake.KindCatalog{GetFunc: func(ctx context.Context, kind string) (domain.AgentTypeDescriptor, error) {
		descriptor, err := writerKinds.Get(ctx, kind)
		descriptor.Guardrails = []domain.GuardrailRule{typeSize}
		return descriptor, err
	}, ListFunc: writerKinds.ListFunc}
	h.portability.Guardrails = &StudioGuardrails{Studio: h.studio, Rules: compiler,
		Restrictions: restrictionLayersFunc(func(context.Context, int64) (domain.RestrictionLayers, error) { return domain.RestrictionLayers{}, nil })}
	agent := h.importable(t)

	agent.Guardrails = []domain.GuardrailRule{gRule("type.size", domain.GuardrailKindMaxOutputBytes, domain.GuardrailActionBlock, `{"max":5000}`)}
	if _, err := h.importNew(agent, portableReferences); !errors.Is(err, domain.ErrAuthorityExceeded) || h.db.commits != 0 {
		t.Fatalf("a rule that loosens the type's is refused, got %v", err)
	}
	agent.Guardrails = []domain.GuardrailRule{tighterSize, ownPhrase}
	if _, err := h.importNew(agent, portableReferences); err != nil {
		t.Fatal(err)
	}
	stored := h.db.last(qGuardrailDraftPut)
	if stored[1] != "writer-00000000000000000000" || !strings.Contains(stored[2].(string), "BLUEJAY") {
		t.Fatalf("own rules are stored with the new agent: %v", stored)
	}
}

func (h *portabilityHarness) replace(agent domain.PortableAgent, revision int64) (domain.AgentSummary, error) {
	h.codec.decoded = agent
	return h.portability.Import(context.Background(), humanActor(8, 9), domain.AgentImportRequest{
		Document: json.RawMessage(`{}`), AgentID: "writer-a", ExpectedDraftRevision: revision, References: portableReferences,
	})
}

func TestStudioImportReplacesAnAgentsDraft(t *testing.T) {
	h := newPortabilityHarness(t)
	agent := h.importable(t)
	agent.Bindings = slices.DeleteFunc(agent.Bindings, func(b domain.PortableBinding) bool { return b.ResourceKind == domain.ResourceTool })
	start := len(h.db.calls)

	if _, err := h.replace(agent, 4); err != nil {
		t.Fatal(err)
	}
	order := h.db.namesFrom(start)
	steps := []string{qStudioLockDraft, qStudioLockAlias, qSeedNonPlatformAtScope, qStudioUpdateDraft, qSeedDropScope, qSeedEndScope, qStudioInsertSetting, qSeedPutOrigin, qStudioAudit}
	last := -1
	for _, step := range steps {
		at := slices.Index(order, step)
		if at <= last {
			t.Fatalf("step %s out of order: %v", step, order)
		}
		last = at
	}
	if end := h.db.last(qSeedEndScope); end[0] != int64(8) || end[1] != "a:writer-a" {
		t.Fatalf("only the agent's own scope is ended: %v", end)
	}
	if update := h.db.last(qStudioUpdateDraft); update[0] != true || update[11] != int64(4) {
		t.Fatalf("the replace keeps enabled and checks the revision: %v", update)
	}
	if h.db.count(qStudioUpdateProfile) != 0 || h.db.count(qStudioRename) != 0 || h.db.count(qStudioBumpAlias) != 0 {
		t.Fatalf("the name and type defaults are untouched: %v", order)
	}
	if h.db.count(qProvisionProfile) != 0 || h.db.commits != 1 || h.db.last(qStudioAudit)[2] != "IMPORT" {
		t.Fatalf("calls = %v", order)
	}
}

func TestStudioImportReplaceRefusals(t *testing.T) {
	h := newPortabilityHarness(t)
	agent := h.importable(t)

	h.denied[domain.StudioEdit] = true
	if _, err := h.replace(agent, 4); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("EDIT is required, got %v", err)
	}

	h = newPortabilityHarness(t)
	h.db.rows[qStudioGetDraft] = [][]any{draftRow(true)}
	if _, err := h.replace(agent, 4); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an archived agent is read-only, got %v", err)
	}

	h = newPortabilityHarness(t)
	other := agent
	other.AgentTypeID, other.Release.AgentTypeID = "editor", "editor"
	if _, err := h.replace(other, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("another type is refused, got %v", err)
	}

	h = newPortabilityHarness(t)
	if _, err := h.replace(agent, 3); !errors.Is(err, domain.ErrRevisionConflict) || h.db.count(qStudioInsertSetting) != 0 {
		t.Fatalf("a stale revision conflicts, got %v", err)
	}

	h = newPortabilityHarness(t)
	h.db.rows[qSeedNonPlatformAtScope] = [][]any{{int64(1)}}
	if _, err := h.replace(agent, 4); !errors.Is(err, domain.ErrConflict) || h.db.count(qSeedEndScope) != 0 {
		t.Fatalf("a product binding at the scope would be dropped, got %v", err)
	}

	h = newPortabilityHarness(t)
	modelless := agent
	modelless.Models = domain.AgentModelSelection{}
	if _, err := h.replace(modelless, 4); !errors.Is(err, domain.ErrValidation) || h.db.count(qStudioUpdateDraft) != 0 {
		t.Fatalf("an enabled agent needs a text model, got %v", err)
	}
}

func TestStudioImportValidatesTheRequestShape(t *testing.T) {
	h := newPortabilityHarness(t)
	for name, request := range map[string]domain.AgentImportRequest{
		"no document":         {IdempotencyKey: "k"},
		"no key":              {Document: json.RawMessage(`{}`)},
		"revision on create":  {Document: json.RawMessage(`{}`), IdempotencyKey: "k", ExpectedDraftRevision: 2},
		"no revision":         {Document: json.RawMessage(`{}`), AgentID: "writer-a"},
		"name on replace":     {Document: json.RawMessage(`{}`), AgentID: "writer-a", ExpectedDraftRevision: 2, DisplayName: "x"},
		"long reference":      {Document: json.RawMessage(`{}`), IdempotencyKey: "k", References: map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"a": strings.Repeat("x", 201)}}},
		"reference of a tool": {Document: json.RawMessage(`{}`), IdempotencyKey: "k", References: map[domain.ResourceKind]map[string]string{domain.ResourceTool: {"a": "b"}}},
	} {
		if _, err := h.portability.Import(context.Background(), humanActor(9, 21), request); !errors.Is(err, domain.ErrValidation) || len(h.db.calls) != 0 {
			t.Errorf("%s: want a validation error before any read, got %v", name, err)
		}
	}
}

const portableRouting = `{"routes":[{"id":"payments","match":{"tools":["pay*"]},"approver":{"kind":"role","ref":"cfo"},` +
	`"deadline_seconds":3600,"escalation":[{"approver":{"kind":"position","ref":"lead"},"extension_seconds":3600}],"on_exhausted":"expire"}]}`

func routingBinding() domain.PortableBinding {
	return domain.PortableBinding{ResourceKind: domain.ResourceApprovalRouting, ResourceID: "approval_routing", MergeMode: domain.MergeReplace, Value: json.RawMessage(portableRouting)}
}

// Approvers name the source tenant's roles and positions, so an import maps every one of them and
// never keeps the source's reference.
func TestStudioImportMapsEveryApprover(t *testing.T) {
	h := newPortabilityHarness(t)
	mapping := map[domain.ResourceKind]map[string]string{domain.ResourceApprovalRouting: {"role:cfo": "role:finance", "position:lead": "position:p-9"}}
	adopted, err := h.studio.adopt(context.Background(), humanActor(9, 21), "writer", "document.bindings", []domain.PortableBinding{routingBinding()}, mapping, adoptImport, nil)
	if err != nil {
		t.Fatal(err)
	}
	value := string(adopted.scoped[0].Value)
	if !strings.Contains(value, `"ref":"finance"`) || !strings.Contains(value, `"ref":"p-9"`) || strings.Contains(value, "cfo") || strings.Contains(value, "lead") {
		t.Fatalf("routing = %s", value)
	}
	delete(mapping[domain.ResourceApprovalRouting], "position:lead")
	_, err = h.studio.adopt(context.Background(), humanActor(9, 21), "writer", "document.bindings", []domain.PortableBinding{routingBinding()}, mapping, adoptImport, nil)
	if fields := fieldsOf(t, err); !slices.Contains(fields, "references.approval_routing.position:lead") {
		t.Fatalf("an unmapped approver must be refused, fields = %v", fields)
	}
	if problems := validateReferences("references", map[domain.ResourceKind]map[string]string{domain.ResourceApprovalRouting: {"role:cfo": "finance"}}, DefaultMaxReferences); len(problems) != 1 {
		t.Fatalf("an approver mapped to no kind must be refused: %v", problems)
	}
}

// A saved template keeps a slot in place of each approver, so no tenant's role travels with it.
func TestStudioTemplateSaveTurnsApproversIntoSlots(t *testing.T) {
	h := newPortabilityHarness(t)
	slots := map[domain.ResourceKind]map[string]string{domain.ResourceApprovalRouting: {"role:cfo": "approver", "position:lead": "backup"}}
	adopted, err := h.studio.adopt(context.Background(), humanActor(9, 21), "writer", "bindings", []domain.PortableBinding{routingBinding()}, slots, adoptTemplateSave, nil)
	if err != nil {
		t.Fatal(err)
	}
	value := string(adopted.portable[0].Value)
	if !slices.Equal(adopted.slots[domain.ResourceApprovalRouting], []string{"approver", "backup"}) || strings.Contains(value, "cfo") || strings.Contains(value, "lead") {
		t.Fatalf("slots = %v, routing = %s", adopted.slots, value)
	}
	created, err := h.studio.adopt(context.Background(), humanActor(9, 21), "writer", "template.bindings", adopted.portable,
		map[domain.ResourceKind]map[string]string{domain.ResourceApprovalRouting: {"approver": "role:finance", "backup": "position:p-9"}}, adoptTemplateCreate, adopted.slots)
	if err != nil {
		t.Fatal(err)
	}
	if value = string(created.scoped[0].Value); !strings.Contains(value, `"ref":"finance"`) || !strings.Contains(value, `"kind":"position","ref":"p-9"`) {
		t.Fatalf("created routing = %s", value)
	}
}

// An imported cost budget is checked against the currency catalog as a saved one is.
func TestStudioImportChecksABudgetsCurrency(t *testing.T) {
	h := newPortabilityHarness(t)
	budget := domain.PortableBinding{ResourceKind: domain.ResourceBudget, ResourceID: "budget", MergeMode: domain.MergeReplace,
		Value: json.RawMessage(`{"tokens":0,"cost_minor_units":500,"currency":"XXX","exponent":2,"period":"turn"}`)}
	_, err := h.studio.adopt(context.Background(), humanActor(9, 21), "writer", "document.bindings", []domain.PortableBinding{budget}, nil, adoptImport, nil)
	if fields := fieldsOf(t, err); len(fields) != 1 || !strings.HasPrefix(fields[0], "document.bindings[0].") {
		t.Fatalf("an unknown currency must be refused at its binding, fields = %v, error = %v", fields, err)
	}
}
