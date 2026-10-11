package guardrail

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/domain"
)

var (
	actionStrictness   = map[domain.GuardrailAction]int{domain.GuardrailActionFlag: 0, domain.GuardrailActionRedact: 1, domain.GuardrailActionBlock: 2}
	severityStrictness = map[domain.GuardrailSeverity]int{domain.GuardrailSeveritySoft: 0, domain.GuardrailSeverityHard: 1}
)

// CheckTightening reports whether candidate enforces at least what inherited does, so a rule
// that reuses an inherited rule's id may stand in for it: the same kind, every stage the
// inherited rule inspects, an action and severity at least as strict, and parameters that
// fire on at least the same content. A difference it cannot prove stricter is
// domain.ErrAuthorityExceeded; a rule whose parameters do not decode is domain.ErrValidation.
func CheckTightening(inherited, candidate domain.GuardrailRule) error {
	loosens := func(format string, args ...any) error {
		return fmt.Errorf("%w: rule %q %s", domain.ErrAuthorityExceeded, candidate.ID, fmt.Sprintf(format, args...))
	}
	if candidate.Kind != inherited.Kind {
		return loosens("changes kind %s to %s", inherited.Kind, candidate.Kind)
	}
	spec, known := kindSpecs[inherited.Kind]
	if !known {
		return fmt.Errorf("%w: rule %q has unknown kind %q", domain.ErrValidation, inherited.ID, inherited.Kind)
	}
	stages := effectiveStages(candidate, spec)
	for _, stage := range effectiveStages(inherited, spec) {
		if !slices.Contains(stages, stage) {
			return loosens("stops inspecting %s", stage)
		}
	}
	parentAction, parentKnown := actionStrictness[inherited.Action]
	childAction, childKnown := actionStrictness[candidate.Action]
	parentSeverity, parentSeverityKnown := severityStrictness[inherited.Severity]
	childSeverity, childSeverityKnown := severityStrictness[candidate.Severity]
	if !parentKnown || !childKnown || !parentSeverityKnown || !childSeverityKnown {
		return fmt.Errorf("%w: rule %q needs a known action and severity", domain.ErrValidation, candidate.ID)
	}
	if childAction < parentAction {
		return loosens("weakens action %s to %s", inherited.Action, candidate.Action)
	}
	if childSeverity < parentSeverity {
		return loosens("lowers severity %s to %s", inherited.Severity, candidate.Severity)
	}
	parent, err := decodeRuleParams(inherited.Params)
	if err != nil {
		return fmt.Errorf("%w: inherited rule %q: %v", domain.ErrValidation, inherited.ID, err)
	}
	child, err := decodeRuleParams(candidate.Params)
	if err != nil {
		return fmt.Errorf("%w: rule %q: %v", domain.ErrValidation, candidate.ID, err)
	}
	if reason := looserParams(inherited.Kind, parent, child); reason != "" {
		return loosens("%s", reason)
	}
	return nil
}

func effectiveStages(rule domain.GuardrailRule, spec kindSpec) []domain.GuardrailStage {
	if len(rule.Stages) == 0 {
		return spec.defaultStages
	}
	return rule.Stages
}

// looserParams names how child's parameters let through content parent's would catch,
// or returns empty when they do not.
func looserParams(kind domain.GuardrailRuleKind, parent, child ruleParams) string {
	switch kind {
	case domain.GuardrailKindMaxInputBytes, domain.GuardrailKindMaxOutputBytes:
		if parent.Max == nil || child.Max == nil || *child.Max > *parent.Max {
			return "raises its byte ceiling"
		}
	case domain.GuardrailKindJSONSchema:
		parentSchema, parentErr := json.Marshal(parent.Schema)
		childSchema, childErr := json.Marshal(child.Schema)
		if parentErr != nil || childErr != nil || string(parentSchema) != string(childSchema) {
			return "changes its schema"
		}
	case domain.GuardrailKindToolAllowlist:
		allowed := toSet(parent.Tools, false)
		for _, tool := range child.Tools {
			if _, ok := allowed[strings.TrimSpace(tool)]; !ok {
				return fmt.Sprintf("allows tool %q", tool)
			}
		}
	case domain.GuardrailKindDestinationAllowlist:
		allowed := toSet(parent.Hosts, true)
		for _, host := range child.Hosts {
			if !hostAllowed(strings.ToLower(strings.TrimSpace(host)), allowed) {
				return fmt.Sprintf("allows host %q", host)
			}
		}
	case domain.GuardrailKindExactPhrase:
		if parent.CaseInsensitive && !child.CaseInsensitive {
			return "matches its phrases case-sensitively"
		}
		for _, phrase := range parent.Phrases {
			if !slices.ContainsFunc(child.Phrases, func(candidate string) bool {
				if child.CaseInsensitive {
					return strings.ToLower(candidate) == strings.ToLower(phrase)
				}
				return candidate == phrase
			}) {
				return fmt.Sprintf("drops phrase %q", phrase)
			}
		}
	case domain.GuardrailKindRegex:
		if child.Pattern != parent.Pattern {
			return "changes its pattern"
		}
		if child.MaxMatchBytes < parent.MaxMatchBytes {
			return "shortens max_match_bytes"
		}
	case domain.GuardrailKindUntrustedContentMarker:
		if markerOr(child.Open, DefaultUntrustedOpen) != markerOr(parent.Open, DefaultUntrustedOpen) ||
			markerOr(child.Close, DefaultUntrustedClose) != markerOr(parent.Close, DefaultUntrustedClose) {
			return "changes its markers"
		}
	case domain.GuardrailKindIrreversibleToolApproval:
		// An empty list asks approval for every tool.
		if len(child.Tools) == 0 {
			return ""
		}
		if len(parent.Tools) == 0 {
			return "no longer asks approval for every tool"
		}
		covered := toSet(child.Tools, false)
		for _, tool := range parent.Tools {
			if _, ok := covered[strings.TrimSpace(tool)]; !ok {
				return fmt.Sprintf("no longer asks approval for tool %q", tool)
			}
		}
	default:
		if threshold(child) > threshold(parent) {
			return "raises its classifier threshold"
		}
	}
	return ""
}

func markerOr(marker, fallback string) string {
	if marker == "" {
		return fallback
	}
	return marker
}

func threshold(params ruleParams) float64 {
	if params.Threshold == nil {
		return 0.5
	}
	return *params.Threshold
}
