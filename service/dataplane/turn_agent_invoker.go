package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// TurnAgentInvoker runs a delegated call as the delegate's own turn. The hop is
// admitted, reserved, and settled under the delegate's principal, within the
// budget its delegator passed down, so a chain settles per hop and the returned
// step reports no usage of its own: adding it would bill the delegator again.
type TurnAgentInvoker struct {
	Ingress contract.ConversationIngress
}

var _ contract.AgentInvoker = (*TurnAgentInvoker)(nil)

func (invoker *TurnAgentInvoker) Invoke(ctx context.Context, call domain.DelegatedCall) (domain.StepResult, error) {
	if invoker.Ingress == nil {
		return domain.StepResult{}, fmt.Errorf("turn agent invoker: ingress is required")
	}
	if call.Target.Kind != domain.PrincipalAgent || strings.TrimSpace(call.Target.ID) == "" || strings.TrimSpace(call.RequestID) == "" || call.Caller.TenantID <= 0 {
		return domain.StepResult{}, fmt.Errorf("%w: a delegated turn needs an agent target, a request id, and the caller's tenant", domain.ErrValidation)
	}
	scopeID := call.Bounds.ScopeID
	if scopeID == "" {
		scopeID = call.Caller.ScopeID
	}
	subscription, err := invoker.Ingress.OpenTurn(ctx, domain.TurnRequest{
		TenantContext: domain.TenantContext{TenantID: call.Caller.TenantID, ScopeID: scopeID},
		Principal: domain.Principal{
			Kind: call.Target.Kind, ID: call.Target.ID, TenantID: call.Caller.TenantID, ScopeID: scopeID,
			// The immediate delegator comes first, the original authority last.
			Authority: append(domain.AuthorityChain{call.Authority}, call.Caller.Authority...),
		},
		DelegationBounds: call.Bounds, WorkItemID: call.WorkItem.ID, WorkItemDepth: call.WorkItem.Depth,
		RequestID: call.RequestID, ConversationID: call.RequestID, AgentID: call.Target.ID, Input: call.Input,
	})
	if err != nil {
		return domain.StepResult{}, err
	}
	defer subscription.Close()
	var state []byte
	for {
		frame, err := subscription.Receive(ctx)
		if errors.Is(err, io.EOF) {
			return domain.StepResult{}, fmt.Errorf("%w: delegated turn %q ended without a final frame", domain.ErrConflict, call.RequestID)
		}
		if err != nil {
			return domain.StepResult{}, err
		}
		if len(frame.Payload) > 0 {
			state = frame.Payload
		}
		if !frame.Final {
			continue
		}
		// A delegate parked for approval parks its delegator too; the resumed step re-attaches to the same turn.
		if slices.ContainsFunc(frame.Events, func(event domain.TurnEvent) bool { return event.Kind == domain.TurnEventApprovalPending }) {
			return domain.StepResult{}, fmt.Errorf("%w: delegated turn of %q", domain.ErrApprovalPending, call.Target.ID)
		}
		if frame.ErrorCode != "" {
			return domain.StepResult{}, delegatedFailure(call, frame.ErrorCode)
		}
		return domain.StepResult{State: state}, nil
	}
}

// delegatedFailure keeps the classes a delegator must tell apart; the rest stay a class name.
func delegatedFailure(call domain.DelegatedCall, errorCode string) error {
	for class, sentinel := range map[string]error{
		"budget_exceeded": domain.ErrBudgetExceeded, "canceled": domain.ErrTurnCanceled,
		"execution_limit": domain.ErrExecutionLimit,
	} {
		if errorCode == class {
			return fmt.Errorf("%w: delegated turn of %q", sentinel, call.Target.ID)
		}
	}
	return fmt.Errorf("delegated turn of %q failed: %s", call.Target.ID, errorCode)
}
