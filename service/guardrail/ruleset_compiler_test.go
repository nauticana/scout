package guardrail

import (
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestCompileRestrictionCachesByLayerAndDigest(t *testing.T) {
	compiler, err := NewRuleSetCompiler(CompilerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	layer := domain.RestrictionLayer{Digest: strings.Repeat("c", 64), Guardrails: []domain.GuardrailRule{
		rule("tenant.codename", domain.GuardrailKindExactPhrase, domain.GuardrailActionBlock, `{"phrases":["BLUEJAY"]}`),
	}}
	tenant, err := compiler.compileRestriction(domain.GuardrailLayerTenant, layer)
	if err != nil || tenant.Layer != domain.GuardrailLayerTenant || tenant.Lookback == 0 {
		t.Fatalf("compiled = %+v, %v", tenant, err)
	}
	if again, _ := compiler.compileRestriction(domain.GuardrailLayerTenant, layer); again != tenant {
		t.Fatal("the same layer digest must reuse its compiled rules")
	}
	if platform, _ := compiler.compileRestriction(domain.GuardrailLayerPlatform, layer); platform == tenant || platform.Layer != domain.GuardrailLayerPlatform {
		t.Fatal("platform and tenant layers attribute their hits separately")
	}
	broken := domain.RestrictionLayer{Digest: strings.Repeat("d", 64), Guardrails: []domain.GuardrailRule{{ID: "x", Kind: "telepathy", Action: domain.GuardrailActionBlock}}}
	if _, err = compiler.compileRestriction(domain.GuardrailLayerTenant, broken); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unknown rule kind: want ErrValidation, got %v", err)
	}
}
