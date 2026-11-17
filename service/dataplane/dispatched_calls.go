package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
	"github.com/nauticana/scout/service/observability"
)

// dispatchedCalls rebuilds a turn's delivery and its calls as they were sent, and settles an open
// call from the reconciliation sources: the work an operator's lookup and the outcome sweep share.
// qs runs operateQueries.
type dispatchedCalls struct {
	qs         port.QueryService
	Outcomes   contract.CallOutcomeStore
	Journal    contract.LoopJournal
	Control    contract.TurnControl
	Principals contract.PrincipalResolver
	Reconciler contract.ToolOutcomeReconciler
	Audit      contract.AuditSink
	Now        func() time.Time
}

// dispatched rebuilds the acting identity a turn was admitted under from its queue row.
func (d dispatchedCalls) dispatched(ctx context.Context, tenantID int64, requestID string) (domain.TurnRequest, error) {
	dispatch, err := d.dispatch(ctx, tenantID, requestID)
	return dispatch.Turn, err
}

// dispatch rebuilds the turn's delivery from its queue row: its acting identity and reply route.
func (d dispatchedCalls) dispatch(ctx context.Context, tenantID int64, requestID string) (domain.TurnDispatch, error) {
	result, err := d.qs.Query(ctx, qOperateDispatch, tenantID, requestID)
	if err != nil {
		return domain.TurnDispatch{}, fmt.Errorf("read dispatch of turn %q: %w", requestID, err)
	}
	if len(result.Rows) == 0 || len(result.Rows[0]) < 4 {
		return domain.TurnDispatch{}, fmt.Errorf("%w: turn %q was never dispatched", domain.ErrConflict, requestID)
	}
	row := result.Rows[0]
	turn := domain.TurnRequest{
		TenantContext: domain.TenantContext{TenantID: tenantID}, RequestID: requestID,
		ConversationID: common.AsString(row[0]), AgentID: common.AsString(row[1]),
	}
	err = decodeActingContext(common.AsString(row[2]), &turn)
	return domain.TurnDispatch{Turn: turn, ReplyRoute: common.AsString(row[3])}, err
}

// settle asks the reconciler about one open call of a turn held or ended at fence and, when it
// answers definitely, resolves the call as resolver and audits it; otherwise the call is returned
// unchanged with settled false.
func (d dispatchedCalls) settle(ctx context.Context, outcome domain.CallOutcome, agentID string, fence int64, resolver domain.PrincipalRef) (domain.CallOutcome, bool, error) {
	if d.Reconciler == nil {
		return outcome, false, fmt.Errorf("%w: no call outcome reconciler is composed", domain.ErrNotReady)
	}
	call, err := d.call(ctx, outcome)
	if err != nil {
		return outcome, false, err
	}
	report, err := d.Reconciler.Reconcile(ctx, call)
	if err != nil {
		return outcome, false, fmt.Errorf("reconcile call %q: %w", outcome.Key.CallID, err)
	}
	settlement, definite := reportSettlement(outcome, report)
	if !definite {
		return outcome, false, nil
	}
	resolved, err := d.Outcomes.Resolve(ctx, settlement, resolver, fence)
	if err != nil {
		return outcome, false, err
	}
	return resolved, true, d.audit(ctx, resolver, outcome.Key, agentID, fence, observability.AuditCategoryCallOutcomeResolved, resolved, string(resolved.Resolution))
}

// call rebuilds the call as it was sent: the turn's acting identity at the conversation's pinned
// release, and the arguments the journal shows it ran with.
func (d dispatchedCalls) call(ctx context.Context, outcome domain.CallOutcome) (domain.ToolCall, error) {
	tenantID := outcome.Key.TenantID
	turn, err := d.dispatched(ctx, tenantID, outcome.Key.RequestID)
	if err != nil {
		return domain.ToolCall{}, err
	}
	pinned, err := d.qs.Query(ctx, qOperateRelease, tenantID, turn.ConversationID)
	if err != nil {
		return domain.ToolCall{}, fmt.Errorf("read release of conversation %q: %w", turn.ConversationID, err)
	}
	if len(pinned.Rows) == 0 {
		return domain.ToolCall{}, fmt.Errorf("%w: conversation %q has no pinned release", domain.ErrConflict, turn.ConversationID)
	}
	call := domain.ToolCall{
		TenantContext: turn.TenantContext, Principal: pinnedPrincipal(turn, common.AsString(pinned.Rows[0][0])), OnBehalfOf: turn.OnBehalfOf,
		RequestID: outcome.Key.RequestID, ConversationID: turn.ConversationID, ToolID: outcome.Tool.ToolID, ToolVersion: outcome.Tool.Version,
		IdempotencyKey: outcome.IdempotencyKey,
	}
	if outcome.Origin == domain.CallFromOperator {
		return d.operatorCall(ctx, call, outcome)
	}
	entries, err := d.Journal.Load(ctx, domain.LoopKey{TenantID: tenantID, RequestID: outcome.Key.RequestID, ExecutionStepID: outcome.Key.ExecutionStepID})
	if err != nil {
		return domain.ToolCall{}, fmt.Errorf("load loop journal: %w", err)
	}
	found := false
	for _, entry := range entries {
		switch {
		case entry.Kind == domain.LoopEntryModel && entry.Model != nil:
			for _, proposed := range entry.Model.ToolCalls {
				if proposed.CallID == outcome.Key.CallID {
					call.Arguments, call.RevisesProposal, found = proposed.Arguments, 0, true
				}
			}
		case entry.Kind == domain.LoopEntryRevision && entry.Revision != nil && entry.Observation != nil && entry.Observation.CallID == outcome.Key.CallID:
			call.Arguments, call.RevisesProposal = entry.Revision.Arguments, entry.Revision.RevisesProposal
		}
	}
	if !found {
		return domain.ToolCall{}, fmt.Errorf("%w: the journal of turn %q proposes no call %q", domain.ErrConflict, outcome.Key.RequestID, outcome.Key.CallID)
	}
	return call, nil
}

// operatorCall rebuilds a person's call from the entry that records it: its arguments, and the
// person, whose credential a lookup resolves as the call's own did.
func (d dispatchedCalls) operatorCall(ctx context.Context, call domain.ToolCall, outcome domain.CallOutcome) (domain.ToolCall, error) {
	tenantID := outcome.Key.TenantID
	entries, err := d.Control.OperatorEntries(ctx, tenantID, outcome.Key.RequestID)
	if err != nil {
		return domain.ToolCall{}, err
	}
	for _, entry := range entries {
		if use := entry.Body.Tool; use != nil && use.CallID == outcome.Key.CallID {
			if call.Operator, err = d.Principals.Resolve(ctx, tenantID, entry.Actor); err != nil {
				return domain.ToolCall{}, err
			}
			call.Arguments = use.Arguments
			return call, nil
		}
	}
	return domain.ToolCall{}, fmt.Errorf("%w: no operator entry of turn %q records call %q", domain.ErrConflict, outcome.Key.RequestID, outcome.Key.CallID)
}

// audit records one act on a call, keyed by the call and actor so each is recorded once.
func (d dispatchedCalls) audit(ctx context.Context, actor domain.PrincipalRef, key domain.CallOutcomeKey, agentID string, fence int64, category string, outcome domain.CallOutcome, reason string) error {
	if d.Audit == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"request_id": key.RequestID, "agent_id": agentID, "fence": fence,
		"execution_step_id": key.ExecutionStepID, "call_id": key.CallID, "tool_id": outcome.Tool.ToolID,
		"status": outcome.Status, "resolution": outcome.Resolution,
	})
	if err != nil {
		return fmt.Errorf("encode %s audit: %w", category, err)
	}
	scoped := domain.WithDecisionScope(context.WithoutCancel(ctx), "turn/", key.RequestID, "/fence/", fence,
		"/call/", key.ExecutionStepID, "/", key.CallID, "/by/", actor.Kind, ":", actor.ID)
	if err := d.Audit.Record(scoped, domain.DecisionRecord{
		TenantID: key.TenantID, Principal: actor, Category: category, Action: "call", Resource: agentID,
		Outcome: domain.DecisionAllow, Reason: reason, RequestID: key.RequestID, Payload: payload, OccurredAt: clk.Now(d.Now),
	}); err != nil {
		return fmt.Errorf("record %s: %w", category, err)
	}
	return nil
}
