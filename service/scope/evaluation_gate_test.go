package scope

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestEvaluationGateCanonicalValueSortsScopesAndSpellsEveryField(t *testing.T) {
	first, err := CanonicalValue(domain.ResourceEvaluationGate, []byte(`{"required_scopes":["gate","dev"],"metric":"rubric_score","min_score_bp":8000}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"metric":"rubric_score","min_score_bp":8000,"min_samples":0,"required_scopes":["dev","gate"]}`
	if string(first) != want {
		t.Fatalf("canonical = %s, want %s", first, want)
	}
	bounded, err := CanonicalValue(domain.ResourceEvaluationGate, []byte(`{"metric":"m","max_regression_bp":0,"required_scopes":["dev"]}`))
	if err != nil || !strings.Contains(string(bounded), `"max_regression_bp":0`) {
		t.Fatalf("a zero regression bound is a bound: %s, %v", bounded, err)
	}
}

func TestEvaluationGateCanonicalValueRefusesMalformedGates(t *testing.T) {
	for name, raw := range map[string]string{
		"no metric":        `{"required_scopes":["dev"]}`,
		"padded metric":    `{"metric":" m","required_scopes":["dev"]}`,
		"long metric":      `{"metric":"` + strings.Repeat("m", 65) + `","required_scopes":["dev"]}`,
		"score too high":   `{"metric":"m","min_score_bp":10001,"required_scopes":["dev"]}`,
		"negative score":   `{"metric":"m","min_score_bp":-1,"required_scopes":["dev"]}`,
		"regression range": `{"metric":"m","max_regression_bp":10001,"required_scopes":["dev"]}`,
		"negative samples": `{"metric":"m","min_samples":-1,"required_scopes":["dev"]}`,
		"no scopes":        `{"metric":"m","required_scopes":[]}`,
		"unknown scope":    `{"metric":"m","required_scopes":["prod"]}`,
		"duplicate scope":  `{"metric":"m","required_scopes":["dev","dev"]}`,
		"unknown field":    `{"metric":"m","required_scopes":["dev"],"sets":["a"]}`,
		"fraction":         `{"metric":"m","min_score_bp":0.5,"required_scopes":["dev"]}`,
		"empty":            ``,
	} {
		if _, err := CanonicalValue(domain.ResourceEvaluationGate, []byte(raw)); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
}

func gateBinding(scopeID, value string) domain.ScopedBinding {
	return domain.ScopedBinding{TenantID: 1, ScopeID: scopeID, ResourceKind: domain.ResourceEvaluationGate,
		ResourceID: string(domain.ResourceEvaluationGate), MergeMode: domain.MergeReplace, Value: []byte(value)}
}

func TestEvaluationGateMergerKeepsTheStricterOfEveryRequirement(t *testing.T) {
	parent := `{"metric":"m","min_score_bp":8000,"max_regression_bp":300,"min_samples":20,"required_scopes":["gate"]}`
	merged, err := EvaluationGateMerger{}.Merge(context.Background(), []byte(parent),
		gateBinding("a", `{"metric":"m","min_score_bp":7000,"max_regression_bp":500,"min_samples":50,"required_scopes":["dev"]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"metric":"m","min_score_bp":8000,"max_regression_bp":300,"min_samples":50,"required_scopes":["dev","gate"]}`
	if string(merged) != want {
		t.Fatalf("merged = %s, want %s", merged, want)
	}
	unbounded, err := EvaluationGateMerger{}.Merge(context.Background(), []byte(parent), gateBinding("a", `{"metric":"m","required_scopes":["gate"]}`))
	if err != nil || !strings.Contains(string(unbounded), `"max_regression_bp":300`) {
		t.Fatalf("a child must not unset the inherited regression bound: %s, %v", unbounded, err)
	}
	if _, err = (EvaluationGateMerger{}).Merge(context.Background(), []byte(parent), gateBinding("a", `{"metric":"other","required_scopes":["gate"]}`)); !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("changing the metric = %v, want authority exceeded", err)
	}
	if _, err = (EvaluationGateMerger{}).Merge(context.Background(), nil, domain.ScopedBinding{MergeMode: domain.MergeAppend, Value: []byte(parent)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("append = %v, want validation", err)
	}
}

func TestEvaluationGateNarrowingRefusesEveryLoosening(t *testing.T) {
	checker := &LatticeChecker{}
	parent := []byte(`{"metric":"m","min_score_bp":8000,"max_regression_bp":300,"min_samples":20,"required_scopes":["gate"]}`)
	for name, candidate := range map[string]string{
		"lower score":       `{"metric":"m","min_score_bp":7999,"max_regression_bp":300,"min_samples":20,"required_scopes":["gate"]}`,
		"higher regression": `{"metric":"m","min_score_bp":8000,"max_regression_bp":301,"min_samples":20,"required_scopes":["gate"]}`,
		"unset regression":  `{"metric":"m","min_score_bp":8000,"min_samples":20,"required_scopes":["gate"]}`,
		"fewer samples":     `{"metric":"m","min_score_bp":8000,"max_regression_bp":300,"min_samples":19,"required_scopes":["gate"]}`,
		"dropped scope":     `{"metric":"m","min_score_bp":8000,"max_regression_bp":300,"min_samples":20,"required_scopes":["dev"]}`,
		"other metric":      `{"metric":"n","min_score_bp":8000,"max_regression_bp":300,"min_samples":20,"required_scopes":["gate"]}`,
	} {
		if err := checker.CheckNarrowing(context.Background(), domain.ResourceEvaluationGate, parent, []byte(candidate)); !errors.Is(err, domain.ErrAuthorityExceeded) {
			t.Errorf("%s: error = %v, want authority exceeded", name, err)
		}
	}
	tighter := `{"metric":"m","min_score_bp":9000,"max_regression_bp":0,"min_samples":30,"required_scopes":["dev","gate"]}`
	if err := checker.CheckNarrowing(context.Background(), domain.ResourceEvaluationGate, parent, []byte(tighter)); err != nil {
		t.Fatalf("a tighter gate = %v", err)
	}
}

func TestCompileLetsAnAgentOnlyTightenItsTypeGate(t *testing.T) {
	release, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceEvaluationGate, `{"metric":"m","min_score_bp":8000,"required_scopes":["dev"]}`),
		binding("agent", domain.ResourceEvaluationGate, `{"metric":"m","min_score_bp":5000,"required_scopes":["gate"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DecodeEvaluationGate(resource(t, release, domain.ResourceEvaluationGate).Value)
	if err != nil || policy.MinScoreBP != 8000 || len(policy.RequiredScopes) != 2 {
		t.Fatalf("effective gate = %+v, %v", policy, err)
	}
	first, err := EvaluationGateDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.RequiredScopes = []domain.GoldenScope{domain.GoldenScopeGate, domain.GoldenScopeDev}
	if second, _ := EvaluationGateDigest(policy); second != first || len(first) != 64 {
		t.Fatalf("digest depends on scope order: %s vs %s", first, second)
	}
}
