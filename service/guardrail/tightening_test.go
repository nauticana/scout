package guardrail

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func tighteningRule(kind domain.GuardrailRuleKind, action domain.GuardrailAction, severity domain.GuardrailSeverity, params string, stages ...domain.GuardrailStage) domain.GuardrailRule {
	rule := domain.GuardrailRule{ID: "shared", Kind: kind, Action: action, Severity: severity, Stages: stages}
	if params != "" {
		rule.Params = json.RawMessage(params)
	}
	return rule
}

func TestCheckTighteningAcceptsOnlyAStricterRule(t *testing.T) {
	const (
		block = domain.GuardrailActionBlock
		flag  = domain.GuardrailActionFlag
		hard  = domain.GuardrailSeverityHard
		soft  = domain.GuardrailSeveritySoft
	)
	cases := []struct {
		name      string
		inherited domain.GuardrailRule
		candidate domain.GuardrailRule
		tighter   bool
	}{
		{"identical", tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), true},
		{"lower ceiling", tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":50}`), true},
		{"higher ceiling", tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":200}`), false},
		{"other kind", tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), tighteningRule(domain.GuardrailKindMaxOutputBytes, block, hard, `{"max":100}`), false},
		{"more stages", tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`, domain.GuardrailStageInput, domain.GuardrailStageToolInput), true},
		{"fewer stages", tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`, domain.GuardrailStageInput, domain.GuardrailStageRetrieval), tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), false},
		{"flag to block", tighteningRule(domain.GuardrailKindMaxInputBytes, flag, soft, `{"max":100}`), tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), true},
		{"block to flag", tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), tighteningRule(domain.GuardrailKindMaxInputBytes, flag, hard, `{"max":100}`), false},
		{"hard to soft", tighteningRule(domain.GuardrailKindMaxInputBytes, block, hard, `{"max":100}`), tighteningRule(domain.GuardrailKindMaxInputBytes, block, soft, `{"max":100}`), false},
		{"fewer tools", tighteningRule(domain.GuardrailKindToolAllowlist, block, hard, `{"tools":["a","b"]}`), tighteningRule(domain.GuardrailKindToolAllowlist, block, hard, `{"tools":["a"]}`), true},
		{"another tool", tighteningRule(domain.GuardrailKindToolAllowlist, block, hard, `{"tools":["a","b"]}`), tighteningRule(domain.GuardrailKindToolAllowlist, block, hard, `{"tools":["a","c"]}`), false},
		{"more denied tools", tighteningRule(domain.GuardrailKindToolDenylist, block, hard, `{"tools":["a"]}`), tighteningRule(domain.GuardrailKindToolDenylist, block, hard, `{"tools":["a","b"]}`), true},
		{"fewer denied tools", tighteningRule(domain.GuardrailKindToolDenylist, block, hard, `{"tools":["a","b"]}`), tighteningRule(domain.GuardrailKindToolDenylist, block, hard, `{"tools":["b"]}`), false},
		{"subdomain host", tighteningRule(domain.GuardrailKindDestinationAllowlist, block, hard, `{"hosts":["example.com"]}`), tighteningRule(domain.GuardrailKindDestinationAllowlist, block, hard, `{"hosts":["API.example.com"]}`), true},
		{"parent domain host", tighteningRule(domain.GuardrailKindDestinationAllowlist, block, hard, `{"hosts":["api.example.com"]}`), tighteningRule(domain.GuardrailKindDestinationAllowlist, block, hard, `{"hosts":["example.com"]}`), false},
		{"more phrases", tighteningRule(domain.GuardrailKindExactPhrase, block, hard, `{"phrases":["Secret"]}`), tighteningRule(domain.GuardrailKindExactPhrase, block, hard, `{"phrases":["Secret","code"]}`), true},
		{"folded phrase", tighteningRule(domain.GuardrailKindExactPhrase, block, hard, `{"phrases":["Secret"]}`), tighteningRule(domain.GuardrailKindExactPhrase, block, hard, `{"phrases":["SECRET"],"case_insensitive":true}`), true},
		{"case-sensitive phrase", tighteningRule(domain.GuardrailKindExactPhrase, block, hard, `{"phrases":["secret"],"case_insensitive":true}`), tighteningRule(domain.GuardrailKindExactPhrase, block, hard, `{"phrases":["secret"]}`), false},
		{"dropped phrase", tighteningRule(domain.GuardrailKindExactPhrase, block, hard, `{"phrases":["secret","code"]}`), tighteningRule(domain.GuardrailKindExactPhrase, block, hard, `{"phrases":["secret"]}`), false},
		{"longer lookback", tighteningRule(domain.GuardrailKindRegex, block, hard, `{"pattern":"a+","max_match_bytes":8}`), tighteningRule(domain.GuardrailKindRegex, block, hard, `{"pattern":"a+","max_match_bytes":16}`), true},
		{"other pattern", tighteningRule(domain.GuardrailKindRegex, block, hard, `{"pattern":"a+","max_match_bytes":8}`), tighteningRule(domain.GuardrailKindRegex, block, hard, `{"pattern":"b+","max_match_bytes":8}`), false},
		{"shorter lookback", tighteningRule(domain.GuardrailKindRegex, block, hard, `{"pattern":"a+","max_match_bytes":8}`), tighteningRule(domain.GuardrailKindRegex, block, hard, `{"pattern":"a+","max_match_bytes":4}`), false},
		{"default markers", tighteningRule(domain.GuardrailKindUntrustedContentMarker, domain.GuardrailActionRedact, soft, `{"open":"<untrusted_content>"}`), tighteningRule(domain.GuardrailKindUntrustedContentMarker, domain.GuardrailActionRedact, soft, ""), true},
		{"other markers", tighteningRule(domain.GuardrailKindUntrustedContentMarker, domain.GuardrailActionRedact, soft, ""), tighteningRule(domain.GuardrailKindUntrustedContentMarker, domain.GuardrailActionRedact, soft, `{"open":"[","close":"]"}`), false},
		{"approval of every tool", tighteningRule(domain.GuardrailKindIrreversibleToolApproval, block, hard, `{"tools":["pay"]}`), tighteningRule(domain.GuardrailKindIrreversibleToolApproval, block, hard, ""), true},
		{"approval of more tools", tighteningRule(domain.GuardrailKindIrreversibleToolApproval, block, hard, `{"tools":["pay"]}`), tighteningRule(domain.GuardrailKindIrreversibleToolApproval, block, hard, `{"tools":["pay","send"]}`), true},
		{"approval of fewer tools", tighteningRule(domain.GuardrailKindIrreversibleToolApproval, block, hard, ""), tighteningRule(domain.GuardrailKindIrreversibleToolApproval, block, hard, `{"tools":["pay"]}`), false},
		{"lower threshold", tighteningRule(domain.GuardrailKindPII, domain.GuardrailActionRedact, hard, ""), tighteningRule(domain.GuardrailKindPII, block, hard, `{"threshold":0.3}`), true},
		{"higher threshold", tighteningRule(domain.GuardrailKindPII, block, hard, `{"threshold":0.4}`), tighteningRule(domain.GuardrailKindPII, block, hard, ""), false},
		{"same schema", tighteningRule(domain.GuardrailKindJSONSchema, block, hard, `{"schema":{"type":"object","required":["a"]}}`), tighteningRule(domain.GuardrailKindJSONSchema, block, hard, `{"schema":{"required":["a"],"type":"object"}}`), true},
		{"other schema", tighteningRule(domain.GuardrailKindJSONSchema, block, hard, `{"schema":{"type":"object","required":["a"]}}`), tighteningRule(domain.GuardrailKindJSONSchema, block, hard, `{"schema":{"type":"object"}}`), false},
	}
	for _, item := range cases {
		err := CheckTightening(item.inherited, item.candidate)
		if item.tighter && err != nil {
			t.Errorf("%s: want tighter, got %v", item.name, err)
		}
		if !item.tighter && !errors.Is(err, domain.ErrAuthorityExceeded) {
			t.Errorf("%s: want ErrAuthorityExceeded, got %v", item.name, err)
		}
	}
}

func TestCheckTighteningRefusesParametersItCannotRead(t *testing.T) {
	inherited := tighteningRule(domain.GuardrailKindMaxInputBytes, domain.GuardrailActionBlock, domain.GuardrailSeverityHard, `{"max":100}`)
	candidate := tighteningRule(domain.GuardrailKindMaxInputBytes, domain.GuardrailActionBlock, domain.GuardrailSeverityHard, `{"max":10,"extra":true}`)
	if err := CheckTightening(inherited, candidate); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown parameter: want ErrValidation, got %v", err)
	}
	candidate = tighteningRule(domain.GuardrailKindMaxInputBytes, "allow", domain.GuardrailSeverityHard, `{"max":10}`)
	if err := CheckTightening(inherited, candidate); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown action: want ErrValidation, got %v", err)
	}
}
