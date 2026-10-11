package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/guardrail"
)

// catalogTxFake lets a release writer bind its own query catalog to the fake transaction.
type catalogTxFake struct{ *studioQueryFake }

func (tx catalogTxFake) QueryService(string, map[string]string) keelport.QueryService {
	return tx.studioQueryFake
}

type catalogDBFake struct{ studioDBFake }

func (db catalogDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return catalogTxFake{db.qs}, nil
}

type restrictionLayersFunc func(context.Context, int64) (domain.RestrictionLayers, error)

func (function restrictionLayersFunc) Layers(ctx context.Context, tenantID int64) (domain.RestrictionLayers, error) {
	return function(ctx, tenantID)
}

func gRule(id string, kind domain.GuardrailRuleKind, action domain.GuardrailAction, params string) domain.GuardrailRule {
	rule := domain.GuardrailRule{ID: id, Kind: kind, Action: action, Severity: domain.GuardrailSeverityHard}
	if params != "" {
		rule.Params = json.RawMessage(params)
	}
	return rule
}

var (
	typeSize    = gRule("type.size", domain.GuardrailKindMaxOutputBytes, domain.GuardrailActionBlock, `{"max":1000}`)
	tenantPII   = gRule("pii", domain.GuardrailKindPII, domain.GuardrailActionRedact, `{"threshold":0.5}`)
	ownPhrase   = gRule("own.codename", domain.GuardrailKindExactPhrase, domain.GuardrailActionBlock, `{"phrases":["BLUEJAY"]}`)
	tighterSize = gRule("type.size", domain.GuardrailKindMaxOutputBytes, domain.GuardrailActionBlock, `{"max":500}`)
)

func draftRow(archived bool) []any {
	state := "active"
	if archived {
		state = "archived"
	}
	return []any{"writer", "Writer", state, true, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}
}

// storedRules is the agent_draft_guardrail row holding rules.
func storedRules(t *testing.T, rules ...domain.GuardrailRule) [][]any {
	t.Helper()
	envelope, digest, _, err := canonicalRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	return [][]any{{string(envelope), digest}}
}

type guardrailHarness struct {
	guardrails *StudioGuardrails
	query      *studioQueryFake
	tenants    []int64
	typeRules  []domain.GuardrailRule
	layers     domain.RestrictionLayers
	layersErr  error
}

func newGuardrailHarness(t *testing.T, rows map[string][][]any) *guardrailHarness {
	t.Helper()
	h := &guardrailHarness{query: &studioQueryFake{rows: rows, args: map[string][]any{}}}
	compiler, err := guardrail.NewRuleSetCompiler(guardrail.CompilerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	studio := &StudioService{Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil),
		DB: catalogDBFake{studioDBFake{qs: h.query}}, Catalog: studioModelCatalogFake{},
		Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{AgentID: "writer-a", AgentTypeID: "writer", BaselineKey: "global",
			LanguageCode: "en-US", Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))}}},
		Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{},
		Kinds: fake.KindCatalog{GetFunc: func(_ context.Context, kind string) (domain.AgentTypeDescriptor, error) {
			return domain.AgentTypeDescriptor{AgentTypeID: kind, Guardrails: h.typeRules}, nil
		}},
	}
	h.guardrails = &StudioGuardrails{Studio: studio, Rules: compiler,
		Restrictions: restrictionLayersFunc(func(_ context.Context, tenantID int64) (domain.RestrictionLayers, error) {
			h.tenants = append(h.tenants, tenantID)
			return h.layers, h.layersErr
		}),
	}
	studio.Sections = append(studio.Sections, h.guardrails)
	studio.ReleaseWriters = append(studio.ReleaseWriters, h.guardrails)
	return h
}

func tenantLayer(rules ...domain.GuardrailRule) domain.RestrictionLayers {
	return domain.RestrictionLayers{Tenant: domain.RestrictionLayer{Digest: strings.Repeat("c", 64), Guardrails: rules}}
}

func TestStudioGuardrailsShowEveryInheritedLayerWidestFirst(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{qStudioGetDraft: {draftRow(false)}})
	h.query.rows[qGuardrailDraftGet] = storedRules(t, ownPhrase, tighterSize)
	h.typeRules = []domain.GuardrailRule{typeSize}
	h.layers = domain.RestrictionLayers{
		Platform: domain.RestrictionLayer{Digest: strings.Repeat("b", 64), Guardrails: []domain.GuardrailRule{gRule("platform.malware", domain.GuardrailKindMalware, domain.GuardrailActionBlock, "")}},
		Tenant:   tenantLayer(tenantPII).Tenant,
	}
	h.guardrails.Baseline = guardrail.DefaultBaseline(1<<10, 1<<12).Rules

	view, err := h.guardrails.Guardrails(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	var layers []domain.GuardrailLayer
	for _, layer := range view.Inherited {
		layers = append(layers, layer.Layer)
	}
	want := []domain.GuardrailLayer{domain.GuardrailLayerBaseline, domain.GuardrailLayerPlatform, domain.GuardrailLayerTenant, domain.GuardrailLayerType}
	if !slices.Equal(layers, want) || view.Inherited[2].Digest != strings.Repeat("c", 64) || view.Inherited[3].Rules[0].ID != "type.size" {
		t.Fatalf("inherited = %+v", view.Inherited)
	}
	if len(view.Rules) != 2 || view.Rules[1].ID != "type.size" || view.Problem != "" || view.DraftRevision != 4 {
		t.Fatalf("view = %+v", view)
	}
	if !slices.Equal(h.tenants, []int64{8}) || h.query.args[qGuardrailDraftGet][0] != int64(8) {
		t.Fatalf("the tenant comes from the actor: layers read for %v, rules for %v", h.tenants, h.query.args[qGuardrailDraftGet])
	}
}

func TestStudioGuardrailsReportOwnRulesAnInheritedLayerOutgrew(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{qStudioGetDraft: {draftRow(false)}})
	h.query.rows[qGuardrailDraftGet] = storedRules(t, gRule("pii", domain.GuardrailKindPII, domain.GuardrailActionRedact, `{"threshold":0.7}`))
	h.layers = tenantLayer(tenantPII)
	view, err := h.guardrails.Guardrails(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Problem, "tenant layer") || !strings.Contains(view.Problem, "threshold") || len(view.Rules) != 1 {
		t.Fatalf("view = %+v", view)
	}
	h.layersErr = errors.New("store down")
	if _, err = h.guardrails.Guardrails(context.Background(), humanActor(8, 9), "writer-a"); err == nil || view.Problem == err.Error() {
		t.Fatalf("an unreadable layer is an error, not a problem: %v", err)
	}
}

func TestSaveGuardrailsStoresCanonicalRulesBehindTheDraftRevision(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{qStudioGetDraft: {draftRow(false)}, qStudioBumpDraft: {{int64(5)}}})
	h.typeRules = []domain.GuardrailRule{typeSize}
	h.layers = tenantLayer(tenantPII)
	stricterPII := gRule("pii", domain.GuardrailKindPII, domain.GuardrailActionBlock, `{ "threshold" : 0.3 }`)
	_, err := h.guardrails.SaveGuardrails(context.Background(), humanActor(8, 9), domain.AgentGuardrailChange{
		AgentID: "writer-a", Rules: []domain.GuardrailRule{stricterPII, tighterSize, ownPhrase}, ExpectedDraftRevision: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bump := h.query.args[qStudioBumpDraft]; bump[0] != int64(9) || bump[1] != int64(8) || bump[2] != "writer-a" || bump[3] != int64(4) {
		t.Fatalf("bump args = %v", bump)
	}
	put := h.query.args[qGuardrailDraftPut]
	envelope := put[2].(string)
	if put[0] != int64(8) || put[1] != "writer-a" || put[3] != guardrail.Digest([]byte(envelope)) || put[4] != int64(9) ||
		!strings.Contains(envelope, `"params":{"threshold":0.3}`) || !strings.HasSuffix(envelope, `"schema_version":1}`) {
		t.Fatalf("put args = %v", put)
	}
	if audit := h.query.args[qStudioAudit]; audit[2] != "GUARDRAILS" || !strings.HasPrefix(audit[3].(string), "3 rules") {
		t.Fatalf("audit = %v", audit)
	}
	if h.query.commits != 1 {
		t.Fatalf("commits = %d", h.query.commits)
	}

	if _, err = h.guardrails.SaveGuardrails(context.Background(), humanActor(8, 9), domain.AgentGuardrailChange{AgentID: "writer-a", ExpectedDraftRevision: 5}); err != nil {
		t.Fatal(err)
	}
	if drop := h.query.args[qGuardrailDraftDrop]; drop[0] != int64(8) || drop[1] != "writer-a" || h.query.args[qStudioAudit][3] != "removed" {
		t.Fatalf("an empty set removes the rules: %v", h.query.queries)
	}
}

func TestSaveGuardrailsRefusesLooseningAnInheritedRuleUnwritten(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{qStudioGetDraft: {draftRow(false)}, qStudioBumpDraft: {{int64(5)}}})
	h.typeRules = []domain.GuardrailRule{typeSize}
	h.layers = tenantLayer(tenantPII)
	for name, rule := range map[string]domain.GuardrailRule{
		"tenant rule": gRule("pii", domain.GuardrailKindPII, domain.GuardrailActionFlag, `{"threshold":0.5}`),
		"type rule":   gRule("type.size", domain.GuardrailKindMaxOutputBytes, domain.GuardrailActionBlock, `{"max":5000}`),
		"other kind":  gRule("type.size", domain.GuardrailKindMaxInputBytes, domain.GuardrailActionBlock, `{"max":10}`),
	} {
		_, err := h.guardrails.SaveGuardrails(context.Background(), humanActor(8, 9), domain.AgentGuardrailChange{
			AgentID: "writer-a", Rules: []domain.GuardrailRule{rule}, ExpectedDraftRevision: 4,
		})
		if !errors.Is(err, domain.ErrAuthorityExceeded) {
			t.Fatalf("%s: want ErrAuthorityExceeded, got %v", name, err)
		}
	}
	if slices.Contains(h.query.queries, qStudioBumpDraft) || h.query.commits != 0 {
		t.Fatalf("a refused set must not be written: %v", h.query.queries)
	}
}

func TestSaveGuardrailsValidatesTheWholeSet(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{qStudioGetDraft: {draftRow(false)}, qStudioBumpDraft: {{int64(5)}}})
	actor := humanActor(8, 9)
	save := func(revision int64, rules ...domain.GuardrailRule) error {
		_, err := h.guardrails.SaveGuardrails(context.Background(), actor, domain.AgentGuardrailChange{AgentID: "writer-a", Rules: rules, ExpectedDraftRevision: revision})
		return err
	}
	var fields *StudioValidationError
	if err := save(0, gRule(" padded", domain.GuardrailKindPII, domain.GuardrailActionBlock, "")); !errors.As(err, &fields) ||
		len(fields.Fields) != 2 || fields.Fields[0].Field != "expected_agent_revision" || fields.Fields[1].Field != "rules[0].id" {
		t.Fatalf("field errors = %v", err)
	}
	if err := save(4, gRule("x", "telepathy", domain.GuardrailActionBlock, "")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown kind: want ErrValidation, got %v", err)
	}
	if err := save(4, ownPhrase, ownPhrase); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("duplicate id: want ErrValidation, got %v", err)
	}
	if err := save(4, gRule("bad", domain.GuardrailKindMaxInputBytes, domain.GuardrailActionBlock, `{max}`)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("malformed params: want ErrValidation, got %v", err)
	}
	limited, err := guardrail.NewRuleSetCompiler(guardrail.CompilerConfig{MaxRules: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.guardrails.Rules, h.typeRules = limited, []domain.GuardrailRule{typeSize}
	if err := save(4, ownPhrase); !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "type's rules") {
		t.Fatalf("the type's rules count toward the release's bound: %v", err)
	}
	if slices.Contains(h.query.queries, qStudioBumpDraft) {
		t.Fatal("an invalid set must not be written")
	}
}

func TestSaveGuardrailsIsRevisionCheckedAuthorizedAndRefusesAnArchivedAgent(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{qStudioGetDraft: {draftRow(false)}})
	change := domain.AgentGuardrailChange{AgentID: "writer-a", Rules: []domain.GuardrailRule{ownPhrase}, ExpectedDraftRevision: 3}
	if _, err := h.guardrails.SaveGuardrails(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrRevisionConflict) || h.query.commits != 0 {
		t.Fatalf("stale revision: %v, commits %d", err, h.query.commits)
	}
	var checked []string
	h.guardrails.Studio.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
		checked = append(checked, object+"/"+action+"/"+value)
		return domain.AuthorizationGrant{Allowed: action == string(domain.StudioView)}, nil
	})
	if _, err := h.guardrails.SaveGuardrails(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("no EDIT grant: want ErrForbidden, got %v", err)
	}
	if !slices.Equal(checked, []string{"AGENT_STUDIO/EDIT/agent:writer-a"}) {
		t.Fatalf("checked %v", checked)
	}
	h.guardrails.Studio.Roles = studioRoles
	h.query.rows[qStudioGetDraft] = [][]any{draftRow(true)}
	if _, err := h.guardrails.SaveGuardrails(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("archived agent: want ErrConflict, got %v", err)
	}
	if _, err := (&StudioGuardrails{Studio: h.guardrails.Studio}).SaveGuardrails(context.Background(), humanActor(8, 9), change); err == nil {
		t.Fatal("a service without a rule compiler and restriction layers must refuse")
	}
}

func TestGuardrailSectionFreezesTheTypesRulesWithTheAgents(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{})
	draft := domain.AgentDraft{AgentID: "writer-a", AgentTypeID: "writer"}
	if section, err := h.guardrails.Section(context.Background(), 8, draft); err != nil || section != nil {
		t.Fatalf("a draft without rules freezes no section: %s, %v", section, err)
	}
	h.typeRules = []domain.GuardrailRule{gRule("type.first", domain.GuardrailKindMalware, domain.GuardrailActionBlock, ""), typeSize}
	h.query.rows[qGuardrailDraftGet] = storedRules(t, ownPhrase, tighterSize)
	raw, err := h.guardrails.Section(context.Background(), 8, draft)
	if err != nil {
		t.Fatal(err)
	}
	var section guardrailSection
	if err = json.Unmarshal(raw, &section); err != nil {
		t.Fatal(err)
	}
	var set domain.GuardrailRuleSet
	if err = json.Unmarshal(section.Rules, &set); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, rule := range set.Rules {
		ids = append(ids, rule.ID)
	}
	if !slices.Equal(ids, []string{"type.first", "type.size", "own.codename"}) || string(set.Rules[1].Params) != `{"max":500}` ||
		section.RulesDigest != guardrail.Digest(section.Rules) {
		t.Fatalf("section = %s", raw)
	}
	h.query.rows[qGuardrailDraftGet][0][1] = strings.Repeat("0", 64)
	if _, err = h.guardrails.Section(context.Background(), 8, draft); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stored rules that do not match their digest: want ErrConflict, got %v", err)
	}
}

func publishRows() map[string][][]any {
	return map[string][][]any{
		qStudioGetDraft:    {draftRow(false)},
		qStudioLockDraft:   {{int64(4), "writer"}},
		qStudioLockAlias:   {{"writer-a", int64(2)}},
		qStudioNextVersion: {{int64(1)}},
		qGuardrailPin:      {{"1"}},
	}
}

var publishRequest = domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}

// releaseConfig makes the fake store answer the read-back of the rule set a release freezes.
func releaseConfig(t *testing.T, query *studioQueryFake, rules ...domain.GuardrailRule) string {
	t.Helper()
	envelope, digest, _, err := canonicalRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	query.rows[qGuardrailGet] = [][]any{{digest, string(envelope), digest}}
	return digest
}

func TestPublishFreezesTheGuardrailsAndNamesThemInTheSameTransaction(t *testing.T) {
	h := newGuardrailHarness(t, publishRows())
	h.typeRules = []domain.GuardrailRule{typeSize}
	h.layers = tenantLayer(tenantPII)
	h.query.rows[qGuardrailDraftGet] = storedRules(t, tighterSize, ownPhrase)
	digest := releaseConfig(t, h.query, tighterSize, ownPhrase)
	h.query.rows[qStudioReleaseGuardrail] = [][]any{{digest}}

	release, err := h.guardrails.Studio.Publish(context.Background(), humanActor(8, 9), publishRequest)
	if err != nil {
		t.Fatal(err)
	}
	var definition domain.AgentDefinition
	if err = json.Unmarshal([]byte(h.query.args[qStudioInsertVersion][3].(string)), &definition); err != nil {
		t.Fatal(err)
	}
	var section guardrailSection
	if err = json.Unmarshal(definition.Sections[GuardrailSectionName], &section); err != nil || section.RulesDigest != digest {
		t.Fatalf("the definition names digest %q, want %q (%v)", section.RulesDigest, digest, err)
	}
	if release.DefinitionDigest != definition.DefinitionDigest {
		t.Fatalf("release digest %q, definition %q", release.DefinitionDigest, definition.DefinitionDigest)
	}
	published := h.query.args[qGuardrailPublish]
	if published[0] != int64(8) || published[1] != "writer-a" || published[2] != digest || published[3] != string(section.Rules) || published[4] != digest {
		t.Fatalf("guardrail config args = %v", published)
	}
	if pin := h.query.args[qGuardrailPin]; pin[0] != digest || pin[1] != int64(8) || pin[2] != "writer-a" || pin[3] != "1" {
		t.Fatalf("pin args = %v", pin)
	}
	order := func(name string) int { return slices.Index(h.query.queries, name) }
	if !(order(qStudioInsertVersion) < order(qGuardrailPublish) && order(qGuardrailPublish) < order(qGuardrailPin)) || h.query.commits != 1 {
		t.Fatalf("queries = %v, commits = %d", h.query.queries, h.query.commits)
	}
}

func TestPublishWithoutRulesNamesTheEmptyRuleSet(t *testing.T) {
	h := newGuardrailHarness(t, publishRows())
	digest := releaseConfig(t, h.query)
	if _, err := h.guardrails.Studio.Publish(context.Background(), humanActor(8, 9), publishRequest); err != nil {
		t.Fatal(err)
	}
	var definition domain.AgentDefinition
	if err := json.Unmarshal([]byte(h.query.args[qStudioInsertVersion][3].(string)), &definition); err != nil || definition.Sections != nil {
		t.Fatalf("a release without rules freezes no section, so its digest is unchanged: %v, %v", definition.Sections, err)
	}
	if h.query.args[qGuardrailPublish][3] != `{"rules":[],"schema_version":1}` || h.query.args[qGuardrailPin][0] != digest {
		t.Fatalf("config = %v, pin = %v", h.query.args[qGuardrailPublish], h.query.args[qGuardrailPin])
	}
}

func TestPublishRefusesAReleaseThatLoosensWhatTheAgentInherits(t *testing.T) {
	h := newGuardrailHarness(t, publishRows())
	// The rules were saved under a laxer type; the type has since tightened.
	h.typeRules = []domain.GuardrailRule{gRule("type.size", domain.GuardrailKindMaxOutputBytes, domain.GuardrailActionBlock, `{"max":100}`)}
	h.query.rows[qGuardrailDraftGet] = storedRules(t, tighterSize)
	releaseConfig(t, h.query, tighterSize)
	if _, err := h.guardrails.Studio.Publish(context.Background(), humanActor(8, 9), publishRequest); !errors.Is(err, domain.ErrAuthorityExceeded) || h.query.commits != 0 {
		t.Fatalf("error = %v, commits = %d", err, h.query.commits)
	}
	if slices.Contains(h.query.queries, qGuardrailPublish) {
		t.Fatal("a refused release must not write its guardrail config")
	}

	h = newGuardrailHarness(t, publishRows())
	h.layers = tenantLayer(tenantPII)
	h.query.rows[qGuardrailDraftGet] = storedRules(t, gRule("pii", domain.GuardrailKindPII, domain.GuardrailActionFlag, ""))
	if _, err := h.guardrails.Studio.Publish(context.Background(), humanActor(8, 9), publishRequest); !errors.Is(err, domain.ErrAuthorityExceeded) || h.query.commits != 0 {
		t.Fatalf("a tenant rule tightened since the save: error = %v, commits = %d", err, h.query.commits)
	}
}

func TestPublishRefusesWhenTheGuardrailsSectionIsNotComposed(t *testing.T) {
	h := newGuardrailHarness(t, publishRows())
	h.guardrails.Studio.Sections = nil
	h.query.rows[qGuardrailDraftGet] = storedRules(t, ownPhrase)
	releaseConfig(t, h.query)
	if _, err := h.guardrails.Studio.Publish(context.Background(), humanActor(8, 9), publishRequest); !errors.Is(err, domain.ErrNotReady) || h.query.commits != 0 {
		t.Fatalf("error = %v, commits = %d", err, h.query.commits)
	}
}

// A release that freezes guardrails no composed writer pins would run without them, so it is refused.
func TestPublishRefusesFrozenGuardrailsNoWriterNames(t *testing.T) {
	h := newGuardrailHarness(t, publishRows())
	h.typeRules = []domain.GuardrailRule{typeSize}
	h.guardrails.Studio.ReleaseWriters = []ReleaseWriter{releaseWriterFunc(func(context.Context, keelport.TxQueryService, int64, domain.AgentDefinition) error { return nil })}
	if _, err := h.guardrails.Studio.Publish(context.Background(), humanActor(8, 9), publishRequest); !errors.Is(err, domain.ErrNotReady) || h.query.commits != 0 {
		t.Fatalf("error = %v, commits = %d", err, h.query.commits)
	}
	if args := h.query.args[qStudioReleaseGuardrail]; len(args) != 3 || args[0] != int64(8) || args[1] != "writer-a" || args[2] != "1" {
		t.Fatalf("pin read args = %v", args)
	}
	h.query.rows[qStudioReleaseGuardrail] = [][]any{{strings.Repeat("e", 64)}}
	h.query.commits = 0
	if _, err := h.guardrails.Studio.Publish(context.Background(), humanActor(8, 9), publishRequest); !errors.Is(err, domain.ErrNotReady) || h.query.commits != 0 {
		t.Fatalf("a release naming another version: error = %v, commits = %d", err, h.query.commits)
	}
}

type releaseWriterFunc func(context.Context, keelport.TxQueryService, int64, domain.AgentDefinition) error

func (function releaseWriterFunc) WriteRelease(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	return function(ctx, tx, tenantID, definition)
}

// The type's rules exist only in a release's own rule set, so a restore must still carry them.
func TestWriteReleaseOfARestoreMustCarryTheTypesCurrentRules(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{qGuardrailPin: {{"7"}}})
	h.layersErr = errors.New("a restore never reads the layers")
	h.typeRules = []domain.GuardrailRule{typeSize}
	releaseConfig(t, h.query)
	sectionless := domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Version: "7", RestoredFromVersion: "2"}
	tx := catalogTxFake{h.query}
	if err := h.guardrails.WriteRelease(context.Background(), tx, 8, sectionless); !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("a source without the type's rule: want ErrAuthorityExceeded, got %v", err)
	}
	frozen := func(rules ...domain.GuardrailRule) domain.AgentDefinition {
		envelope, digest, _, err := canonicalRules(rules)
		if err != nil {
			t.Fatal(err)
		}
		section, _ := json.Marshal(guardrailSection{Rules: envelope, RulesDigest: digest})
		definition := sectionless
		definition.Sections = map[string]json.RawMessage{GuardrailSectionName: section}
		return definition
	}
	looser := gRule("type.size", domain.GuardrailKindMaxOutputBytes, domain.GuardrailActionBlock, `{"max":5000}`)
	if err := h.guardrails.WriteRelease(context.Background(), tx, 8, frozen(looser)); !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("a source that loosens the type's rule: want ErrAuthorityExceeded, got %v", err)
	}
	if slices.Contains(h.query.queries, qGuardrailPublish) {
		t.Fatal("a refused restore must not write its guardrail config")
	}
	digest := releaseConfig(t, h.query, tighterSize, ownPhrase)
	if err := h.guardrails.WriteRelease(context.Background(), tx, 8, frozen(tighterSize, ownPhrase)); err != nil {
		t.Fatal(err)
	}
	if h.query.args[qGuardrailPin][0] != digest || len(h.tenants) != 0 {
		t.Fatalf("pin = %v, layers read for %v", h.query.args[qGuardrailPin], h.tenants)
	}
}

func TestWriteReleaseOfARestoreKeepsItsFrozenRules(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{qGuardrailPin: {{"7"}}})
	h.layersErr = errors.New("a restore never reads the layers")
	digest := releaseConfig(t, h.query, ownPhrase)
	envelope, _, _, _ := canonicalRules([]domain.GuardrailRule{ownPhrase})
	section, _ := json.Marshal(guardrailSection{Rules: envelope, RulesDigest: digest})
	definition := domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Version: "7", RestoredFromVersion: "2",
		Sections: map[string]json.RawMessage{GuardrailSectionName: section}}
	tx := catalogTxFake{h.query}
	if err := h.guardrails.WriteRelease(context.Background(), tx, 8, definition); err != nil {
		t.Fatal(err)
	}
	if h.query.args[qGuardrailPin][0] != digest || h.query.args[qGuardrailPin][3] != "7" || len(h.tenants) != 0 {
		t.Fatalf("pin = %v, layers read for %v", h.query.args[qGuardrailPin], h.tenants)
	}

	tampered := strings.Replace(string(section), "BLUEJAY", "ROBIN", 1)
	definition.Sections[GuardrailSectionName] = json.RawMessage(tampered)
	if err := h.guardrails.WriteRelease(context.Background(), tx, 8, definition); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("frozen rules that do not match their digest: want ErrConflict, got %v", err)
	}
	definition.Sections[GuardrailSectionName] = section
	h.query.rows[qGuardrailPin] = nil
	if err := h.guardrails.WriteRelease(context.Background(), tx, 8, definition); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a release that already names a version: want ErrConflict, got %v", err)
	}
	h.query.rows[qGuardrailGet] = [][]any{{digest, "{}", strings.Repeat("d", 64)}}
	if err := h.guardrails.WriteRelease(context.Background(), tx, 8, definition); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a version holding other rules: want ErrConflict, got %v", err)
	}
	var plain keelport.TxQueryService = struct{ keelport.TxQueryService }{}
	if err := h.guardrails.WriteRelease(context.Background(), plain, 8, definition); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a transaction without a catalog: want ErrNotReady, got %v", err)
	}
}

func TestDraftDriftNamesChangedGuardrails(t *testing.T) {
	h := newGuardrailHarness(t, publishRows())
	h.query.rows[qStudioActiveDefinition] = [][]any{{"1", `{"agent_id":"writer-a","agent_type_id":"writer","version":"1","enabled":true}`}}
	h.query.rows[qGuardrailDraftGet] = storedRules(t, ownPhrase)
	draft, err := h.guardrails.Studio.GetDraft(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Drift == nil || !slices.Contains(draft.Drift.Causes, "section:"+GuardrailSectionName) {
		t.Fatalf("drift = %+v", draft.Drift)
	}
}

func TestStudioSafetyEventsPageAViewableAgentOfTheActorsTenant(t *testing.T) {
	h := newGuardrailHarness(t, map[string][][]any{})
	var asked []any
	h.guardrails.Events = fake.SafetyEventQueryFunc(func(_ context.Context, tenantID int64, filter domain.SafetyEventFilter) ([]domain.SafetyEvent, error) {
		asked = []any{tenantID, filter}
		return []domain.SafetyEvent{{ID: 3}}, nil
	})
	filter := domain.SafetyEventFilter{AgentID: "writer-a", Before: 9, Limit: 20}
	page, err := h.guardrails.SafetyEvents(context.Background(), humanActor(8, 9), filter)
	if err != nil || len(page) != 1 || asked[0] != int64(8) || asked[1] != filter {
		t.Fatalf("page = %+v, asked = %v, error = %v", page, asked, err)
	}
	h.guardrails.Studio.Roles = fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{}, nil
	})
	asked = nil
	if _, err = h.guardrails.SafetyEvents(context.Background(), humanActor(8, 9), filter); !errors.Is(err, domain.ErrForbidden) || asked != nil {
		t.Fatalf("no VIEW grant: error = %v, asked = %v", err, asked)
	}
	h.guardrails.Events = nil
	if _, err = h.guardrails.SafetyEvents(context.Background(), humanActor(8, 9), filter); err == nil {
		t.Fatal("a service without a safety event query must refuse")
	}
}

func TestCreateCopiesTheSourceAgentsGuardrails(t *testing.T) {
	rows := createRows()
	rows[qStudioGetDraft] = [][]any{draftRow(false)}
	service, query := lifecycleService(rows)
	request := createRequest()
	request.CopyFrom = "writer-a"
	if _, err := service.Create(context.Background(), humanActor(8, 9), request); err != nil {
		t.Fatal(err)
	}
	if copied := query.args[qStudioCopyGuardrails]; len(copied) != 4 || copied[0] != createdAgentID || copied[1] != int64(9) || copied[2] != int64(8) || copied[3] != "writer-a" {
		t.Fatalf("copy args = %v", copied)
	}
}
