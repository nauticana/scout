package dataplane

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qFeedbackTurn    = "scout_feedback_turn"
	qFeedbackPut     = "scout_feedback_put"
	qFeedbackSummary = "scout_feedback_summary"
)

var feedbackQueries = map[string]string{
	qFeedbackTurn: `
SELECT t.conversation_id, t.turn_no, t.status_code, c.agent_id, c.end_user_ref, c.agent_version
  FROM conversation_turn t
  JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
 WHERE t.partner_id = ? AND t.request_id = ?`,
	qFeedbackPut: `
INSERT INTO turn_feedback (partner_id, conversation_id, turn_no, principal_kind, principal_id, rating, reason)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, conversation_id, turn_no, principal_kind, principal_id)
DO UPDATE SET rating = EXCLUDED.rating, reason = EXCLUDED.reason, recorded_at = CURRENT_TIMESTAMP`,
	qFeedbackSummary: `
SELECT c.agent_version, f.rating, COUNT(*)
  FROM turn_feedback f
  JOIN agent_conversation c ON c.partner_id = f.partner_id AND c.conversation_id = f.conversation_id
 WHERE f.partner_id = ? AND c.agent_id = ? AND f.recorded_at >= ? AND f.recorded_at < ?
 GROUP BY c.agent_version, f.rating
 ORDER BY c.agent_version, f.rating`,
}

// TurnFeedback records a person's rating of a completed turn against the release that
// produced it, and reports ratings per release. The giver is kept for audit only: no
// report names a person.
type TurnFeedback struct {
	DB port.DatabaseRepository
	// Roles authorizes the person to converse with the agent that answered.
	Roles contract.PrincipalAuthorizer
	// Authorizer authorizes Studio's report.
	Authorizer contract.StudioAuthorizer
	// MaxWindow bounds one report's span; default DefaultUsageWindow.
	MaxWindow time.Duration
	// Review queues a turn rated at or below its threshold for human review; nil queues none.
	// It is best-effort: a failure is logged and the stored rating stands. The rating
	// person is never passed on.
	Review contract.ProductionReviewIntake

	once sync.Once
	qs   port.QueryService
}

var (
	_ contract.TurnFeedbackRecorder  = (*TurnFeedback)(nil)
	_ contract.StudioFeedbackBackend = (*TurnFeedback)(nil)
)

func (f *TurnFeedback) queries(ctx context.Context) (port.QueryService, error) {
	if f.DB == nil {
		return nil, fmt.Errorf("turn feedback: database is required")
	}
	f.once.Do(func() { f.qs = f.DB.GetQueryService(ctx, feedbackQueries) })
	return f.qs, nil
}

func (f *TurnFeedback) RecordFeedback(ctx context.Context, tenant domain.TenantContext, person domain.PrincipalRef, requestID string, rating int, reason string) error {
	if tenant.TenantID <= 0 || person.ID == "" || strings.TrimSpace(requestID) == "" || rating < 1 || rating > 5 || len(reason) > 400 {
		return fmt.Errorf("%w: feedback needs a person, a request, a rating from 1 to 5, and at most 400 characters of reason", domain.ErrValidation)
	}
	if f.Roles == nil {
		return fmt.Errorf("turn feedback: role authorizer is required")
	}
	qs, err := f.queries(ctx)
	if err != nil {
		return err
	}
	turn, err := qs.Query(ctx, qFeedbackTurn, tenant.TenantID, requestID)
	if err != nil {
		return fmt.Errorf("load rated turn: %w", err)
	}
	if len(turn.Rows) == 0 {
		return fmt.Errorf("%w: turn %q", domain.ErrNotFound, requestID)
	}
	row := turn.Rows[0]
	if common.AsString(row[4]) != person.ID {
		return fmt.Errorf("%w: turn %q does not belong to this person", domain.ErrForbidden, requestID)
	}
	if common.AsString(row[2]) != "completed" {
		return fmt.Errorf("%w: only a completed turn is rated", domain.ErrConflict)
	}
	principal := domain.Principal{Kind: person.Kind, ID: person.ID, TenantID: tenant.TenantID, ScopeID: tenant.ScopeID}
	grant, err := f.Roles.Authorize(ctx, principal, ConversationAuthorizationObject, ConversationActionUse, "agent:"+common.AsString(row[3]))
	if err != nil {
		return fmt.Errorf("authorize feedback: %w", err)
	}
	if !grant.Allowed {
		return fmt.Errorf("%w: feedback on agent %q", domain.ErrForbidden, common.AsString(row[3]))
	}
	if _, err = qs.Query(ctx, qFeedbackPut, tenant.TenantID, common.AsString(row[0]), common.AsInt64(row[1]),
		string(person.Kind), person.ID, rating, nullableUsageField(reason)); err != nil {
		return fmt.Errorf("record feedback: %w", err)
	}
	if f.Review == nil {
		return nil
	}
	rated := domain.ReviewTurn{TenantContext: tenant, RequestID: requestID, AgentID: common.AsString(row[3]), AgentVersion: common.AsString(row[5])}
	if err = f.Review.TurnRated(ctx, rated, rating); err != nil {
		log.Printf("rated turn %s of tenant %d not queued for review: %v", requestID, tenant.TenantID, err)
	}
	return nil
}

// Feedback counts the ratings of [from, to) per release; the window spans at most MaxWindow.
func (f *TurnFeedback) Feedback(ctx context.Context, actor domain.StudioActor, agentID string, from, to time.Time) ([]domain.FeedbackSummary, error) {
	if f.Authorizer == nil {
		return nil, fmt.Errorf("turn feedback: studio authorizer is required")
	}
	if f.MaxWindow < 0 {
		return nil, fmt.Errorf("turn feedback: window limit must not be negative")
	}
	if err := checkReportWindow("feedback", from, to, positiveOr(f.MaxWindow, DefaultUsageWindow)); err != nil {
		return nil, err
	}
	if err := f.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	qs, err := f.queries(ctx)
	if err != nil {
		return nil, err
	}
	result, err := qs.Query(ctx, qFeedbackSummary, actor.TenantID, agentID, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("summarize feedback: %w", err)
	}
	var summaries []domain.FeedbackSummary
	for _, row := range result.Rows {
		release, rating := common.AsString(row[0]), common.AsInt64(row[1])
		if rating < 1 || rating > 5 {
			return nil, fmt.Errorf("summarize feedback: stored rating %d is out of range", rating)
		}
		if len(summaries) == 0 || summaries[len(summaries)-1].Release != release {
			summaries = append(summaries, domain.FeedbackSummary{Release: release})
		}
		summaries[len(summaries)-1].Counts[rating-1] = common.AsInt64(row[2])
	}
	return summaries, nil
}
