package evaluation

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ReviewIntake feeds production turns into the human review queue: a completed turn
// when the production sampler selects it, a rated turn when its rating is at or below
// FeedbackThreshold. A tenant whose sampling policy opts out, requires redaction, or
// pins another residency region contributes neither; turns are never redacted here.
type ReviewIntake struct {
	Sampler  contract.ProductionSampler
	Policies contract.SamplingPolicyRepository
	Queue    contract.HumanReviewQueue
	// FeedbackThreshold is the highest rating, 1 to 5, that queues a rated turn; zero queues none.
	FeedbackThreshold int
}

var _ contract.ProductionReviewIntake = (*ReviewIntake)(nil)

func validReviewTurn(turn domain.ReviewTurn) error {
	if turn.TenantContext.TenantID <= 0 || strings.TrimSpace(turn.RequestID) == "" || strings.TrimSpace(turn.AgentID) == "" {
		return fmt.Errorf("%w: a reviewed turn needs a tenant, a request, and an agent", domain.ErrValidation)
	}
	return nil
}

// TurnCompleted queues the turn when the sampler selects it.
func (intake *ReviewIntake) TurnCompleted(ctx context.Context, turn domain.ReviewTurn) error {
	if intake.Sampler == nil || intake.Queue == nil {
		return fmt.Errorf("review intake: sampler and queue are required")
	}
	if err := validReviewTurn(turn); err != nil {
		return err
	}
	reason, err := intake.Sampler.Sample(ctx, domain.SampleSignal{
		TenantContext: turn.TenantContext, RequestID: turn.RequestID, AgentID: turn.AgentID, AgentVersion: turn.AgentVersion,
	})
	if err != nil || reason == "" {
		return err
	}
	return intake.enqueue(ctx, turn, domain.ReviewSourceSampled, "sampled: "+reason)
}

// TurnRated queues the turn when its rating is at or below the threshold and the tenant's policy admits it.
func (intake *ReviewIntake) TurnRated(ctx context.Context, turn domain.ReviewTurn, rating int) error {
	if intake.Policies == nil || intake.Queue == nil {
		return fmt.Errorf("review intake: sampling policies and queue are required")
	}
	if intake.FeedbackThreshold < 0 || intake.FeedbackThreshold > domain.ReviewMaxScore {
		return fmt.Errorf("%w: feedback threshold must be 0 to %d", domain.ErrValidation, domain.ReviewMaxScore)
	}
	if err := validReviewTurn(turn); err != nil {
		return err
	}
	if rating < domain.ReviewMinScore || rating > domain.ReviewMaxScore {
		return fmt.Errorf("%w: rating must be %d to %d", domain.ErrValidation, domain.ReviewMinScore, domain.ReviewMaxScore)
	}
	if rating > intake.FeedbackThreshold {
		return nil
	}
	policy, err := intake.Policies.SamplingPolicyFor(ctx, turn.TenantContext.TenantID)
	if err != nil {
		return fmt.Errorf("sampling policy for tenant %d: %w", turn.TenantContext.TenantID, err)
	}
	if !policyAdmits(policy, turn.TenantContext, false) {
		return nil
	}
	return intake.enqueue(ctx, turn, domain.ReviewSourceFeedback, fmt.Sprintf("rated %d of %d", rating, domain.ReviewMaxScore))
}

func (intake *ReviewIntake) enqueue(ctx context.Context, turn domain.ReviewTurn, source domain.ReviewSource, reason string) error {
	return intake.Queue.Enqueue(ctx, domain.HumanReviewItem{
		TenantID: turn.TenantContext.TenantID, Source: source, Reason: reason, Turn: domain.ReviewTurnRef{RequestID: turn.RequestID},
	})
}
