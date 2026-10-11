package policy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// settingsStudio answers Settings the way StudioService does after authorizing VIEW on the agent.
type settingsStudio struct {
	contract.AgentStudioHTTPBackend
	settings domain.AgentSettings
	err      error
	asked    []string
}

func (studio *settingsStudio) Settings(_ context.Context, actor domain.StudioActor, agentID string) (domain.AgentSettings, error) {
	studio.asked = append(studio.asked, fmt.Sprintf("%d/%s/%s", actor.TenantID, actor.Principal.ID, agentID))
	return studio.settings, studio.err
}

var restrictionAdmin = domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}

type studioRestrictionsFixture struct {
	tables  *restrictionTables
	store   *TableRestrictionLayers
	studio  *settingsStudio
	service *StudioRestrictions
	checks  []string
}

// newStudioRestrictions grants the actor the listed AGENT_RESTRICTION actions and records every check.
func newStudioRestrictions(t *testing.T, granted ...domain.RestrictionAction) *studioRestrictionsFixture {
	t.Helper()
	tables, store := newRestrictions(t)
	fixture := &studioRestrictionsFixture{tables: tables, store: store, studio: &settingsStudio{}}
	fixture.service = &StudioRestrictions{
		Layers: store, Writer: store, Studio: fixture.studio,
		Principals: fake.PrincipalResolverFunc(func(_ context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
			return domain.Principal{Kind: ref.Kind, ID: ref.ID, TenantID: tenantID}, nil
		}),
		Roles: fake.PrincipalAuthorizerFunc(func(_ context.Context, principal domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
			fixture.checks = append(fixture.checks, fmt.Sprintf("%d/%s %s %s %s", principal.TenantID, principal.ID, object, action, value))
			return domain.AuthorizationGrant{Allowed: slices.Contains(granted, domain.RestrictionAction(action))}, nil
		}),
	}
	return fixture
}

func TestStudioRestrictionsShowBothLayersOfTheActorsTenant(t *testing.T) {
	fixture := newStudioRestrictions(t, domain.RestrictionView)
	ctx := context.Background()
	platform, err := fixture.store.ReplacePlatform(ctx, platformService, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("p")}}, "")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := fixture.store.ReplaceTenant(ctx, platformService, 7, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("t")}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.ReplaceTenant(ctx, platformService, 8, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("other")}}, ""); err != nil {
		t.Fatal(err)
	}
	overview, err := fixture.service.Restrictions(ctx, restrictionAdmin, "")
	if err != nil {
		t.Fatalf("Restrictions: %v", err)
	}
	if overview.Layers.Platform.Digest != platform || overview.Layers.Tenant.Digest != tenant || overview.Layers.Tenant.Denials[0].ID != "t" ||
		overview.Overrides != nil || overview.AgentID != "" {
		t.Fatalf("overview = %+v", overview)
	}
	if want := []string{"7/42 AGENT_RESTRICTION VIEW tenant"}; !slices.Equal(fixture.checks, want) || len(fixture.studio.asked) != 0 {
		t.Fatalf("checks = %v, studio asked = %v", fixture.checks, fixture.studio.asked)
	}
}

func TestStudioRestrictionsExplainAnAgentsOverriddenSettings(t *testing.T) {
	fixture := newStudioRestrictions(t, domain.RestrictionView)
	ctx := context.Background()
	refunds := domain.PolicyStatement{ID: "no-refunds", Effect: domain.PolicyDeny, Actions: []string{domain.PolicyActionToolInvoke}, Resources: []string{"payments.*"}}
	if _, err := fixture.store.ReplaceTenant(ctx, platformService, 7, domain.RestrictionLayer{Denials: []domain.PolicyStatement{refunds}}, ""); err != nil {
		t.Fatal(err)
	}
	fixture.studio.settings = domain.AgentSettings{AgentID: "writer-a", Effective: []domain.ResourceExplanation{
		effectiveSetting(domain.ResourceTool, "tools", `["payments.refund","search"]`),
	}}
	overview, err := fixture.service.Restrictions(ctx, restrictionAdmin, "writer-a")
	if err != nil {
		t.Fatalf("Restrictions: %v", err)
	}
	want := []domain.RestrictionOverride{{Layer: domain.RestrictionTenant, Kind: domain.RestrictionDenial, RestrictionID: "no-refunds",
		Settings: []domain.RestrictedSetting{{ResourceKind: domain.ResourceTool, ResourceID: "tools", Members: []string{"payments.refund"}}}}}
	if overview.AgentID != "writer-a" || overview.SettingsProblem != "" || !reflect.DeepEqual(overview.Overrides, want) {
		t.Fatalf("overview = %+v", overview)
	}
	if !slices.Equal(fixture.studio.asked, []string{"7/42/writer-a"}) {
		t.Fatalf("the agent's settings must be read as the actor: %v", fixture.studio.asked)
	}

	fixture.studio.settings = domain.AgentSettings{Problem: "budget broadens what it inherits"}
	if overview, err = fixture.service.Restrictions(ctx, restrictionAdmin, "writer-a"); err != nil ||
		overview.SettingsProblem != "budget broadens what it inherits" || overview.Overrides != nil || overview.Layers.Tenant.Digest == "" {
		t.Fatalf("settings that do not compile: %+v, %v", overview, err)
	}
	fixture.studio.settings = domain.AgentSettings{Effective: []domain.ResourceExplanation{effectiveSetting(domain.ResourceTool, "tools", `{}`)}}
	if overview, err = fixture.service.Restrictions(ctx, restrictionAdmin, "writer-a"); err != nil || overview.SettingsProblem == "" {
		t.Fatalf("an undecodable setting is a settings problem: %+v, %v", overview, err)
	}
	fixture.studio.err = fmt.Errorf("%w: AGENT_STUDIO VIEW on agent:writer-a", domain.ErrForbidden)
	if _, err = fixture.service.Restrictions(ctx, restrictionAdmin, "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an agent the actor may not view: want ErrForbidden, got %v", err)
	}
}

func TestStudioRestrictionsReplaceTheTenantLayerAsAWhole(t *testing.T) {
	fixture := newStudioRestrictions(t, domain.RestrictionReplaceTenant)
	ctx := context.Background()
	platform, err := fixture.store.ReplacePlatform(ctx, platformService, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("p")}}, "")
	if err != nil {
		t.Fatal(err)
	}
	neighbour, err := fixture.store.ReplaceTenant(ctx, platformService, 8, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("other")}}, "")
	if err != nil {
		t.Fatal(err)
	}
	written, err := fixture.service.ReplaceTenant(ctx, restrictionAdmin, domain.RestrictionLayer{
		Denials: []domain.PolicyStatement{denial("b"), denial(" a ")}, Guardrails: []domain.GuardrailRule{onlySearch()},
	}, "")
	if err != nil {
		t.Fatalf("ReplaceTenant: %v", err)
	}
	if written.Digest == "" || written.Digest != fixture.tables.current[7] || written.Denials[0].ID != "a" || written.Denials[1].ID != "b" || len(written.Guardrails) != 1 {
		t.Fatalf("written = %+v, in force %q", written, fixture.tables.current[7])
	}
	if last := fixture.tables.audits[len(fixture.tables.audits)-1]; last != (restrictionAudit{tenant: int64(7), category: domain.DecisionCategoryRestriction, version: written.Digest, principal: "human:42"}) {
		t.Fatalf("the change must be audited as the person: %+v", last)
	}
	if !slices.Equal(fixture.checks, []string{"7/42 AGENT_RESTRICTION REPLACE_TENANT tenant"}) {
		t.Fatalf("checks = %v", fixture.checks)
	}

	narrower := domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("c")}}
	if _, err = fixture.service.ReplaceTenant(ctx, restrictionAdmin, narrower, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stale expectation: want ErrConflict, got %v", err)
	}
	replaced, err := fixture.service.ReplaceTenant(ctx, restrictionAdmin, narrower, written.Digest)
	if err != nil {
		t.Fatalf("ReplaceTenant: %v", err)
	}
	layers, err := fixture.store.Layers(ctx, 7)
	if err != nil || layers.Tenant.Digest != replaced.Digest || len(layers.Tenant.Denials) != 1 || layers.Tenant.Denials[0].ID != "c" || len(layers.Tenant.Guardrails) != 0 {
		t.Fatalf("the layer must be replaced as a whole: %+v, %v", layers.Tenant, err)
	}
	if fixture.tables.current[0] != platform || fixture.tables.current[8] != neighbour {
		t.Fatalf("only the actor's tenant layer may change: %+v", fixture.tables.current)
	}
}

func TestStudioRestrictionsRefuseWhatTheyCannotWrite(t *testing.T) {
	fixture := newStudioRestrictions(t, domain.RestrictionReplaceTenant)
	fixture.service.MaxEntries = 1
	ctx := context.Background()
	allow := denial("a")
	allow.Effect = domain.PolicyAllow
	for name, layer := range map[string]domain.RestrictionLayer{
		"too many denials":    {Denials: []domain.PolicyStatement{denial("a"), denial("b")}},
		"too many guardrails": {Guardrails: []domain.GuardrailRule{onlySearch(), noCodename()}},
		"an allow":            {Denials: []domain.PolicyStatement{allow}},
		"a flagging rule":     {Guardrails: []domain.GuardrailRule{{ID: "x", Kind: domain.GuardrailKindExactPhrase, Action: domain.GuardrailActionFlag}}},
		"an invalid rule":     {Guardrails: []domain.GuardrailRule{{ID: "x", Kind: domain.GuardrailKindToolAllowlist, Action: domain.GuardrailActionBlock, Severity: domain.GuardrailSeverityHard}}},
	} {
		if _, err := fixture.service.ReplaceTenant(ctx, restrictionAdmin, layer, ""); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	if len(fixture.tables.current) != 0 || len(fixture.tables.audits) != 0 {
		t.Fatalf("a refused layer must not be written: %+v", fixture.tables.current)
	}
}

func TestStudioRestrictionsFailClosed(t *testing.T) {
	ctx := context.Background()
	layer := domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("a")}}
	viewer := newStudioRestrictions(t, domain.RestrictionView)
	if _, err := viewer.service.ReplaceTenant(ctx, restrictionAdmin, layer, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("VIEW alone must not replace: %v", err)
	}
	nobody := newStudioRestrictions(t)
	if _, err := nobody.service.Restrictions(ctx, restrictionAdmin, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("no grant must not view: %v", err)
	}
	if len(viewer.tables.current) != 0 || nobody.tables.reads != 0 {
		t.Fatalf("a refused call must neither write nor read: %+v, %d reads", viewer.tables.current, nobody.tables.reads)
	}

	admin := newStudioRestrictions(t, domain.RestrictionView, domain.RestrictionReplaceTenant)
	for name, actor := range map[string]domain.StudioActor{
		"an agent":           {TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"}},
		"not a user account": {TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "ops"}},
		"no tenant":          {Principal: restrictionAdmin.Principal},
		"nobody":             {TenantID: 7},
	} {
		if _, err := admin.service.Restrictions(ctx, actor, ""); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s viewing: want ErrValidation, got %v", name, err)
		}
		if _, err := admin.service.ReplaceTenant(ctx, actor, layer, ""); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s replacing: want ErrValidation, got %v", name, err)
		}
	}
	if len(admin.checks) != 0 {
		t.Fatalf("an invalid actor must not reach authorization: %v", admin.checks)
	}

	admin.service.Principals = fake.PrincipalResolverFunc(func(context.Context, int64, domain.PrincipalRef) (domain.Principal, error) {
		return domain.Principal{}, domain.ErrPrincipalUnknown
	})
	if _, err := admin.service.Restrictions(ctx, restrictionAdmin, ""); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("an unresolved actor: %v", err)
	}
	admin = newStudioRestrictions(t, domain.RestrictionView)
	admin.service.Roles = fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{Allowed: true}, errors.New("grant store down")
	})
	if _, err := admin.service.Restrictions(ctx, restrictionAdmin, ""); err == nil {
		t.Fatal("an authorizer failure must refuse")
	}
	admin.service.Studio = nil
	if _, err := admin.service.Restrictions(ctx, restrictionAdmin, ""); err == nil {
		t.Fatal("a service without its dependencies must refuse")
	}
}
