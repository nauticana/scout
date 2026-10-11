package policy

import (
	"errors"
	"reflect"
	"testing"

	"github.com/nauticana/scout/domain"
)

func effectiveSetting(kind domain.ResourceKind, id, value string) domain.ResourceExplanation {
	return domain.ResourceExplanation{ResourceKind: kind, ResourceID: id, Value: []byte(value)}
}

func onlySearch() domain.GuardrailRule {
	return domain.GuardrailRule{ID: "only-search", Kind: domain.GuardrailKindToolAllowlist, Action: domain.GuardrailActionBlock,
		Severity: domain.GuardrailSeverityHard, Params: []byte(`{"tools":["search"]}`)}
}

func TestRestrictionOverridesNameWhatEachRestrictionTakesAway(t *testing.T) {
	effective := []domain.ResourceExplanation{
		effectiveSetting(domain.ResourceBudget, "budget", `{"tokens":100,"cost_minor_units":0,"period":"turn"}`),
		effectiveSetting(domain.ResourceModel, "models", `["vendor/model-a"]`),
		effectiveSetting(domain.ResourcePolicy, "default", `[
			{"id":"day-search","effect":"allow","actions":["tool:invoke"],"resources":["search"],"conditions":{"shift":"day"}},
			{"id":"pay","effect":"allow","actions":["tool:invoke"],"resources":["payments.*"]},
			{"id":"read","effect":"allow","actions":["tool:invoke"],"resources":["search"]},
			{"id":"refund-stop","effect":"deny","actions":["tool:invoke"],"resources":["payments.refund"]}]`),
		effectiveSetting(domain.ResourceTool, "tools", `["payments.refund","payments.status","search"]`),
	}
	refunds := domain.PolicyStatement{ID: "no-refunds", Effect: domain.PolicyDeny, Actions: []string{"tool:*"}, Resources: []string{"payments.refund"}}
	exports := domain.PolicyStatement{ID: "no-export", Effect: domain.PolicyDeny, Actions: []string{"data:export"}, Resources: []string{"*"}}
	nights := domain.PolicyStatement{ID: "no-night-search", Effect: domain.PolicyDeny, Actions: []string{"tool:invoke"}, Resources: []string{"search"},
		Conditions: []byte(`{"shift":"night"}`)}
	layers := domain.RestrictionLayers{
		Platform: domain.RestrictionLayer{Digest: "platform", Denials: []domain.PolicyStatement{exports, refunds}},
		Tenant:   domain.RestrictionLayer{Digest: "tenant", Denials: []domain.PolicyStatement{nights}, Guardrails: []domain.GuardrailRule{noCodename(), onlySearch()}},
	}
	overrides, err := restrictionOverrides(layers, effective)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.RestrictionOverride{
		{Layer: domain.RestrictionPlatform, Kind: domain.RestrictionDenial, RestrictionID: "no-refunds", Settings: []domain.RestrictedSetting{
			{ResourceKind: domain.ResourcePolicy, ResourceID: "default", Members: []string{"pay"}},
			{ResourceKind: domain.ResourceTool, ResourceID: "tools", Members: []string{"payments.refund"}},
		}},
		// day-search can never meet a night-only denial; the agent's own deny is not overridden.
		{Layer: domain.RestrictionTenant, Kind: domain.RestrictionDenial, RestrictionID: "no-night-search", Conditional: true, Settings: []domain.RestrictedSetting{
			{ResourceKind: domain.ResourcePolicy, ResourceID: "default", Members: []string{"read"}},
			{ResourceKind: domain.ResourceTool, ResourceID: "tools", Members: []string{"search"}},
		}},
		{Layer: domain.RestrictionTenant, Kind: domain.RestrictionGuardrail, RestrictionID: "only-search", Settings: []domain.RestrictedSetting{
			{ResourceKind: domain.ResourceTool, ResourceID: "tools", Members: []string{"payments.refund", "payments.status"}},
		}},
	}
	if !reflect.DeepEqual(overrides, want) {
		t.Fatalf("overrides =\n%+v\nwant\n%+v", overrides, want)
	}
}

func TestRestrictionOverridesOfNothing(t *testing.T) {
	layers := domain.RestrictionLayers{Tenant: domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("a")}, Guardrails: []domain.GuardrailRule{onlySearch()}}}
	overrides, err := restrictionOverrides(layers, nil)
	if err != nil || overrides == nil || len(overrides) != 0 {
		t.Fatalf("no settings: %+v, %v", overrides, err)
	}
	// A denial of other actions than tool calls leaves a tool set alone.
	reads := domain.PolicyStatement{ID: "no-reads", Effect: domain.PolicyDeny, Actions: []string{"knowledge:read"}, Resources: []string{"*"}}
	tools := []domain.ResourceExplanation{effectiveSetting(domain.ResourceTool, "tools", `["search"]`)}
	overrides, err = restrictionOverrides(domain.RestrictionLayers{Platform: domain.RestrictionLayer{Denials: []domain.PolicyStatement{reads}}}, tools)
	if err != nil || len(overrides) != 0 {
		t.Fatalf("an unrelated denial: %+v, %v", overrides, err)
	}
}

func TestRestrictionOverridesReportAnUndecodableSetting(t *testing.T) {
	layers := domain.RestrictionLayers{Tenant: domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("a")}}}
	for name, setting := range map[string]domain.ResourceExplanation{
		"tool set":   effectiveSetting(domain.ResourceTool, "tools", `{"search":true}`),
		"policy":     effectiveSetting(domain.ResourcePolicy, "default", `{"id":"x"}`),
		"conditions": effectiveSetting(domain.ResourcePolicy, "default", `[{"id":"x","effect":"allow","actions":["*"],"resources":["*"],"conditions":[1]}]`),
	} {
		if _, err := restrictionOverrides(layers, []domain.ResourceExplanation{setting}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	// A restriction that does not decode is the layer's fault, not the agent's settings'.
	conditioned := denial("a")
	conditioned.Conditions = []byte(`[1]`)
	allowlist := onlySearch()
	allowlist.Params = []byte(`[1]`)
	tools := []domain.ResourceExplanation{effectiveSetting(domain.ResourceTool, "tools", `["search"]`)}
	for name, layer := range map[string]domain.RestrictionLayer{
		"denial conditions": {Denials: []domain.PolicyStatement{conditioned}},
		"guardrail params":  {Guardrails: []domain.GuardrailRule{allowlist}},
	} {
		_, err := restrictionOverrides(domain.RestrictionLayers{Platform: layer}, tools)
		if !errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrConflict alone, got %v", name, err)
		}
	}
}

func TestPatternsOverlapFollowTheMatchingRule(t *testing.T) {
	for _, test := range []struct {
		left, right []string
		want        bool
	}{
		{[]string{"tool:invoke"}, []string{"tool:invoke"}, true},
		{[]string{"tool:invoke"}, []string{"tool:read"}, false},
		{[]string{"tool:*"}, []string{"tool:invoke"}, true},
		{[]string{"tool:invoke"}, []string{"tool:*"}, true},
		{[]string{"tool:*"}, []string{"to*"}, true},
		{[]string{"tool:*"}, []string{"data:*"}, false},
		{[]string{"*"}, []string{"anything"}, true},
		{[]string{"payments.*"}, []string{"payment"}, false},
		{[]string{"a", "payments.refund"}, []string{"b", "payments.*"}, true},
		{nil, []string{"*"}, false},
	} {
		if got := patternsOverlap(test.left, test.right); got != test.want {
			t.Fatalf("patternsOverlap(%v, %v) = %v", test.left, test.right, got)
		}
		if got := patternsOverlap(test.right, test.left); got != test.want {
			t.Fatalf("overlap must be symmetric: %v, %v", test.right, test.left)
		}
	}
	// On a concrete value the overlap is exactly the evaluator's match.
	for _, pattern := range []string{"*", "pay*", "payments.refund", "payments.*", "refund"} {
		if patternsOverlap([]string{pattern}, []string{"payments.refund"}) != matchesAny([]string{pattern}, "payments.refund") {
			t.Fatalf("pattern %q disagrees with matchesAny", pattern)
		}
	}
}
