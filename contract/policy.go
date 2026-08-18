package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// PolicyDecisionPoint is the single place a governed boundary asks whether an
// action is allowed. It fails closed: an evaluator error, an expired policy, or
// an unknown resource is a deny, never a default allow.
type PolicyDecisionPoint interface {
	Decide(ctx context.Context, subject domain.DecisionSubject) (domain.Decision, error)
}

// PolicyResolver returns the policy set bound to a principal's effective release.
type PolicyResolver interface {
	Policies(ctx context.Context, principal domain.Principal) (domain.PolicySet, error)
}

// ObligationEnforcer applies one obligation a decision attached to an allow.
// An unrecognized kind is a hard failure at the boundary, never a skip.
type ObligationEnforcer interface {
	Kind() domain.ObligationKind
	Enforce(ctx context.Context, subject domain.DecisionSubject, obligation domain.Obligation) error
}
