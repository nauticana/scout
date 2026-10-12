package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/domain"
)

// overridable is one effective setting a restriction can override: a tool set, or the
// allow statements of a policy with their decoded conditions.
type overridable struct {
	kind   domain.ResourceKind
	id     string
	tools  []string
	allows []allowStatement
}

type allowStatement struct {
	statement  domain.PolicyStatement
	conditions map[string]string
}

// restrictionOverrides lists the platform's, then the tenant's, restrictions that override an
// agent's effective settings: a denial overrides the policy allows it can meet and, covering
// tool:invoke, the tools it matches; a blocking tool_allowlist overrides the tools it leaves out,
// and a blocking tool_denylist the tools it names.
// An undecodable setting is ErrValidation; an undecodable restriction is ErrConflict.
func restrictionOverrides(layers domain.RestrictionLayers, effective []domain.ResourceExplanation) ([]domain.RestrictionOverride, error) {
	settings, err := decodeOverridable(effective)
	if err != nil {
		return nil, err
	}
	overrides := []domain.RestrictionOverride{}
	for _, in := range []struct {
		name  domain.RestrictionLayerName
		layer domain.RestrictionLayer
	}{{domain.RestrictionPlatform, layers.Platform}, {domain.RestrictionTenant, layers.Tenant}} {
		for _, denial := range in.layer.Denials {
			conditions, err := statementConditions(denial)
			if err != nil {
				return nil, fmt.Errorf("%w: %s restriction layer: %v", domain.ErrConflict, in.name, err)
			}
			if found := deniedBy(settings, denial, conditions); len(found) > 0 {
				overrides = append(overrides, domain.RestrictionOverride{Layer: in.name, Kind: domain.RestrictionDenial,
					RestrictionID: denial.ID, Conditional: len(conditions) > 0, Settings: found})
			}
		}
		for _, rule := range in.layer.Guardrails {
			found, err := blockedBy(settings, rule)
			if err != nil {
				return nil, err
			}
			if len(found) > 0 {
				overrides = append(overrides, domain.RestrictionOverride{Layer: in.name, Kind: domain.RestrictionGuardrail,
					RestrictionID: rule.ID, Settings: found})
			}
		}
	}
	return overrides, nil
}

func decodeOverridable(effective []domain.ResourceExplanation) ([]overridable, error) {
	var settings []overridable
	for _, resource := range effective {
		if len(bytes.TrimSpace(resource.Value)) == 0 {
			continue
		}
		setting := overridable{kind: resource.ResourceKind, id: resource.ResourceID}
		switch resource.ResourceKind {
		case domain.ResourceTool:
			if err := json.Unmarshal(resource.Value, &setting.tools); err != nil {
				return nil, fmt.Errorf("%w: tool setting %q is not a tool set: %v", domain.ErrValidation, resource.ResourceID, err)
			}
		case domain.ResourcePolicy:
			var statements []domain.PolicyStatement
			if err := json.Unmarshal(resource.Value, &statements); err != nil {
				return nil, fmt.Errorf("%w: policy setting %q is not a statement list: %v", domain.ErrValidation, resource.ResourceID, err)
			}
			for _, statement := range statements {
				if statement.Effect != domain.PolicyAllow {
					continue
				}
				conditions, err := statementConditions(statement)
				if err != nil {
					return nil, err
				}
				setting.allows = append(setting.allows, allowStatement{statement: statement, conditions: conditions})
			}
		default:
			continue
		}
		settings = append(settings, setting)
	}
	return settings, nil
}

// deniedBy returns the settings a denial overrides: the tools a tool call is decided on when
// the denial covers tool:invoke, and the allow statements some decision could meet it at.
func deniedBy(settings []overridable, denial domain.PolicyStatement, conditions map[string]string) []domain.RestrictedSetting {
	coversTools := matchesAny(denial.Actions, domain.PolicyActionToolInvoke)
	var found []domain.RestrictedSetting
	for _, setting := range settings {
		var members []string
		for _, tool := range setting.tools {
			if coversTools && matchesAny(denial.Resources, tool) {
				members = append(members, tool)
			}
		}
		for _, allow := range setting.allows {
			if patternsOverlap(denial.Actions, allow.statement.Actions) && patternsOverlap(denial.Resources, allow.statement.Resources) &&
				conditionsMeet(conditions, allow.conditions) {
				members = append(members, allow.statement.ID)
			}
		}
		if len(members) > 0 {
			found = append(found, domain.RestrictedSetting{ResourceKind: setting.kind, ResourceID: setting.id, Members: members})
		}
	}
	return found
}

// blockedBy returns the tools of each tool set a blocking tool_allowlist rule leaves out or a
// blocking tool_denylist rule names; no other guardrail kind decides on a setting.
func blockedBy(settings []overridable, rule domain.GuardrailRule) ([]domain.RestrictedSetting, error) {
	deny := rule.Kind == domain.GuardrailKindToolDenylist
	if !deny && rule.Kind != domain.GuardrailKindToolAllowlist || rule.Action != domain.GuardrailActionBlock {
		return nil, nil
	}
	var params struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(rule.Params, &params); err != nil {
		return nil, fmt.Errorf("%w: restriction guardrail %q has unreadable params: %v", domain.ErrConflict, rule.ID, err)
	}
	var found []domain.RestrictedSetting
	for _, setting := range settings {
		var members []string
		for _, tool := range setting.tools {
			if slices.Contains(params.Tools, tool) == deny {
				members = append(members, tool)
			}
		}
		if len(members) > 0 {
			found = append(found, domain.RestrictedSetting{ResourceKind: setting.kind, ResourceID: setting.id, Members: members})
		}
	}
	return found, nil
}

func statementConditions(statement domain.PolicyStatement) (map[string]string, error) {
	if len(bytes.TrimSpace(statement.Conditions)) == 0 {
		return nil, nil
	}
	var conditions map[string]string
	if err := json.Unmarshal(statement.Conditions, &conditions); err != nil {
		return nil, fmt.Errorf("%w: statement %q has invalid conditions: %v", domain.ErrValidation, statement.ID, err)
	}
	return conditions, nil
}

// patternsOverlap reports whether one value matches a pattern of each list under the
// exact-or-single-trailing-"*" rule of matchesAny.
func patternsOverlap(left, right []string) bool {
	for _, a := range left {
		for _, b := range right {
			aPrefix, aWild := strings.CutSuffix(a, "*")
			bPrefix, bWild := strings.CutSuffix(b, "*")
			switch {
			case aWild && bWild:
				if strings.HasPrefix(aPrefix, bPrefix) || strings.HasPrefix(bPrefix, aPrefix) {
					return true
				}
			case aWild:
				if strings.HasPrefix(b, aPrefix) {
					return true
				}
			case bWild:
				if strings.HasPrefix(a, bPrefix) {
					return true
				}
			case a == b:
				return true
			}
		}
	}
	return false
}

// conditionsMeet reports whether one decision environment can hold both condition sets.
func conditionsMeet(left, right map[string]string) bool {
	for key, want := range left {
		if other, ok := right[key]; ok && other != want {
			return false
		}
	}
	return true
}
