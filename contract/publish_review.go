package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// PublishSeparation says whether a tenant separates authoring an agent from publishing it:
// then the person who requested a publish review may neither approve nor publish it. Excuse
// admits one explicit exception relieving that person of the separation at the instant, and
// fails closed with an error when the exception does not.
type PublishSeparation interface {
	Required(ctx context.Context, tenantID int64, agentID string, at time.Time) (bool, error)
	Excuse(ctx context.Context, tenantID int64, agentID, exceptionID string, person domain.PrincipalRef, at time.Time) error
}

// PublishReviewEvaluator supplies the evaluation results a publish review shows for the
// release its draft would freeze.
type PublishReviewEvaluator interface {
	Evaluations(ctx context.Context, candidate domain.PublishCandidate) ([]domain.PublishReviewEvaluation, error)
}

// StudioPublishReviewBackend requests, shows, and decides two-person publish reviews.
type StudioPublishReviewBackend interface {
	Request(ctx context.Context, actor domain.StudioActor, request domain.PublishReviewRequest) (domain.PublishReview, error)
	Review(ctx context.Context, actor domain.StudioActor, agentID string) (domain.PublishReviewState, error)
	Approve(ctx context.Context, actor domain.StudioActor, decision domain.PublishReviewDecision) (domain.PublishReview, error)
	Reject(ctx context.Context, actor domain.StudioActor, decision domain.PublishReviewDecision) (domain.PublishReview, error)
}

// StudioDraftCommentBackend lists, adds, and resolves comments on an agent's draft.
type StudioDraftCommentBackend interface {
	Comments(ctx context.Context, actor domain.StudioActor, agentID string, status domain.DraftCommentStatus) ([]domain.DraftComment, error)
	Comment(ctx context.Context, actor domain.StudioActor, comment domain.DraftComment) (domain.DraftComment, error)
	Resolve(ctx context.Context, actor domain.StudioActor, agentID string, commentID int64) (domain.DraftComment, error)
}
