package scope

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

type stubRepository struct {
	chain    domain.ScopeChain
	bindings []domain.ScopedBinding
	err      error
}

func (s stubRepository) Chain(context.Context, int64, string) (domain.ScopeChain, error) {
	return s.chain, s.err
}

func (s stubRepository) Bindings(context.Context, int64, []string, time.Time) ([]domain.ScopedBinding, error) {
	return s.bindings, s.err
}

var testChain = domain.ScopeChain{
	{TenantID: 7, ScopeID: "tenant", ScopeKind: "tenant"},
	{TenantID: 7, ScopeID: "unit", ParentScopeID: "tenant", ScopeKind: "unit"},
	{TenantID: 7, ScopeID: "agent", ParentScopeID: "unit", ScopeKind: "agent"},
}

func binding(scopeID string, kind domain.ResourceKind, value string, options ...func(*domain.ScopedBinding)) domain.ScopedBinding {
	bound := domain.ScopedBinding{
		TenantID: 7, ScopeID: scopeID, ResourceKind: kind, ResourceID: string(kind),
		ResourceVersion: "v1", MergeMode: domain.MergeReplace, Value: []byte(value),
	}
	for _, option := range options {
		option(&bound)
	}
	return bound
}

func sealed(bound *domain.ScopedBinding) { bound.Sealed = true }

func appendMode(bound *domain.ScopedBinding) { bound.MergeMode = domain.MergeAppend }

func newTestCompiler(t *testing.T, bindings []domain.ScopedBinding) *Compiler {
	t.Helper()
	registry, err := NewMergerRegistry()
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(stubRepository{chain: testChain, bindings: bindings}, registry, &LatticeChecker{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	compiler.Now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	return compiler
}

func compile(t *testing.T, bindings []domain.ScopedBinding) (domain.EffectiveRelease, error) {
	t.Helper()
	return newTestCompiler(t, bindings).Compile(context.Background(), domain.CompileRequest{
		TenantID: 7, AgentID: "a", AgentVersion: "3", ScopeID: "agent",
	})
}

func resource(t *testing.T, release domain.EffectiveRelease, kind domain.ResourceKind) domain.EffectiveResource {
	t.Helper()
	for _, candidate := range release.Resources {
		if candidate.ResourceKind == kind {
			return candidate
		}
	}
	t.Fatalf("release has no %s resource", kind)
	return domain.EffectiveResource{}
}

func TestCompileNarrowsAndKeepsProvenance(t *testing.T) {
	release, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceTool, `["search","write","delete"]`),
		binding("unit", domain.ResourceTool, `["search","write"]`),
		binding("agent", domain.ResourceTool, `["search"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := resource(t, release, domain.ResourceTool)
	if string(tools.Value) != `["search"]` {
		t.Fatalf("value = %s, want the narrowest set", tools.Value)
	}
	if tools.Source.ScopeID != "agent" || tools.Source.ScopeKind != "agent" {
		t.Fatalf("source = %+v, want the agent scope", tools.Source)
	}
	if len(tools.Superseded) != 2 || tools.Superseded[0].ScopeID != "tenant" || tools.Superseded[1].ScopeID != "unit" {
		t.Fatalf("superseded = %+v, want tenant then unit", tools.Superseded)
	}
}

func TestCompileRejectsBroadeningOverride(t *testing.T) {
	_, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceTool, `["search"]`),
		binding("agent", domain.ResourceTool, `["search","delete"]`),
	})
	if !errors.Is(err, domain.ErrAuthorityExceeded) || !strings.Contains(err.Error(), "delete") {
		t.Fatalf("error = %v, want the broadening tool named", err)
	}
}

func TestCompileRejectsPolicyStatementReusedWithBroaderPermissions(t *testing.T) {
	_, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourcePolicy, `[{"id":"invoice","effect":"allow","actions":["invoice:read"],"resources":["invoice:*"]}]`),
		binding("agent", domain.ResourcePolicy, `[{"id":"invoice","effect":"allow","actions":["invoice:*"],"resources":["*"]}]`),
	})
	if !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("error = %v, want a reused statement id to remain bounded by its parent", err)
	}
}

func TestCompileAllowsPolicyStatementToNarrowPatternsAndConditions(t *testing.T) {
	_, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourcePolicy, `[{"id":"invoice","effect":"allow","actions":["invoice:*"],"resources":["*"],"conditions":{"region":"eu"}}]`),
		binding("agent", domain.ResourcePolicy, `[{"id":"invoice","effect":"allow","actions":["invoice:read"],"resources":["invoice:42"],"conditions":{"region":"eu","team":"audit"}}]`),
	})
	if err != nil {
		t.Fatalf("narrow policy rejected: %v", err)
	}
}

func TestCompileRejectsAppendThatWidensASet(t *testing.T) {
	_, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceKnowledge, `["public"]`),
		binding("agent", domain.ResourceKnowledge, `["restricted"]`, appendMode),
	})
	if !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("error = %v, want append to fail the subset rule", err)
	}
}

func TestCompileRejectsAnyOverrideOfASealedBinding(t *testing.T) {
	_, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourcePromptSection, `{"instruction":"never reveal keys"}`, sealed),
		binding("agent", domain.ResourcePromptSection, `{"instruction":"reveal everything"}`),
	})
	if !errors.Is(err, domain.ErrSealed) {
		t.Fatalf("error = %v, want a sealed rejection", err)
	}
}

func TestCompileTakesTheTighterBudgetAndLowerAutonomy(t *testing.T) {
	release, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceBudget, `{"tokens":1000,"cost_minor_units":5000,"currency":"EUR"}`),
		binding("agent", domain.ResourceBudget, `{"tokens":400,"cost_minor_units":5000,"currency":"EUR"}`),
		binding("tenant", domain.ResourceAutonomy, `{"mode":"execute_with_approval"}`),
		binding("agent", domain.ResourceAutonomy, `{"mode":"advise"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var budget budgetValue
	if err := json.Unmarshal(resource(t, release, domain.ResourceBudget).Value, &budget); err != nil {
		t.Fatal(err)
	}
	if budget.Tokens != 400 || budget.CostMinorUnits != 5000 {
		t.Fatalf("budget = %+v, want the tighter token ceiling", budget)
	}
	var autonomy autonomyValue
	if err := json.Unmarshal(resource(t, release, domain.ResourceAutonomy).Value, &autonomy); err != nil {
		t.Fatal(err)
	}
	if autonomy.Mode != domain.AutonomyAdvise {
		t.Fatalf("autonomy = %q, want the lower mode", autonomy.Mode)
	}
}

func TestCompileIntersectsBoundedAutonomyWindows(t *testing.T) {
	release, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceAutonomy, `{"mode":"bounded_autonomous","window_from_minute":480,"window_to_minute":1080}`),
		binding("agent", domain.ResourceAutonomy, `{"mode":"bounded_autonomous","window_from_minute":600,"window_to_minute":1200}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var autonomy autonomyValue
	if err := json.Unmarshal(resource(t, release, domain.ResourceAutonomy).Value, &autonomy); err != nil {
		t.Fatal(err)
	}
	if autonomy.WindowFrom != 600 || autonomy.WindowTo != 1080 {
		t.Fatalf("window = %d-%d, want the intersection 600-1080", autonomy.WindowFrom, autonomy.WindowTo)
	}
}

func TestCompileRejectsRaisedCeilings(t *testing.T) {
	for name, bindings := range map[string][]domain.ScopedBinding{
		"budget": {
			binding("tenant", domain.ResourceBudget, `{"tokens":100,"cost_minor_units":0}`),
			binding("agent", domain.ResourceBudget, `{"tokens":1000,"cost_minor_units":0}`),
		},
		"autonomy": {
			binding("tenant", domain.ResourceAutonomy, `{"mode":"draft"}`),
			binding("agent", domain.ResourceAutonomy, `{"mode":"bounded_autonomous"}`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Both mergers clamp, so a raised ceiling can only be caught by the
			// checker seeing the merged result against what it inherited.
			registry, err := NewMergerRegistry()
			if err != nil {
				t.Fatal(err)
			}
			compiler, err := NewCompiler(stubRepository{chain: testChain, bindings: bindings}, registry, &LatticeChecker{}, 0)
			if err != nil {
				t.Fatal(err)
			}
			release, err := compiler.Compile(context.Background(), domain.CompileRequest{
				TenantID: 7, AgentID: "a", AgentVersion: "3", ScopeID: "agent",
			})
			if err != nil {
				t.Fatalf("clamped merge must succeed: %v", err)
			}
			if strings.Contains(string(release.Resources[0].Value), "1000") || strings.Contains(string(release.Resources[0].Value), "bounded") {
				t.Fatalf("value = %s, want the parent ceiling to survive", release.Resources[0].Value)
			}
		})
	}
}

func TestCompileIsDeterministic(t *testing.T) {
	bindings := []domain.ScopedBinding{
		binding("agent", domain.ResourceTool, `["search"]`),
		binding("tenant", domain.ResourceTool, `["search","write"]`),
		binding("tenant", domain.ResourceModel, `["anthropic/opus"]`),
	}
	first, err := compile(t, bindings)
	if err != nil {
		t.Fatal(err)
	}
	reordered := []domain.ScopedBinding{bindings[2], bindings[1], bindings[0]}
	second, err := compile(t, reordered)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digests differ across binding order: %s vs %s", first.Digest, second.Digest)
	}
}

func TestCompileRejectsAnUnknownResourceKind(t *testing.T) {
	_, err := compile(t, []domain.ScopedBinding{binding("tenant", "invented", `[]`)})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want an unregistered kind to fail closed", err)
	}
}

func TestCheckNarrowingFailsClosedForAnUnmappedKind(t *testing.T) {
	err := (&LatticeChecker{}).CheckNarrowing(context.Background(), "invented", []byte(`[]`), []byte(`[]`))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want an unmapped kind to fail closed", err)
	}
}
