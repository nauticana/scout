package approval

import (
	"context"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Resumer closes the human-in-the-loop cycle: it records the verdict, returns
// the suspended turn to the queue, and re-dispatches it. Resolving without
// re-dispatching would leave the turn parked forever, so the three steps belong
// together rather than in the store.
type Resumer struct {
	Store      contract.ApprovalStore
	Authorizer contract.ApprovalAuthorizer
	Records    contract.TurnRecordStore
	Dispatcher contract.TurnDispatcher
	// Audit is optional; when set every verdict becomes evidence.
	Audit contract.AuditSink
	Now   func() time.Time
}

// Resolve records the verdict and, when it permits the action, resumes the turn.
// A rejection fails the turn instead: the work must not continue unapproved.
func (r *Resumer) Resolve(ctx context.Context, verdict domain.ApprovalVerdict, dispatch domain.TurnDispatch) (domain.ApprovalRequest, error) {
	if r.Store == nil || r.Authorizer == nil || r.Records == nil || r.Dispatcher == nil {
		return domain.ApprovalRequest{}, fmt.Errorf("approval resumer: store, authorizer, turn records, and dispatcher are required")
	}
	request, err := r.Store.Get(ctx, verdict.RequestKey)
	if err != nil {
		return domain.ApprovalRequest{}, err
	}
	if err := r.Authorizer.AuthorizeApproval(ctx, request, verdict.Decider); err != nil {
		return domain.ApprovalRequest{}, err
	}
	request, err = r.Store.Resolve(ctx, verdict)
	if err != nil {
		return domain.ApprovalRequest{}, err
	}
	if err := r.record(ctx, request, verdict); err != nil {
		return request, err
	}
	key := verdict.RequestKey
	switch verdict.Status {
	case domain.ApprovalStatusApproved:
		if err := r.Records.Resume(ctx, key.TenantID, key.RequestID); err != nil {
			return request, err
		}
		if err := r.Dispatcher.Enqueue(ctx, dispatch); err != nil {
			return request, fmt.Errorf("re-dispatch resumed turn %q: %w", key.RequestID, err)
		}
	default:
		if err := r.Records.Fail(ctx, key.TenantID, key.RequestID, "failed", string(verdict.Status)); err != nil {
			return request, err
		}
	}
	return request, nil
}

func (r *Resumer) record(ctx context.Context, request domain.ApprovalRequest, verdict domain.ApprovalVerdict) error {
	if r.Audit == nil {
		return nil
	}
	outcome := domain.DecisionDeny
	if verdict.Status == domain.ApprovalStatusApproved {
		outcome = domain.DecisionAllow
	}
	return r.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: request.TenantID, Principal: verdict.Decider, Authority: verdict.Authority,
		ScopeID: request.ScopeID, Category: domain.DecisionCategoryApproval, Action: string(verdict.Status),
		Resource: request.Resource, Outcome: outcome, Reason: verdict.Reason,
		RequestID: request.RequestID, ConversationID: request.ConversationID, OccurredAt: r.now(),
	})
}

func (r *Resumer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
