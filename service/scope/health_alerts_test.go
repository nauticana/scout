package scope

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestHealthAlertsCanonicalValueSpellsEveryField(t *testing.T) {
	canonical, err := CanonicalValue(domain.ResourceHealthAlerts, []byte(`{"max_silence_minutes":120,"min_turns":20,"max_failure_rate_bp":500}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"window_minutes":0,"min_turns":20,"max_failure_rate_bp":500,"max_p95_latency_ms":0,"max_hard_safety_events":0,"max_silence_minutes":120,"max_unresolved_minutes":0}`
	if string(canonical) != want {
		t.Fatalf("canonical = %s, want %s", canonical, want)
	}
}

func TestHealthAlertsCanonicalValueRefusesMalformedPolicies(t *testing.T) {
	for name, raw := range map[string]string{
		"negative threshold":  `{"max_failure_rate_bp":-1}`,
		"rate above whole":    `{"max_failure_rate_bp":10001}`,
		"window too long":     `{"window_minutes":10081}`,
		"silence too long":    `{"max_silence_minutes":10081}`,
		"unresolved too long": `{"max_unresolved_minutes":10081}`,
		"negative unresolved": `{"max_unresolved_minutes":-1}`,
		"latency too long":    `{"max_p95_latency_ms":86400001}`,
		"negative floor":      `{"min_turns":-5}`,
		"fraction":            `{"max_p95_latency_ms":1.5}`,
		"unknown field":       `{"max_failure_rate_bp":5,"max_cost":3}`,
		"trailing content":    `{"max_failure_rate_bp":5} {}`,
		"not an object":       `[1]`,
		"empty":               ``,
	} {
		if _, err := CanonicalValue(domain.ResourceHealthAlerts, []byte(raw)); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
}

func alertsBinding(value string) domain.ScopedBinding {
	return domain.ScopedBinding{TenantID: 7, ScopeID: "agent", ResourceKind: domain.ResourceHealthAlerts,
		ResourceID: string(domain.ResourceHealthAlerts), MergeMode: domain.MergeReplace, Value: []byte(value)}
}

func TestHealthAlertsMergerOnlyTightensAndZeroInherits(t *testing.T) {
	parent := `{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":500,"max_p95_latency_ms":30000,"max_hard_safety_events":0,"max_silence_minutes":120}`
	merged, err := HealthAlertsMerger{}.Merge(context.Background(), []byte(parent),
		alertsBinding(`{"min_turns":50,"max_failure_rate_bp":200,"max_hard_safety_events":3,"max_silence_minutes":0}`))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DecodeHealthAlerts(merged)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.HealthAlertPolicy{WindowMinutes: 60, MinTurns: 20, MaxFailureRateBP: 200, MaxP95LatencyMS: 30000, MaxHardSafetyEvents: 3, MaxSilenceMinutes: 120}
	if policy != want {
		t.Fatalf("merged = %+v, want %+v", policy, want)
	}
	if _, err = (HealthAlertsMerger{}).Merge(context.Background(), []byte(parent), alertsBinding(`{"window_minutes":15}`)); !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("a child may not change the inherited window: %v", err)
	}
	appended := alertsBinding(`{"max_failure_rate_bp":5}`)
	appended.MergeMode = domain.MergeAppend
	if _, err = (HealthAlertsMerger{}).Merge(context.Background(), []byte(parent), appended); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("append must be refused: %v", err)
	}
	root, err := HealthAlertsMerger{}.Merge(context.Background(), nil, alertsBinding(`{"window_minutes":15,"max_silence_minutes":30}`))
	if err != nil || string(root) != `{"window_minutes":15,"min_turns":0,"max_failure_rate_bp":0,"max_p95_latency_ms":0,"max_hard_safety_events":0,"max_silence_minutes":30,"max_unresolved_minutes":0}` {
		t.Fatalf("root policy = %s, %v", root, err)
	}
}

// The unresolved-outcome span narrows like the other thresholds: a child may lower it or set one
// its parent leaves unset, and its zero inherits; it may neither raise nor unset the parent's.
func TestHealthAlertsUnresolvedSpanOnlyTightens(t *testing.T) {
	parent := `{"window_minutes":60,"max_unresolved_minutes":30}`
	for child, want := range map[string]int64{
		`{"max_unresolved_minutes":10}`: 10,
		`{"max_unresolved_minutes":90}`: 30,
		`{"max_failure_rate_bp":5}`:     30,
	} {
		merged, err := HealthAlertsMerger{}.Merge(context.Background(), []byte(parent), alertsBinding(child))
		if err != nil {
			t.Fatal(err)
		}
		if policy, err := DecodeHealthAlerts(merged); err != nil || policy.MaxUnresolvedMinutes != want {
			t.Fatalf("%s merged to %+v, %v; want %d", child, policy, err, want)
		}
	}
	if merged, err := (HealthAlertsMerger{}).Merge(context.Background(), []byte(`{"window_minutes":60}`), alertsBinding(`{"max_unresolved_minutes":45}`)); err != nil ||
		!strings.Contains(string(merged), `"max_unresolved_minutes":45`) {
		t.Fatalf("a child sets an unresolved span its parent leaves unset: %s, %v", merged, err)
	}
	checker := &LatticeChecker{}
	for name, candidate := range map[string]string{
		"raised": `{"window_minutes":60,"max_unresolved_minutes":45}`,
		"unset":  `{"window_minutes":60}`,
	} {
		if err := checker.CheckNarrowing(context.Background(), domain.ResourceHealthAlerts, []byte(parent), []byte(candidate)); !errors.Is(err, domain.ErrAuthorityExceeded) {
			t.Errorf("%s unresolved span = %v, want authority exceeded", name, err)
		}
	}
	if policy := (domain.HealthAlertPolicy{MaxUnresolvedMinutes: 5}); !policy.Evaluates() {
		t.Fatal("an unresolved-outcome span alone must be evaluated")
	}
}

func TestHealthAlertsNarrowingRefusesAnythingThatAlertsLess(t *testing.T) {
	parent := []byte(`{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":500,"max_p95_latency_ms":30000,"max_hard_safety_events":0,"max_silence_minutes":120}`)
	checker := &LatticeChecker{}
	for name, candidate := range map[string]string{
		"raised rate":      `{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":600,"max_p95_latency_ms":30000,"max_silence_minutes":120}`,
		"unset latency":    `{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":500,"max_silence_minutes":120}`,
		"raised floor":     `{"window_minutes":60,"min_turns":21,"max_failure_rate_bp":500,"max_p95_latency_ms":30000,"max_silence_minutes":120}`,
		"longer silence":   `{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":500,"max_p95_latency_ms":30000,"max_silence_minutes":180}`,
		"changed window":   `{"window_minutes":30,"min_turns":20,"max_failure_rate_bp":500,"max_p95_latency_ms":30000,"max_silence_minutes":120}`,
		"unwindowed child": `{"min_turns":20,"max_failure_rate_bp":500,"max_p95_latency_ms":30000,"max_silence_minutes":120}`,
	} {
		if err := checker.CheckNarrowing(context.Background(), domain.ResourceHealthAlerts, parent, []byte(candidate)); !errors.Is(err, domain.ErrAuthorityExceeded) {
			t.Errorf("%s: error = %v, want authority exceeded", name, err)
		}
	}
	tighter := `{"window_minutes":60,"min_turns":10,"max_failure_rate_bp":100,"max_p95_latency_ms":20000,"max_hard_safety_events":1,"max_silence_minutes":60}`
	if err := checker.CheckNarrowing(context.Background(), domain.ResourceHealthAlerts, parent, []byte(tighter)); err != nil {
		t.Fatalf("a tighter policy must pass: %v", err)
	}
}

func TestCompileLetsAnAgentOnlyTightenItsInheritedAlerts(t *testing.T) {
	release, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceHealthAlerts, `{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":500}`),
		binding("agent", domain.ResourceHealthAlerts, `{"max_failure_rate_bp":900,"max_silence_minutes":30}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DecodeHealthAlerts(HealthAlertsValue(release.Resources))
	if err != nil || policy != (domain.HealthAlertPolicy{WindowMinutes: 60, MinTurns: 20, MaxFailureRateBP: 500, MaxSilenceMinutes: 30}) {
		t.Fatalf("effective policy = %+v, %v", policy, err)
	}
	if _, err = compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceHealthAlerts, `{"max_failure_rate_bp":500}`, sealed),
		binding("agent", domain.ResourceHealthAlerts, `{"max_failure_rate_bp":100}`),
	}); !errors.Is(err, domain.ErrSealed) {
		t.Fatalf("a sealed policy may not be overridden: %v", err)
	}
}

func TestWithoutOperationalSettingsFreezesNeitherGateNorAlerts(t *testing.T) {
	release, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceHealthAlerts, `{"max_failure_rate_bp":500}`),
		binding("tenant", domain.ResourceEvaluationGate, `{"metric":"m","required_scopes":["dev"]}`),
		binding("tenant", domain.ResourceLimits, `{"max_steps":4}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen := WithoutOperationalSettings(release)
	if len(frozen.Resources) != 1 || frozen.Resources[0].ResourceKind != domain.ResourceLimits || frozen.Digest == release.Digest ||
		frozen.Digest != Digest(frozen) || len(release.Resources) != 3 {
		t.Fatalf("frozen = %+v, compiled resources %d", frozen.Resources, len(release.Resources))
	}
}
