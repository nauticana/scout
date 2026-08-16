package guardrail

import (
	"encoding/json"

	"github.com/nauticana/scout/domain"
)

// DefaultBaseline is the reference release-independent policy: bounded sizes and untrusted-content
// fencing of tool and retrieved content. Operators extend it; classifier rules are added only when
// the matching providers are wired.
func DefaultBaseline(maxInputBytes, maxOutputBytes int) domain.GuardrailRuleSet {
	return domain.GuardrailRuleSet{
		SchemaVersion: RuleSetSchemaVersion,
		Rules: []domain.GuardrailRule{
			{ID: "baseline.max_input_bytes", Kind: domain.GuardrailKindMaxInputBytes, Stages: []domain.GuardrailStage{domain.GuardrailStageInput, domain.GuardrailStageToolInput, domain.GuardrailStageRetrieval}, Action: domain.GuardrailActionBlock, Severity: domain.GuardrailSeverityHard, Params: mustJSON(map[string]int{"max": maxInputBytes})},
			{ID: "baseline.max_output_bytes", Kind: domain.GuardrailKindMaxOutputBytes, Stages: []domain.GuardrailStage{domain.GuardrailStageOutput, domain.GuardrailStageToolOutput}, Action: domain.GuardrailActionBlock, Severity: domain.GuardrailSeverityHard, Params: mustJSON(map[string]int{"max": maxOutputBytes})},
			{ID: "baseline.untrusted_content", Kind: domain.GuardrailKindUntrustedContentMarker, Action: domain.GuardrailActionRedact, Severity: domain.GuardrailSeveritySoft},
		},
	}
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
