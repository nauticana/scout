package scope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// NarrowingRule names the comparator applied when a narrower scope overrides what
// it inherits. The values match resource_kind.narrowing_rule.
type NarrowingRule string

const (
	// NarrowSubset requires the candidate set to be contained in the inherited set.
	NarrowSubset NarrowingRule = "subset"
	// NarrowAtMost requires every candidate limit to be no larger than the inherited one.
	NarrowAtMost NarrowingRule = "at_most"
	// NarrowOrdered requires the candidate rank to be no higher than the inherited rank.
	NarrowOrdered NarrowingRule = "ordered"
	// NarrowPolicy requires the candidate's allow statements to be a subset of the
	// inherited ones; denies may always be added, because adding one narrows.
	NarrowPolicy NarrowingRule = "policy"
	// NarrowText carries no authority, so any value is accepted; sealing still applies.
	NarrowText NarrowingRule = "text"
)

// PlatformNarrowingRules mirrors the seeded resource_kind.narrowing_rule column.
var PlatformNarrowingRules = map[domain.ResourceKind]NarrowingRule{
	domain.ResourcePromptSection: NarrowText,
	domain.ResourcePolicy:        NarrowPolicy,
	domain.ResourceTool:          NarrowSubset,
	domain.ResourceKnowledge:     NarrowSubset,
	domain.ResourceModel:         NarrowSubset,
	domain.ResourceEntitlement:   NarrowSubset,
	domain.ResourceBudget:        NarrowAtMost,
	domain.ResourceAutonomy:      NarrowOrdered,
}

// LatticeChecker rejects any effective value that broadens what it inherits. An
// unmapped resource kind fails closed rather than passing unchecked.
type LatticeChecker struct {
	// Rules overrides PlatformNarrowingRules per kind; unset entries fall through to it.
	Rules map[domain.ResourceKind]NarrowingRule
}

// CheckNarrowing reports domain.ErrAuthorityExceeded when candidate is broader than inherited.
func (c *LatticeChecker) CheckNarrowing(_ context.Context, kind domain.ResourceKind, inherited, candidate []byte) error {
	if len(inherited) == 0 {
		return nil
	}
	rule, err := c.ruleFor(kind)
	if err != nil {
		return err
	}
	switch rule {
	case NarrowText:
		return nil
	case NarrowSubset:
		return checkSubset(kind, inherited, candidate)
	case NarrowPolicy:
		return checkPolicy(kind, inherited, candidate)
	case NarrowAtMost:
		return checkAtMost(kind, inherited, candidate)
	case NarrowOrdered:
		return checkOrdered(kind, inherited, candidate)
	default:
		return fmt.Errorf("%w: unsupported narrowing rule %q for kind %q", domain.ErrValidation, rule, kind)
	}
}

func (c *LatticeChecker) ruleFor(kind domain.ResourceKind) (NarrowingRule, error) {
	if rule, ok := c.Rules[kind]; ok {
		return rule, nil
	}
	if rule, ok := PlatformNarrowingRules[kind]; ok {
		return rule, nil
	}
	return "", fmt.Errorf("%w: no narrowing rule for resource kind %q", domain.ErrValidation, kind)
}

func checkSubset(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, err := decodeSet(inherited)
	if err != nil {
		return err
	}
	child, err := decodeSet(candidate)
	if err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(parent))
	for _, item := range parent {
		allowed[item] = struct{}{}
	}
	for _, item := range child {
		if _, ok := allowed[item]; !ok {
			return fmt.Errorf("%w: %s adds %q, which the parent scope does not grant", domain.ErrAuthorityExceeded, kind, item)
		}
	}
	return nil
}

func checkPolicy(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, err := decodeStatements(inherited)
	if err != nil {
		return err
	}
	child, err := decodeStatements(candidate)
	if err != nil {
		return err
	}
	granted := make(map[string]domain.PolicyStatement, len(parent))
	for _, statement := range parent {
		if statement.Effect == domain.PolicyAllow {
			granted[statement.ID] = statement
		}
	}
	for _, statement := range child {
		if statement.Effect != domain.PolicyAllow {
			continue
		}
		parentStatement, ok := granted[statement.ID]
		if !ok {
			return fmt.Errorf("%w: %s adds allow statement %q, which the parent scope does not grant", domain.ErrAuthorityExceeded, kind, statement.ID)
		}
		if !policyAllowSubset(parentStatement, statement) {
			return fmt.Errorf("%w: %s broadens allow statement %q", domain.ErrAuthorityExceeded, kind, statement.ID)
		}
	}
	return nil
}

func policyAllowSubset(parent, child domain.PolicyStatement) bool {
	return patternsSubset(parent.Actions, child.Actions) && patternsSubset(parent.Resources, child.Resources) &&
		conditionsSubset(parent.Conditions, child.Conditions) && obligationsPreserved(parent.Obligations, child.Obligations)
}

func patternsSubset(parent, child []string) bool {
	for _, candidate := range child {
		covered := false
		for _, inherited := range parent {
			if patternSubset(candidate, inherited) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func patternSubset(child, parent string) bool {
	if parent == "*" || child == parent {
		return true
	}
	parentPrefix, parentWildcard := strings.CutSuffix(parent, "*")
	if !parentWildcard {
		return false
	}
	childPrefix, childWildcard := strings.CutSuffix(child, "*")
	if childWildcard {
		return strings.HasPrefix(childPrefix, parentPrefix)
	}
	return strings.HasPrefix(child, parentPrefix)
}

func conditionsSubset(parentRaw, childRaw json.RawMessage) bool {
	parent, ok := conditionMap(parentRaw)
	if !ok {
		return false
	}
	child, ok := conditionMap(childRaw)
	if !ok {
		return false
	}
	for key, value := range parent {
		if child[key] != value {
			return false
		}
	}
	return true
}

func conditionMap(raw json.RawMessage) (map[string]string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]string{}, true
	}
	var conditions map[string]string
	if err := json.Unmarshal(raw, &conditions); err != nil {
		return nil, false
	}
	return conditions, true
}

func obligationsPreserved(parent, child []domain.Obligation) bool {
	for _, required := range parent {
		found := false
		for _, candidate := range child {
			if required.Kind == candidate.Kind && bytes.Equal(required.Params, candidate.Params) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func checkAtMost(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, err := decodeBudget(inherited)
	if err != nil {
		return err
	}
	child, err := decodeBudget(candidate)
	if err != nil {
		return err
	}
	if parent.Tokens > 0 && (child.Tokens == 0 || child.Tokens > parent.Tokens) {
		return fmt.Errorf("%w: %s raises the token ceiling above %d", domain.ErrAuthorityExceeded, kind, parent.Tokens)
	}
	if parent.CostMinorUnits > 0 && (child.CostMinorUnits == 0 || child.CostMinorUnits > parent.CostMinorUnits) {
		return fmt.Errorf("%w: %s raises the cost ceiling above %d", domain.ErrAuthorityExceeded, kind, parent.CostMinorUnits)
	}
	return nil
}

func checkOrdered(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, err := decodeAutonomy(inherited)
	if err != nil {
		return err
	}
	child, err := decodeAutonomy(candidate)
	if err != nil {
		return err
	}
	if autonomyRank[child.Mode] > autonomyRank[parent.Mode] {
		return fmt.Errorf("%w: %s raises the operating mode from %q to %q", domain.ErrAuthorityExceeded, kind, parent.Mode, child.Mode)
	}
	if child.Mode == domain.AutonomyBounded && parent.Mode == domain.AutonomyBounded &&
		!windowSubset(parent.WindowFrom, parent.WindowTo, child.WindowFrom, child.WindowTo) {
		return fmt.Errorf("%w: %s widens the bounded operating window", domain.ErrAuthorityExceeded, kind)
	}
	return nil
}

func windowSubset(parentFrom, parentTo, childFrom, childTo int) bool {
	if parentTo == 0 {
		return true
	}
	return childTo != 0 && childFrom >= parentFrom && childTo <= parentTo
}

var _ contract.NarrowingChecker = (*LatticeChecker)(nil)
