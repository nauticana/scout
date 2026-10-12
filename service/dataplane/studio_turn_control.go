package dataplane

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/observability"
)

const (
	qOperateLiveTurns = "scout_operate_live_turns"
	qOperateTurn      = "scout_operate_turn"
	qOperateDispatch  = "scout_operate_dispatch"
	qOperateRelease   = "scout_operate_release"

	// DefaultLiveTurnPageSize and DefaultLiveTurnMaxPageSize bound one Studio page of live turns.
	DefaultLiveTurnPageSize    = 50
	DefaultLiveTurnMaxPageSize = 200
	maxLiveTurnPageSize        = 1000
)

const liveTurnColumns = `
SELECT turn.request_id, turn.conversation_id, turn.turn_no, turn.status_code, turn.fence,
       COALESCE(turn.hold_code, ''), turn.hold_requested_at IS NOT NULL, COALESCE(turn.hold_reason, ''),
       turn.cancel_requested_at IS NOT NULL, turn.queued_at, turn.started_at, turn.held_at,
       COALESCE(turn.holder_kind, ''), COALESCE(turn.holder_id, ''), turn.hold_until
  FROM conversation_turn turn
  JOIN agent_conversation conv ON conv.partner_id = turn.partner_id AND conv.conversation_id = turn.conversation_id`

var operateQueries = map[string]string{
	qOperateLiveTurns: liveTurnColumns + `
 WHERE turn.partner_id = ? AND conv.agent_id = ? AND turn.status_code IN (` + sqlTurnLive + `)
   AND (? = '' OR turn.queued_at < ? OR turn.queued_at = ? AND turn.request_id < ?)
 ORDER BY turn.queued_at DESC, turn.request_id DESC
 LIMIT ?`,
	qOperateTurn: liveTurnColumns + `
 WHERE turn.partner_id = ? AND conv.agent_id = ? AND turn.request_id = ?`,
	// The acting context the turn was admitted under, which its re-admission is charged to, and
	// the route its replies go to.
	qOperateDispatch: `
SELECT conversation_id, agent_id, COALESCE(acting_context, ''), reply_route
  FROM turn_queue
 WHERE partner_id = ? AND request_id = ?`,
	qOperateRelease: `
SELECT agent_version, COALESCE(end_user_ref, '') FROM agent_conversation WHERE partner_id = ? AND conversation_id = ?`,
}

// StudioTurnControl lets an operator see an agent's live turns, hold them, settle their unknown
// call outcomes, release held ones, and take one over: act in it under a lease, then hand it back
// or finish it. Every call needs AGENT_STUDIO VIEW on the agent, which an unviewable agent fails as
// not found, and AGENT_OPERATION VIEW to read or OPERATE to change on agent:<id>; a preempting hold
// also needs PREEMPT and an attestation RECONCILE. A release or handback re-admits the turn's
// budget before it requeues the turn, and releases only a reservation it newly took when the
// transition itself is refused.
type StudioTurnControl struct {
	DB         port.DatabaseRepository
	Authorizer contract.StudioAuthorizer
	Principals contract.PrincipalResolver
	Roles      contract.PrincipalAuthorizer
	Control    contract.TurnControl
	Estimator  contract.TurnBudgetEstimator
	Budget     contract.TenantBudgetManager
	Outcomes   contract.CallOutcomeStore
	// Journal reads the arguments a call ran with, which a lookup reconciles by.
	Journal contract.LoopJournal
	// Reconciler looks an open call up; nil answers every lookup domain.ErrNotReady.
	Reconciler contract.ToolOutcomeReconciler
	// Separation decides who may attest; an attestation without it fails closed.
	Separation contract.OperationSeparation
	// Evidence verifies an attestation's evidence object; evidence without it is refused.
	Evidence contract.EvidenceObjectVerifier
	// Audit records each hold, release, attestation, resolution, and operator act; nil records none.
	Audit  contract.AuditSink
	Limits domain.LiveTurnLimits
	Now    func() time.Time

	// The operator's tool calls need Tools, the release's bound tools; Gateway, the governed gateway
	// they go through; OperatorBudget, which admits each against the tenant's window; and Dispatch,
	// which bounds how long one may stay in flight. Without them a tool act is domain.ErrNotReady.
	Tools          contract.ToolRegistry
	Gateway        contract.GovernedToolGateway
	OperatorBudget contract.OperatorBudget
	Dispatch       domain.ToolDispatchWindow
	// A reply or finish passes Guardrails under the release's GuardrailConfigs (nil reads an empty
	// policy) and, when Definitions shows the release requires evidence, Claims; Publisher sends its
	// frame. Without Guardrails, Definitions, or Publisher a reply is domain.ErrNotReady.
	Guardrails       contract.GuardrailEnforcer
	GuardrailConfigs contract.GuardrailConfigRepository
	Definitions      contract.DefinitionResolver
	Claims           contract.EvidenceValidator
	Publisher        contract.TurnReplyPublisher

	once sync.Once
	qs   port.QueryService
}

var _ contract.StudioTurnControlBackend = (*StudioTurnControl)(nil)

func (s *StudioTurnControl) ready(ctx context.Context) (port.QueryService, error) {
	if s.DB == nil || s.Authorizer == nil || s.Principals == nil || s.Roles == nil || s.Control == nil || s.Estimator == nil || s.Budget == nil ||
		s.Outcomes == nil || s.Journal == nil {
		return nil, fmt.Errorf("studio turn control: database, authorizers, principal resolver, turn control, estimator, budget, call outcomes, and journal are required")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, operateQueries) })
	return s.qs, nil
}

func (s *StudioTurnControl) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// authorize fails closed unless the actor may view the agent in Studio and holds every action
// on AGENT_OPERATION for it.
func (s *StudioTurnControl) authorize(ctx context.Context, actor domain.StudioActor, action domain.OperationAction, agentID string, more ...domain.OperationAction) error {
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("%w: agent_name is required", domain.ErrValidation)
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			return fmt.Errorf("%w: agent %q", domain.ErrNotFound, agentID)
		}
		return err
	}
	principal, err := s.Principals.Resolve(ctx, actor.TenantID, actor.Principal)
	if err != nil {
		return err
	}
	for _, required := range append([]domain.OperationAction{action}, more...) {
		grant, err := s.Roles.Authorize(ctx, principal, domain.OperationAuthorizationObject, string(required), "agent:"+agentID)
		if err != nil {
			return fmt.Errorf("authorize %s %s: %w", domain.OperationAuthorizationObject, required, err)
		}
		if !grant.Allowed {
			return fmt.Errorf("%w: %s %s on agent %q", domain.ErrForbidden, domain.OperationAuthorizationObject, required, agentID)
		}
	}
	return nil
}

func (s *StudioTurnControl) limits() (domain.LiveTurnLimits, error) {
	limits := domain.LiveTurnLimits{
		PageSize: cmp.Or(s.Limits.PageSize, DefaultLiveTurnPageSize), MaxPageSize: cmp.Or(s.Limits.MaxPageSize, DefaultLiveTurnMaxPageSize),
	}
	if s.Limits.PageSize < 0 || s.Limits.MaxPageSize < 0 || limits.MaxPageSize > maxLiveTurnPageSize || limits.PageSize > limits.MaxPageSize {
		return domain.LiveTurnLimits{}, fmt.Errorf("%w: live turn page size %d must lie within 1..%d and its maximum within 1..%d",
			domain.ErrValidation, limits.PageSize, limits.MaxPageSize, maxLiveTurnPageSize)
	}
	return limits, nil
}

// LiveTurns pages the agent's non-terminal turns, newest first.
func (s *StudioTurnControl) LiveTurns(ctx context.Context, actor domain.StudioActor, filter domain.LiveTurnFilter) (domain.LiveTurnPage, error) {
	qs, err := s.ready(ctx)
	if err != nil {
		return domain.LiveTurnPage{}, err
	}
	limits, err := s.limits()
	if err != nil {
		return domain.LiveTurnPage{}, err
	}
	if filter.Limit < 0 || filter.Limit > limits.MaxPageSize {
		return domain.LiveTurnPage{}, fmt.Errorf("%w: limit must lie within 0..%d", domain.ErrValidation, limits.MaxPageSize)
	}
	beforeAt, beforeRequest, err := decodeLiveTurnCursor(filter.Before)
	if err != nil {
		return domain.LiveTurnPage{}, err
	}
	if err = s.authorize(ctx, actor, domain.OperationView, filter.AgentID); err != nil {
		return domain.LiveTurnPage{}, err
	}
	limit := cmp.Or(filter.Limit, limits.PageSize)
	result, err := qs.Query(ctx, qOperateLiveTurns, actor.TenantID, filter.AgentID, filter.Before, beforeAt, beforeAt, beforeRequest, limit+1)
	if err != nil {
		return domain.LiveTurnPage{}, fmt.Errorf("list live turns of %q: %w", filter.AgentID, err)
	}
	page := domain.LiveTurnPage{Turns: make([]domain.LiveTurn, 0, min(len(result.Rows), limit))}
	for index, row := range result.Rows {
		if index == limit {
			last := page.Turns[limit-1]
			page.NextBefore = strconv.FormatInt(last.QueuedAt.UnixNano(), 10) + ":" + last.RequestID
			break
		}
		turn, err := decodeLiveTurn(row)
		if err != nil {
			return domain.LiveTurnPage{}, err
		}
		page.Turns = append(page.Turns, turn)
	}
	return page, nil
}

func decodeLiveTurnCursor(before string) (time.Time, string, error) {
	if before == "" {
		return time.Unix(0, 0).UTC(), "", nil
	}
	nanos, requestID, found := strings.Cut(before, ":")
	at, err := strconv.ParseInt(nanos, 10, 64)
	if !found || err != nil || strings.TrimSpace(requestID) == "" {
		return time.Time{}, "", fmt.Errorf("%w: before is not a live turn cursor", domain.ErrValidation)
	}
	return time.Unix(0, at).UTC(), requestID, nil
}

func decodeLiveTurn(row []any) (domain.LiveTurn, error) {
	if len(row) < 15 {
		return domain.LiveTurn{}, fmt.Errorf("decode live turn: expected 15 columns, got %d", len(row))
	}
	turn := domain.LiveTurn{
		RequestID: common.AsString(row[0]), ConversationID: common.AsString(row[1]), TurnNo: common.AsInt64(row[2]),
		Status: domain.TurnStatus(common.AsString(row[3])), Fence: common.AsInt64(row[4]), HoldCode: domain.TurnHoldCode(common.AsString(row[5])),
		HoldRequested: common.AsBool(row[6]), HoldReason: common.AsString(row[7]), CancelRequested: common.AsBool(row[8]),
	}
	turn.QueuedAt, _ = common.AsTimeOK(row[9])
	turn.StartedAt, _ = common.AsTimeOK(row[10])
	turn.HeldAt, _ = common.AsTimeOK(row[11])
	turn.Holder = domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[12])), ID: common.AsString(row[13])}
	turn.HoldUntil, _ = common.AsTimeOK(row[14])
	return turn, nil
}

// Turn reads one turn of the agent with its call outcomes and operator entries; a turn of another
// agent is not found.
func (s *StudioTurnControl) Turn(ctx context.Context, actor domain.StudioActor, agentID, requestID string) (domain.LiveTurn, error) {
	if _, err := s.ready(ctx); err != nil {
		return domain.LiveTurn{}, err
	}
	if err := s.authorize(ctx, actor, domain.OperationView, agentID); err != nil {
		return domain.LiveTurn{}, err
	}
	return s.turnWithOutcomes(ctx, actor.TenantID, agentID, requestID)
}

func (s *StudioTurnControl) turnWithOutcomes(ctx context.Context, tenantID int64, agentID, requestID string) (domain.LiveTurn, error) {
	turn, err := s.turn(ctx, tenantID, agentID, requestID)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	if turn.Outcomes, err = s.Outcomes.List(ctx, tenantID, requestID); err != nil {
		return domain.LiveTurn{}, err
	}
	if turn.Entries, err = s.Control.OperatorEntries(ctx, tenantID, requestID); err != nil {
		return domain.LiveTurn{}, err
	}
	return turn, nil
}

func (s *StudioTurnControl) turn(ctx context.Context, tenantID int64, agentID, requestID string) (domain.LiveTurn, error) {
	if strings.TrimSpace(requestID) == "" {
		return domain.LiveTurn{}, fmt.Errorf("%w: request_id is required", domain.ErrValidation)
	}
	result, err := s.qs.Query(ctx, qOperateTurn, tenantID, agentID, requestID)
	if err != nil {
		return domain.LiveTurn{}, fmt.Errorf("read turn %q: %w", requestID, err)
	}
	if len(result.Rows) == 0 {
		return domain.LiveTurn{}, fmt.Errorf("%w: turn %q of agent %q", domain.ErrNotFound, requestID, agentID)
	}
	return decodeLiveTurn(result.Rows[0])
}

// Hold holds a queued or parked turn of the agent at once and, in checkpoint mode, asks a running
// one to hold at its next durable checkpoint; in preempt mode a running one is held at once too,
// fencing out its worker.
func (s *StudioTurnControl) Hold(ctx context.Context, actor domain.StudioActor, request domain.TurnHoldRequest) (domain.TurnHoldOutcome, error) {
	if _, err := s.ready(ctx); err != nil {
		return domain.TurnHoldOutcome{}, err
	}
	reason := strings.TrimSpace(request.Reason)
	if request.Mode != domain.HoldModeCheckpoint && request.Mode != domain.HoldModePreempt || reason == "" {
		return domain.TurnHoldOutcome{}, fmt.Errorf("%w: a hold needs the mode %q or %q and a reason", domain.ErrValidation, domain.HoldModeCheckpoint, domain.HoldModePreempt)
	}
	var more []domain.OperationAction
	if request.Mode == domain.HoldModePreempt {
		more = append(more, domain.OperationPreempt)
	}
	if err := s.authorize(ctx, actor, domain.OperationOperate, request.AgentID, more...); err != nil {
		return domain.TurnHoldOutcome{}, err
	}
	if _, err := s.turn(ctx, actor.TenantID, request.AgentID, request.RequestID); err != nil {
		return domain.TurnHoldOutcome{}, err
	}
	var outcome domain.TurnHoldOutcome
	var err error
	if request.Mode == domain.HoldModePreempt {
		outcome, err = s.Control.Preempt(ctx, actor.TenantID, request.RequestID)
	} else {
		outcome, err = s.Control.RequestHold(ctx, actor.TenantID, request.RequestID, domain.HoldCheckpoint, reason)
	}
	if err != nil {
		return domain.TurnHoldOutcome{}, err
	}
	category := observability.AuditCategoryTurnHoldRequested
	if outcome.Held() {
		category = observability.AuditCategoryTurnHeld
	}
	return outcome, auditTurnControl(ctx, s.Audit, actor, request.AgentID, request.RequestID, category, outcome.Fence, reason, s.now())
}

// Release re-admits the held turn's budget and then returns it to the queue under a new fence.
// A refused re-admission leaves the turn held; a refused release, such as a stale fence,
// releases only a reservation this call newly took.
func (s *StudioTurnControl) Release(ctx context.Context, actor domain.StudioActor, request domain.TurnReleaseRequest) (domain.LiveTurn, error) {
	qs, err := s.ready(ctx)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	if request.Fence <= 0 {
		return domain.LiveTurn{}, fmt.Errorf("%w: the held fence is required", domain.ErrValidation)
	}
	if err = s.authorize(ctx, actor, domain.OperationOperate, request.AgentID); err != nil {
		return domain.LiveTurn{}, err
	}
	held, err := s.turn(ctx, actor.TenantID, request.AgentID, request.RequestID)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	if held.Status != domain.TurnHeld || held.Fence != request.Fence {
		return domain.LiveTurn{}, fmt.Errorf("%w: turn %q is %s at fence %d, not held at %d", domain.ErrConflict, request.RequestID, held.Status, held.Fence, request.Fence)
	}
	turn, err := s.dispatched(ctx, qs, actor.TenantID, request.RequestID)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	quote, err := s.Estimator.Estimate(ctx, turn)
	if err != nil {
		return domain.LiveTurn{}, fmt.Errorf("estimate turn budget: %w", err)
	}
	// A held turn's reservation is settled only with its end, so a settled one refuses the release.
	reservation, err := s.Budget.Reserve(ctx, turnBudgetRequest(turn, quote))
	if err != nil {
		return domain.LiveTurn{}, fmt.Errorf("re-admit held turn: %w", err)
	}
	fence, err := s.Control.Release(ctx, actor.TenantID, request.RequestID, request.Fence)
	if err != nil {
		if reservation.ReservationID != "" && !reservation.Replayed {
			err = errors.Join(err, s.Budget.Release(context.WithoutCancel(ctx), reservation))
		}
		return domain.LiveTurn{}, err
	}
	if err = auditTurnControl(ctx, s.Audit, actor, request.AgentID, request.RequestID, observability.AuditCategoryTurnReleased, fence, "released", s.now()); err != nil {
		return domain.LiveTurn{}, err
	}
	return s.turn(ctx, actor.TenantID, request.AgentID, request.RequestID)
}

// dispatched rebuilds the acting identity a turn was admitted under from its queue row.
func (s *StudioTurnControl) dispatched(ctx context.Context, qs port.QueryService, tenantID int64, requestID string) (domain.TurnRequest, error) {
	dispatch, err := s.dispatch(ctx, qs, tenantID, requestID)
	return dispatch.Turn, err
}

// dispatch rebuilds the turn's delivery from its queue row: its acting identity and reply route.
func (s *StudioTurnControl) dispatch(ctx context.Context, qs port.QueryService, tenantID int64, requestID string) (domain.TurnDispatch, error) {
	result, err := qs.Query(ctx, qOperateDispatch, tenantID, requestID)
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
