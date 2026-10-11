package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// ProductionReviewIntake offers production turns to human review: a completed turn
// through the production sampler, a rated turn when its rating is at or below the
// configured threshold. Both honor the tenant's sampling opt-out, redaction, and residency.
type ProductionReviewIntake interface {
	TurnCompleted(ctx context.Context, turn domain.ReviewTurn) error
	TurnRated(ctx context.Context, turn domain.ReviewTurn, rating int) error
}

// StudioReviewBackend serves Studio's Review tab: an agent's sampled and low-rated
// turns, their content, grading, dismissal, and promotion into the golden set.
type StudioReviewBackend interface {
	Reviews(ctx context.Context, actor domain.StudioActor, filter domain.ReviewFilter) (domain.ReviewPage, error)
	ReviewTurn(ctx context.Context, actor domain.StudioActor, itemID int64) (domain.ReviewTurnContent, error)
	Grade(ctx context.Context, actor domain.StudioActor, itemID int64, grade domain.ReviewGrade) (domain.HumanReviewItem, error)
	Dismiss(ctx context.Context, actor domain.StudioActor, itemID int64, notes string) (domain.HumanReviewItem, error)
	Promote(ctx context.Context, actor domain.StudioActor, promotion domain.ReviewPromotion) (domain.HumanReviewItem, domain.GoldenCaseSet, error)
}
