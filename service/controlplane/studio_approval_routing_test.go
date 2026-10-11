package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/scope"
)

const (
	executeWithApproval = `{"mode":"execute_with_approval"}`
	payRoute            = `{"routes":[{"id":"pay","match":{"tools":["pay.*"]},"approver":{"kind":"position","ref":"controller"},"deadline_seconds":3600,` +
		`"escalation":[{"approver":{"kind":"role","ref":"cfo-office"},"extension_seconds":600}]}]}`
)

func frozenSetting(t *testing.T, kind domain.ResourceKind, value string) domain.EffectiveResource {
	t.Helper()
	canonical, err := scope.CanonicalValue(kind, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return domain.EffectiveResource{ResourceKind: kind, ResourceID: string(kind), Value: canonical}
}

func routingCandidate(tools []string, resources ...domain.EffectiveResource) domain.PublishCandidate {
	definition := domain.AgentDefinition{AgentID: "writer-a"}
	for _, tool := range tools {
		definition.Tools = append(definition.Tools, domain.ToolReference{ToolID: tool, Version: "v1"})
	}
	return domain.PublishCandidate{Actor: humanActor(8, 9), Definition: definition, Settings: domain.EffectiveRelease{Resources: resources}}
}

func knownReferences(checked *[]string) *fake.ApproverResolver {
	return &fake.ApproverResolver{CheckFunc: func(_ context.Context, tenantID int64, reference domain.ApproverReference) error {
		*checked = append(*checked, string(reference.Kind)+":"+reference.Ref)
		if tenantID != 8 {
			return domain.ErrNotFound
		}
		return nil
	}}
}

func routingFields(t *testing.T, err error) []string {
	t.Helper()
	var invalid *StudioValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want a field validation error", err)
	}
	var messages []string
	for _, field := range invalid.Fields {
		if field.Field != "approval_routing" {
			t.Fatalf("field = %+v", field)
		}
		messages = append(messages, field.Message)
	}
	return messages
}

func TestApprovalGuardIgnoresAReleaseThatNeverAsks(t *testing.T) {
	guard := &StudioApprovalRouting{}
	for name, candidate := range map[string]domain.PublishCandidate{
		"human only":       routingCandidate([]string{"pay.wire"}),
		"draft":            routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceAutonomy, `{"mode":"draft"}`)),
		"unwindowed bound": routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceAutonomy, `{"mode":"bounded_autonomous"}`)),
		"no tools":         routingCandidate(nil, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval)),
	} {
		if err := guard.CheckPublish(context.Background(), candidate); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestApprovalGuardRefusesAnUncoveredAsk(t *testing.T) {
	var checked []string
	guard := &StudioApprovalRouting{Resolver: knownReferences(&checked)}
	candidate := routingCandidate([]string{"pay.wire", "search"}, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval), frozenSetting(t, domain.ResourceApprovalRouting, payRoute))
	messages := routingFields(t, guard.CheckPublish(context.Background(), candidate))
	if len(messages) != 1 || !strings.Contains(messages[0], `"search"`) || !strings.Contains(messages[0], "autonomy") {
		t.Fatalf("messages = %v", messages)
	}
	if !slices.Equal(checked, []string{"position:controller", "role:cfo-office"}) {
		t.Fatalf("checked = %v", checked)
	}
	covered := routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval), frozenSetting(t, domain.ResourceApprovalRouting, payRoute))
	if err := guard.CheckPublish(context.Background(), covered); err != nil {
		t.Fatalf("covered: %v", err)
	}
	covered.Activation = true
	if err := guard.CheckPublish(context.Background(), covered); err != nil {
		t.Fatalf("activation: %v", err)
	}
}

func TestApprovalGuardExemptsReadOnlyToolsThroughTheRegistry(t *testing.T) {
	var checked []string
	registry := &fake.ToolRegistry{GetFunc: func(_ context.Context, tenantID int64, toolID, version string) (domain.ToolDefinition, error) {
		if tenantID != 8 || version != "v1" {
			t.Fatalf("registry read %d %s %s", tenantID, toolID, version)
		}
		return domain.ToolDefinition{ToolID: toolID, Version: version, ReadOnly: toolID == "search"}, nil
	}}
	guard := &StudioApprovalRouting{Resolver: knownReferences(&checked), Tools: registry}
	candidate := routingCandidate([]string{"pay.wire", "search"}, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval), frozenSetting(t, domain.ResourceApprovalRouting, payRoute))
	if err := guard.CheckPublish(context.Background(), candidate); err != nil {
		t.Fatalf("a read-only tool never asks: %v", err)
	}
	guard.Tools = &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{}, domain.ErrNotFound
	}}
	if err := guard.CheckPublish(context.Background(), candidate); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a registry failure fails closed: %v", err)
	}
}

func TestApprovalGuardCountsAWindowedBoundedMode(t *testing.T) {
	guard := &StudioApprovalRouting{}
	windowed := routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceAutonomy, `{"mode":"bounded_autonomous","window_from_minute":480,"window_to_minute":1020}`))
	if err := guard.CheckPublish(context.Background(), windowed); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a windowed mode asks out of hours, and nothing resolves approvers: %v", err)
	}
}

func TestApprovalGuardCoversEveryPolicyTier(t *testing.T) {
	var checked []string
	guard := &StudioApprovalRouting{Resolver: knownReferences(&checked)}
	policy := frozenSetting(t, domain.ResourcePolicy, `[{"id":"a","effect":"allow","actions":["tool:invoke"],"resources":["pay.*"],"obligations":[{"kind":"require_approval","params":{"risk_tier":"critical"}}]}]`)
	highOnly := `{"routes":[{"id":"pay","match":{"tools":["pay.*"],"sources":["policy"],"min_risk_tier":"high"},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600},` +
		`{"id":"low","match":{"tools":["pay.*"],"sources":["autonomy"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600}]}`
	if err := guard.CheckPublish(context.Background(), routingCandidate([]string{"pay.wire"}, policy, frozenSetting(t, domain.ResourceApprovalRouting, highOnly))); err != nil {
		t.Fatalf("critical is at least high: %v", err)
	}
	criticalOnly := strings.Replace(highOnly, `"min_risk_tier":"high"`, `"min_risk_tier":"critical"`, 1)
	lowPolicy := frozenSetting(t, domain.ResourcePolicy, `[{"id":"a","effect":"allow","actions":["tool:invoke"],"resources":["pay.*"],"obligations":[{"kind":"require_approval","params":{"risk_tier":"medium"}}]}]`)
	messages := routingFields(t, guard.CheckPublish(context.Background(), routingCandidate([]string{"pay.wire"}, lowPolicy, frozenSetting(t, domain.ResourceApprovalRouting, criticalOnly))))
	if len(messages) != 1 || !strings.Contains(messages[0], "medium") {
		t.Fatalf("messages = %v", messages)
	}
}

func TestApprovalGuardReadsFrozenGuardrailRules(t *testing.T) {
	envelope, digest, _, err := canonicalRules([]domain.GuardrailRule{{
		ID: "pay.approval", Kind: domain.GuardrailKindIrreversibleToolApproval, Action: domain.GuardrailActionBlock,
		Severity: domain.GuardrailSeverityHard, Params: json.RawMessage(`{"tools":["pay.wire"]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	section, err := json.Marshal(guardrailSection{Rules: envelope, RulesDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	candidate := routingCandidate([]string{"pay.wire", "search"})
	candidate.Definition.Sections = map[string]json.RawMessage{GuardrailSectionName: section}
	var checked []string
	guard := &StudioApprovalRouting{Resolver: knownReferences(&checked)}
	messages := routingFields(t, guard.CheckPublish(context.Background(), candidate))
	if len(messages) != 1 || !strings.Contains(messages[0], `"pay.wire"`) || !strings.Contains(messages[0], "guardrail") {
		t.Fatalf("messages = %v", messages)
	}
}

func TestApprovalGuardNeedsAResolverThatKnowsEveryReference(t *testing.T) {
	candidate := routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval), frozenSetting(t, domain.ResourceApprovalRouting, payRoute))
	if err := (&StudioApprovalRouting{}).CheckPublish(context.Background(), candidate); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no resolver: %v", err)
	}
	unknown := &fake.ApproverResolver{CheckFunc: func(_ context.Context, _ int64, reference domain.ApproverReference) error {
		if reference.Ref == "cfo-office" {
			return domain.ErrNotFound
		}
		return nil
	}}
	messages := routingFields(t, (&StudioApprovalRouting{Resolver: unknown}).CheckPublish(context.Background(), candidate))
	if len(messages) != 1 || !strings.Contains(messages[0], "role:cfo-office") {
		t.Fatalf("messages = %v", messages)
	}
	down := &fake.ApproverResolver{CheckFunc: func(context.Context, int64, domain.ApproverReference) error { return errors.New("directory down") }}
	err := (&StudioApprovalRouting{Resolver: down}).CheckPublish(context.Background(), candidate)
	if err == nil || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an outage is not a validation problem: %v", err)
	}
}

func TestApprovalGuardChecksTheSettingsARestoreCopies(t *testing.T) {
	var checked []string
	resources := []domain.EffectiveResource{frozenSetting(t, domain.ResourceAutonomy, executeWithApproval)}
	var read string
	guard := &StudioApprovalRouting{Resolver: knownReferences(&checked), Releases: &fake.EffectiveReleaseRepository{
		GetFunc: func(_ context.Context, tenantID int64, agentID, version string) (domain.EffectiveRelease, error) {
			read = agentID + "@" + version
			return domain.EffectiveRelease{TenantID: tenantID, AgentID: agentID, AgentVersion: version, Resources: resources}, nil
		},
	}}
	restore := routingCandidate([]string{"pay.wire"})
	restore.RestoredFrom = "2"
	if messages := routingFields(t, guard.CheckPublish(context.Background(), restore)); len(messages) != 1 || read != "writer-a@2" {
		t.Fatalf("a version frozen before routing cannot be restored: %v, read %q", messages, read)
	}
}

func TestApprovalGuardReportsAChildRouteHiddenByItsParent(t *testing.T) {
	var checked []string
	guard := &StudioApprovalRouting{Resolver: knownReferences(&checked)}
	hidden := `{"routes":[{"id":"pay","match":{"tools":["pay.*"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600},` +
		`{"id":"wire","match":{"tools":["pay.wire"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600}]}`
	resource := domain.EffectiveResource{ResourceKind: domain.ResourceApprovalRouting, ResourceID: "approval_routing", Value: []byte(hidden)}
	messages := routingFields(t, guard.CheckPublish(context.Background(), routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval), resource)))
	if len(messages) != 1 || !strings.Contains(messages[0], `"wire"`) {
		t.Fatalf("messages = %v", messages)
	}
}

func TestSaveSettingCanonicalizesAndNarrowsApprovalRouting(t *testing.T) {
	parent := setting("t:writer", domain.ResourceApprovalRouting, payRoute)
	service, query := settingsService([]domain.ScopedBinding{parent})
	change := domain.AgentSettingChange{
		AgentID: "writer-a", ScopeID: "a:writer-a", ResourceKind: domain.ResourceApprovalRouting, ResourceID: "approval_routing",
		ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2,
		Value: json.RawMessage(`{"routes":[{"id":"search","match":{"sources":["policy","autonomy"],"tools":["search"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":600}]}`),
	}
	if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), change); err != nil {
		t.Fatalf("SaveSetting: %v", err)
	}
	stored := query.args[qStudioInsertSetting][7].(string)
	if !strings.Contains(stored, `"sources":["autonomy","policy"]`) || !strings.Contains(stored, `"on_exhausted":"expire"`) {
		t.Fatalf("stored = %s", stored)
	}
	for name, value := range map[string]string{
		"longer deadline": strings.Replace(payRoute, `"deadline_seconds":3600`, `"deadline_seconds":7200`, 1),
		"other approver":  strings.Replace(payRoute, `"ref":"controller"`, `"ref":"intern"`, 1),
	} {
		refused, query := settingsService([]domain.ScopedBinding{parent})
		change.Value = json.RawMessage(value)
		if _, err := refused.SaveSetting(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrAuthorityExceeded) || slices.Contains(query.queries, qStudioInsertSetting) {
			t.Errorf("%s: %v", name, err)
		}
	}
	unreachable, query := settingsService(nil)
	change.Value = json.RawMessage(`{"routes":[{"id":"all","match":{},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":600},` +
		`{"id":"pay","match":{"tools":["pay"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":600}]}`)
	if _, err := unreachable.SaveSetting(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrValidation) || slices.Contains(query.queries, qStudioInsertSetting) {
		t.Fatalf("an unreachable route: %v", err)
	}
	change.ResourceID = "routes"
	if _, err := unreachable.SaveSetting(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("approval routing is a singleton: %v", err)
	}
}

func routingViewService(t *testing.T, bindings []domain.ScopedBinding) (*StudioApprovalRouting, *[]string) {
	t.Helper()
	service, _ := publishService()
	service.Scopes = studioScopes(bindings)
	service.Kinds = fake.KindCatalog{GetFunc: func(context.Context, string) (domain.AgentTypeDescriptor, error) {
		return domain.AgentTypeDescriptor{AgentTypeID: "writer", Tools: []domain.ToolReference{{ToolID: "pay.wire", Version: "v1"}, {ToolID: "search", Version: "v1"}}}, nil
	}}
	var checked []string
	return &StudioApprovalRouting{Studio: service, Resolver: knownReferences(&checked)}, &checked
}

func TestApprovalRoutingViewReportsOriginsAndCoverage(t *testing.T) {
	own := `{"routes":[{"id":"search","match":{"tools":["search"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":600},` +
		`{"id":"pay","match":{"tools":["pay.*"]},"approver":{"kind":"position","ref":"controller"},"deadline_seconds":600,"escalation":[{"approver":{"kind":"role","ref":"cfo-office"},"extension_seconds":600}]}]}`
	routing, _ := routingViewService(t, []domain.ScopedBinding{
		setting("t:writer", domain.ResourceApprovalRouting, payRoute),
		setting("a:writer-a", domain.ResourceApprovalRouting, own),
		setting("a:writer-a", domain.ResourceAutonomy, executeWithApproval),
	})
	view, err := routing.ApprovalRouting(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Problems) != 0 || !view.ResolverComposed || len(view.Routes) != 2 {
		t.Fatalf("view = %+v", view)
	}
	if first := view.Routes[0]; first.Route.ID != "pay" || first.ScopeID != "t:writer" || !first.Inherited || first.Route.DeadlineSeconds != 600 {
		t.Fatalf("first = %+v", first)
	}
	if second := view.Routes[1]; second.Route.ID != "search" || second.ScopeID != "a:writer-a" || second.Inherited {
		t.Fatalf("second = %+v", second)
	}
	want := []domain.ApprovalCoverage{
		{ToolID: "pay.wire", Source: domain.ApprovalSourceAutonomy, RiskTier: domain.RiskHigh, RouteID: "pay"},
		{ToolID: "search", Source: domain.ApprovalSourceAutonomy, RiskTier: domain.RiskHigh, RouteID: "search"},
	}
	if !slices.Equal(view.Coverage, want) {
		t.Fatalf("coverage = %+v", view.Coverage)
	}
}

func TestApprovalRoutingViewReportsWhatPublicationWouldRefuse(t *testing.T) {
	routing, _ := routingViewService(t, []domain.ScopedBinding{setting("a:writer-a", domain.ResourceAutonomy, executeWithApproval)})
	view, err := routing.ApprovalRouting(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || len(view.Problems) != 2 || len(view.Coverage) != 2 || view.Coverage[0].RouteID != "" {
		t.Fatalf("view = %+v, %v", view, err)
	}
	broken, _ := routingViewService(t, []domain.ScopedBinding{
		setting("t:writer", domain.ResourceApprovalRouting, payRoute),
		setting("a:writer-a", domain.ResourceApprovalRouting, strings.Replace(payRoute, `"deadline_seconds":3600`, `"deadline_seconds":7200`, 1)),
	})
	view, err = broken.ApprovalRouting(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || len(view.Problems) != 1 || !strings.Contains(view.Problems[0], "authority exceeded") || len(view.Routes) != 0 {
		t.Fatalf("a chain that does not compile: %+v, %v", view, err)
	}
}

func TestApprovalRoutingViewNeedsView(t *testing.T) {
	routing, _ := routingViewService(t, nil)
	routing.Studio.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, principal domain.Principal, _, action, _ string) (domain.AuthorizationGrant, error) {
		if principal.TenantID != 8 {
			t.Fatalf("authorized in tenant %d", principal.TenantID)
		}
		return domain.AuthorizationGrant{Allowed: action != string(domain.StudioView)}, nil
	})
	if _, err := routing.ApprovalRouting(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want forbidden", err)
	}
}

func irreversibleRule(id, tools string) domain.GuardrailRule {
	return domain.GuardrailRule{ID: id, Kind: domain.GuardrailKindIrreversibleToolApproval, Action: domain.GuardrailActionBlock,
		Severity: domain.GuardrailSeverityHard, Params: json.RawMessage(`{"tools":` + tools + `}`)}
}

func TestApprovalGuardAsksOnlyForToolsTheAgentCarries(t *testing.T) {
	envelope, digest, _, err := canonicalRules([]domain.GuardrailRule{irreversibleRule("pay.approval", `["pay.wire","crm.delete"]`)})
	if err != nil {
		t.Fatal(err)
	}
	section, err := json.Marshal(guardrailSection{Rules: envelope, RulesDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	candidate := routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceApprovalRouting, payRoute))
	candidate.Definition.Sections = map[string]json.RawMessage{GuardrailSectionName: section}
	var checked []string
	guard := &StudioApprovalRouting{Resolver: knownReferences(&checked)}
	if err := guard.CheckPublish(context.Background(), candidate); err != nil {
		t.Fatalf("a tool the agent cannot call never asks: %v", err)
	}
}

func TestApprovalGuardCountsTheBaselineAndRestrictionLayers(t *testing.T) {
	var checked []string
	var tenants []int64
	guard := &StudioApprovalRouting{
		Resolver: knownReferences(&checked),
		Baseline: domain.GuardrailRuleSet{SchemaVersion: 1, Rules: []domain.GuardrailRule{irreversibleRule("baseline.pay", `["pay.wire"]`)}},
		Restrictions: restrictionLayersFunc(func(_ context.Context, tenantID int64) (domain.RestrictionLayers, error) {
			tenants = append(tenants, tenantID)
			return domain.RestrictionLayers{Tenant: domain.RestrictionLayer{Guardrails: []domain.GuardrailRule{irreversibleRule("tenant.crm", `["crm.delete"]`)}}}, nil
		}),
	}
	candidate := routingCandidate([]string{"pay.wire", "crm.delete"}, frozenSetting(t, domain.ResourceApprovalRouting, payRoute))
	messages := routingFields(t, guard.CheckPublish(context.Background(), candidate))
	if len(messages) != 1 || !strings.Contains(messages[0], `"crm.delete"`) || !strings.Contains(messages[0], "guardrail") || !slices.Equal(tenants, []int64{8}) {
		t.Fatalf("messages = %v, tenants = %v", messages, tenants)
	}
	unrouted := routingCandidate([]string{"pay.wire"})
	if err := (&StudioApprovalRouting{Baseline: guard.Baseline}).CheckPublish(context.Background(), unrouted); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a baseline approval rule asks even when the release asks nothing: %v", err)
	}
	down := errors.New("layers down")
	guard.Restrictions = restrictionLayersFunc(func(context.Context, int64) (domain.RestrictionLayers, error) {
		return domain.RestrictionLayers{}, down
	})
	if err := guard.CheckPublish(context.Background(), candidate); !errors.Is(err, down) {
		t.Fatalf("an unreadable layer fails closed: %v", err)
	}
}

// A release that binds no routing is served by the runtime router's fallback when one is composed,
// so the guard admits it as the runtime would; a release that binds routing must still cover every ask.
func TestApprovalGuardAdmitsAnUnroutedReleaseUnderAFallback(t *testing.T) {
	unrouted := routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval))
	if err := (&StudioApprovalRouting{}).CheckPublish(context.Background(), unrouted); err == nil {
		t.Fatal("without a fallback an unrouted ask must be refused")
	}
	if err := (&StudioApprovalRouting{Fallback: true}).CheckPublish(context.Background(), unrouted); err != nil {
		t.Fatalf("a fallback routes a release that binds no routing: %v", err)
	}
	var checked []string
	routed := routingCandidate([]string{"pay.wire", "search"}, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval), frozenSetting(t, domain.ResourceApprovalRouting, payRoute))
	if err := (&StudioApprovalRouting{Fallback: true, Resolver: knownReferences(&checked)}).CheckPublish(context.Background(), routed); err == nil {
		t.Fatal("a release that binds routing is never served by the fallback")
	}
}
