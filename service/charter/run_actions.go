package charter

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/charter/sdk/evidence"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// RunActions reads one request's Charter actions in the tenant's namespace.
type RunActions struct {
	Evidence evidence.Provider
	// Namespace maps a tenant to the Charter namespace that owns its action records.
	Namespace func(tenantID int64) string
}

var _ contract.CharterActionSource = (*RunActions)(nil)

// RunActions returns the latest limit actions of the run, in time order.
func (r *RunActions) RunActions(ctx context.Context, tenantID int64, requestID string, limit int) ([]domain.CharterAction, error) {
	if r == nil || r.Evidence == nil || r.Namespace == nil {
		return nil, fmt.Errorf("%w: run actions need evidence and a namespace", domain.ErrNotReady)
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || limit <= 0 {
		return nil, fmt.Errorf("%w: tenant, request id, and a positive limit are required", domain.ErrValidation)
	}
	namespace := r.Namespace(tenantID)
	if strings.TrimSpace(namespace) == "" {
		return nil, fmt.Errorf("%w: tenant %d has no Charter namespace", domain.ErrNotReady, tenantID)
	}
	records, err := (evidence.Queries{Provider: r.Evidence}).ActionsIn(ctx, namespace, requestID, limit)
	if err != nil {
		return nil, fmt.Errorf("read charter actions of request %q: %w", requestID, err)
	}
	actions := make([]domain.CharterAction, 0, len(records))
	for _, record := range records {
		if record.Namespace != namespace || record.RuntimeContext.ExecutionContextID != requestID {
			continue
		}
		actions = append(actions, charterAction(record))
	}
	return actions, nil
}

// charterAction keeps the disposition and evaluation results; reasons, inputs, and subjects stay in Charter.
func charterAction(record model.ActionRecord) domain.CharterAction {
	action := domain.CharterAction{
		ActionID:   domain.CharterRef{Namespace: record.Namespace, ID: record.ID},
		Capability: charterRef(record.CapabilityID), OperationClass: record.OperationClass,
		Disposition: record.Disposition, Outcome: record.Outcome,
		Assignment: optionalRef(record.AssignmentID), Reconciles: optionalRef(record.ReconcilesActionID),
		SodException: optionalRef(record.SodExceptionID), MaterialInputsDigest: record.MaterialInputsDigest,
		At: record.ActionTime,
	}
	for _, evaluation := range record.AuthorityEvaluations {
		action.Authority = append(action.Authority, domain.CharterEvaluation{Ref: optionalRef(evaluation.AuthorityGrantID), Result: evaluation.Result})
	}
	for _, evaluation := range record.ApprovalEvaluations {
		action.Approvals = append(action.Approvals, domain.CharterEvaluation{Ref: optionalRef(evaluation.ApprovalID), Result: evaluation.Result})
	}
	return action
}

func charterRef(ref model.Ref) domain.CharterRef {
	return domain.CharterRef{Namespace: ref.Namespace, ID: ref.ID}
}

func optionalRef(ref *model.Ref) *domain.CharterRef {
	if ref == nil {
		return nil
	}
	converted := charterRef(*ref)
	return &converted
}
