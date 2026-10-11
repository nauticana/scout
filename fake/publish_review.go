package fake

import (
	"context"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PublishSeparation requires two-person review when RequiredFunc says so and excuses through
// ExcuseFunc; a nil ExcuseFunc refuses every exception.
type PublishSeparation struct {
	RequiredFunc func(ctx context.Context, tenantID int64, agentID string, kind domain.PublishReviewKind, at time.Time) (bool, error)
	ExcuseFunc   func(ctx context.Context, tenantID int64, agentID string, kind domain.PublishReviewKind, exceptionID string, person domain.PrincipalRef, at time.Time) error
}

var _ contract.PublishSeparation = PublishSeparation{}

func (f PublishSeparation) Required(ctx context.Context, tenantID int64, agentID string, kind domain.PublishReviewKind, at time.Time) (bool, error) {
	if f.RequiredFunc == nil {
		return false, nil
	}
	return f.RequiredFunc(ctx, tenantID, agentID, kind, at)
}

func (f PublishSeparation) Excuse(ctx context.Context, tenantID int64, agentID string, kind domain.PublishReviewKind, exceptionID string, person domain.PrincipalRef, at time.Time) error {
	if f.ExcuseFunc == nil {
		return domain.ErrForbidden
	}
	return f.ExcuseFunc(ctx, tenantID, agentID, kind, exceptionID, person, at)
}

// PublishReviewEvaluator is a function supplying the evaluations of a review's candidate.
type PublishReviewEvaluator func(ctx context.Context, candidate domain.PublishCandidate) ([]domain.PublishReviewEvaluation, error)

var _ contract.PublishReviewEvaluator = PublishReviewEvaluator(nil)

func (f PublishReviewEvaluator) Evaluations(ctx context.Context, candidate domain.PublishCandidate) ([]domain.PublishReviewEvaluation, error) {
	return f(ctx, candidate)
}
