// Package policy is the enforcement-owning decision point. An external engine
// may be plugged in behind contract.PolicyDecisionPoint, but the canonical
// policy state and the enforcement stay here.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// SetEvaluator decides against the policy set bound to a principal's effective
// release. Deny wins over allow, and no match is a deny — the default is never
// permissive, so an unbound resource is refused rather than allowed by omission.
type SetEvaluator struct {
	Policies contract.PolicyResolver
	Now      func() time.Time
}

// Decide evaluates one subject and returns an auditable decision.
func (e *SetEvaluator) Decide(ctx context.Context, subject domain.DecisionSubject) (domain.Decision, error) {
	if e.Policies == nil {
		return denied("", "", "no policy resolver configured", e.now()), fmt.Errorf("policy evaluator: resolver is required")
	}
	if subject.Principal.Kind == "" || strings.TrimSpace(subject.Principal.ID) == "" {
		return denied("", "", "unresolved principal", e.now()), fmt.Errorf("%w: policy decisions require a principal", domain.ErrPrincipalUnknown)
	}
	if strings.TrimSpace(subject.Action) == "" {
		return denied("", "", "no action", e.now()), fmt.Errorf("%w: policy decisions require an action", domain.ErrValidation)
	}
	set, err := e.Policies.Policies(ctx, subject.Principal)
	if err != nil {
		return denied("", "", "policy unavailable", e.now()), err
	}
	environment, err := decodeEnvironment(subject.Environment)
	if err != nil {
		return denied(set.PolicyID, set.Version, "invalid environment", e.now()), err
	}

	var allow *domain.PolicyStatement
	var obligations []domain.Obligation
	for index := range set.Statements {
		statement := &set.Statements[index]
		matched, err := matches(statement, subject, environment)
		if err != nil {
			return denied(set.PolicyID, set.Version, "invalid statement", e.now()), err
		}
		if !matched {
			continue
		}
		if statement.Effect == domain.PolicyDeny {
			return domain.Decision{
				Outcome: domain.DecisionDeny, PolicyID: set.PolicyID, PolicyVersion: set.Version,
				Reason: reasonOr(statement, "denied by "+statement.ID), EvaluatedAt: e.now(),
			}, nil
		}
		if allow == nil {
			allow = statement
		}
		obligations = appendUniqueObligations(obligations, statement.Obligations)
	}
	if allow == nil {
		return denied(set.PolicyID, set.Version, "no statement grants "+subject.Action, e.now()), nil
	}
	return domain.Decision{
		Outcome: domain.DecisionAllow, Obligations: obligations,
		PolicyID: set.PolicyID, PolicyVersion: set.Version,
		Reason: reasonOr(allow, "allowed by "+allow.ID), EvaluatedAt: e.now(),
	}, nil
}

func appendUniqueObligations(existing, additions []domain.Obligation) []domain.Obligation {
	for _, addition := range additions {
		duplicate := false
		for _, current := range existing {
			if current.Kind == addition.Kind && bytes.Equal(current.Params, addition.Params) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, addition)
		}
	}
	return existing
}

func matches(statement *domain.PolicyStatement, subject domain.DecisionSubject, environment map[string]string) (bool, error) {
	if statement.Effect != domain.PolicyAllow && statement.Effect != domain.PolicyDeny {
		return false, fmt.Errorf("%w: statement %q has no effect", domain.ErrValidation, statement.ID)
	}
	if !matchesAny(statement.Actions, subject.Action) || !matchesAny(statement.Resources, subject.Resource) {
		return false, nil
	}
	if len(statement.Conditions) == 0 {
		return true, nil
	}
	var conditions map[string]string
	if err := json.Unmarshal(statement.Conditions, &conditions); err != nil {
		return false, fmt.Errorf("%w: statement %q has invalid conditions: %v", domain.ErrValidation, statement.ID, err)
	}
	for key, want := range conditions {
		if environment[key] != want {
			return false, nil
		}
	}
	return true, nil
}

// matchesAny supports an exact value or a single trailing "*"; nothing else, so
// a pattern can never widen unexpectedly.
func matchesAny(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if pattern == "*" || pattern == value {
			return true
		}
		if prefix, found := strings.CutSuffix(pattern, "*"); found && strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func decodeEnvironment(raw []byte) (map[string]string, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var environment map[string]string
	if err := json.Unmarshal(raw, &environment); err != nil {
		return nil, fmt.Errorf("%w: decision environment must be a flat string map: %v", domain.ErrValidation, err)
	}
	return environment, nil
}

func denied(policyID, version, reason string, at time.Time) domain.Decision {
	return domain.Decision{Outcome: domain.DecisionDeny, PolicyID: policyID, PolicyVersion: version, Reason: reason, EvaluatedAt: at}
}

func reasonOr(statement *domain.PolicyStatement, fallback string) string {
	if strings.TrimSpace(statement.Reason) != "" {
		return statement.Reason
	}
	return fallback
}

func (e *SetEvaluator) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

var _ contract.PolicyDecisionPoint = (*SetEvaluator)(nil)
