package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/guardrail"
)

// encodedLayer is a validated layer in its stored, digest-addressed form.
type encodedLayer struct {
	layer      domain.RestrictionLayer
	denials    []byte
	guardrails []byte
}

// encodeLayer validates that a layer only restricts — deny statements, and
// guardrail rules that block or redact — and canonicalizes it, so equal layers
// share a digest however they were ordered.
func encodeLayer(ctx context.Context, rules contract.GuardrailRuleCompiler, layer domain.RestrictionLayer) (encodedLayer, error) {
	denials := slices.Clone(layer.Denials)
	for i := range denials {
		denials[i].ID = strings.TrimSpace(denials[i].ID)
	}
	if err := validateRestrictionDenials(denials); err != nil {
		return encodedLayer{}, err
	}
	slices.SortFunc(denials, func(a, b domain.PolicyStatement) int { return strings.Compare(a.ID, b.ID) })
	ruleList := slices.Clone(layer.Guardrails)
	if err := validateRestrictionGuardrails(ruleList); err != nil {
		return encodedLayer{}, err
	}
	slices.SortFunc(ruleList, func(a, b domain.GuardrailRule) int { return strings.Compare(a.ID, b.ID) })
	if denials == nil {
		denials = []domain.PolicyStatement{}
	}
	if ruleList == nil {
		ruleList = []domain.GuardrailRule{}
	}
	denialJSON, err := json.Marshal(denials)
	if err != nil {
		return encodedLayer{}, fmt.Errorf("%w: restriction denials: %v", domain.ErrValidation, err)
	}
	ruleJSON, err := json.Marshal(domain.GuardrailRuleSet{SchemaVersion: guardrail.RuleSetSchemaVersion, Rules: ruleList})
	if err != nil {
		return encodedLayer{}, fmt.Errorf("%w: restriction guardrails: %v", domain.ErrValidation, err)
	}
	if len(ruleList) > 0 {
		if rules == nil {
			return encodedLayer{}, fmt.Errorf("%w: a guardrail rule compiler is required to write restriction guardrails", domain.ErrNotReady)
		}
		ruleDigest := sha256.Sum256(ruleJSON)
		config := domain.GuardrailConfig{Version: "restriction", RulesDigest: hex.EncodeToString(ruleDigest[:]), Rules: ruleJSON}
		if _, err = rules.Validate(ctx, config); err != nil {
			return encodedLayer{}, err
		}
	}
	return encodedLayer{
		layer:   domain.RestrictionLayer{Digest: layerDigest(denialJSON, ruleJSON), Denials: denials, Guardrails: ruleList},
		denials: denialJSON, guardrails: ruleJSON,
	}, nil
}

// decodeLayer reads a stored layer and refuses one whose content no longer matches its digest.
func decodeLayer(digest, denialJSON, ruleJSON string) (domain.RestrictionLayer, error) {
	if layerDigest([]byte(denialJSON), []byte(ruleJSON)) != digest {
		return domain.RestrictionLayer{}, fmt.Errorf("%w: restriction layer %s does not match its digest", domain.ErrConflict, digest)
	}
	layer := domain.RestrictionLayer{Digest: digest}
	if err := json.Unmarshal([]byte(denialJSON), &layer.Denials); err != nil {
		return domain.RestrictionLayer{}, fmt.Errorf("%w: restriction layer %s denials: %v", domain.ErrConflict, digest, err)
	}
	if err := validateRestrictionDenials(layer.Denials); err != nil {
		return domain.RestrictionLayer{}, fmt.Errorf("%w: restriction layer %s denials are not restrictive: %v", domain.ErrConflict, digest, err)
	}
	var rules domain.GuardrailRuleSet
	if err := json.Unmarshal([]byte(ruleJSON), &rules); err != nil {
		return domain.RestrictionLayer{}, fmt.Errorf("%w: restriction layer %s guardrails: %v", domain.ErrConflict, digest, err)
	}
	if rules.SchemaVersion != guardrail.RuleSetSchemaVersion {
		return domain.RestrictionLayer{}, fmt.Errorf("%w: restriction layer %s has guardrail schema version %d", domain.ErrConflict, digest, rules.SchemaVersion)
	}
	if err := validateRestrictionGuardrails(rules.Rules); err != nil {
		return domain.RestrictionLayer{}, fmt.Errorf("%w: restriction layer %s guardrails are not restrictive: %v", domain.ErrConflict, digest, err)
	}
	layer.Guardrails = rules.Rules
	return layer, nil
}

func validateRestrictionDenials(denials []domain.PolicyStatement) error {
	seen := make(map[string]bool, len(denials))
	for _, statement := range denials {
		switch {
		case statement.ID == "" || statement.ID != strings.TrimSpace(statement.ID) || seen[statement.ID]:
			return fmt.Errorf("%w: every restriction denial needs a unique id", domain.ErrValidation)
		case statement.Effect != domain.PolicyDeny:
			return fmt.Errorf("%w: restriction statement %q must deny", domain.ErrValidation, statement.ID)
		case !nonEmptyPatterns(statement.Actions) || !nonEmptyPatterns(statement.Resources):
			return fmt.Errorf("%w: restriction denial %q names no valid action or resource", domain.ErrValidation, statement.ID)
		case len(statement.Obligations) > 0:
			return fmt.Errorf("%w: restriction denial %q carries obligations", domain.ErrValidation, statement.ID)
		}
		if len(statement.Conditions) > 0 {
			var conditions map[string]string
			if err := json.Unmarshal(statement.Conditions, &conditions); err != nil {
				return fmt.Errorf("%w: restriction denial %q has invalid conditions", domain.ErrValidation, statement.ID)
			}
		}
		seen[statement.ID] = true
	}
	return nil
}

func validateRestrictionGuardrails(rules []domain.GuardrailRule) error {
	for _, rule := range rules {
		if rule.Action != domain.GuardrailActionBlock && rule.Action != domain.GuardrailActionRedact {
			return fmt.Errorf("%w: restriction guardrail %q must block or redact", domain.ErrValidation, rule.ID)
		}
	}
	return nil
}

func nonEmptyPatterns(patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			return false
		}
	}
	return true
}

func layerDigest(denialJSON, ruleJSON []byte) string {
	hash := sha256.New()
	hash.Write([]byte("scout.restriction_layer.v1\n"))
	hash.Write(denialJSON)
	hash.Write([]byte("\n"))
	hash.Write(ruleJSON)
	return hex.EncodeToString(hash.Sum(nil))
}
