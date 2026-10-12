package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
	"github.com/nauticana/scout/service/observability"
)

// operatorAdmissionTokens is the hold one operator tool call asks of the tenant's window before it
// leaves: a call quotes no tokens, so the hold admits it only while the window has room, and the
// call settles at what it spent, overrun included.
const operatorAdmissionTokens = 1

// TakeOver takes a turn held at the fence the actor saw: the actor holds it under a store-clock
// lease and no worker runs it until they hand it back, finish it, or the lease lapses.
func (s *StudioTurnControl) TakeOver(ctx context.Context, actor domain.StudioActor, request domain.TurnTakeoverRequest) (domain.LiveTurn, error) {
	if _, err := s.ready(ctx); err != nil {
		return domain.LiveTurn{}, err
	}
	if request.Fence <= 0 {
		return domain.LiveTurn{}, fmt.Errorf("%w: the held fence is required", domain.ErrValidation)
	}
	if err := operatingPerson(actor); err != nil {
		return domain.LiveTurn{}, err
	}
	if err := s.authorize(ctx, actor, domain.OperationOperate, request.AgentID); err != nil {
		return domain.LiveTurn{}, err
	}
	held, err := s.turn(ctx, actor.TenantID, request.AgentID, request.RequestID)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	if held.Status != domain.TurnHeld || held.Fence != request.Fence {
		return domain.LiveTurn{}, fmt.Errorf("%w: turn %q is %s at fence %d, not held at %d", domain.ErrConflict, request.RequestID, held.Status, held.Fence, request.Fence)
	}
	hold, err := s.Control.TakeOver(ctx, domain.TurnTakeover{TenantID: actor.TenantID, RequestID: request.RequestID, Fence: request.Fence, Holder: actor.Principal})
	if err != nil {
		return domain.LiveTurn{}, err
	}
	if err = auditTurnControl(ctx, s.Audit, actor, request.AgentID, request.RequestID, observability.AuditCategoryTurnTakenOver, hold.Lease.Fence, "taken over", s.now()); err != nil {
		return domain.LiveTurn{}, err
	}
	return s.turnWithOutcomes(ctx, actor.TenantID, request.AgentID, request.RequestID)
}

// operatingPerson refuses an actor that is not a person: only people operate turns.
func operatingPerson(actor domain.StudioActor) error {
	if actor.Principal.Kind != domain.PrincipalHuman || strings.TrimSpace(actor.Principal.ID) == "" {
		return fmt.Errorf("%w: only a person operates a turn", domain.ErrForbidden)
	}
	return nil
}

// operated authorizes the actor and reads the turn they operate at fence; any other state, fence,
// or holder is domain.ErrConflict. The store's guarded touch checks the lease again.
func (s *StudioTurnControl) operated(ctx context.Context, actor domain.StudioActor, agentID, requestID string, fence int64) (domain.LiveTurn, domain.OperatorLease, error) {
	if _, err := s.ready(ctx); err != nil {
		return domain.LiveTurn{}, domain.OperatorLease{}, err
	}
	if fence <= 0 {
		return domain.LiveTurn{}, domain.OperatorLease{}, fmt.Errorf("%w: the operated fence is required", domain.ErrValidation)
	}
	if err := operatingPerson(actor); err != nil {
		return domain.LiveTurn{}, domain.OperatorLease{}, err
	}
	if err := s.authorize(ctx, actor, domain.OperationOperate, agentID); err != nil {
		return domain.LiveTurn{}, domain.OperatorLease{}, err
	}
	turn, err := s.turn(ctx, actor.TenantID, agentID, requestID)
	if err != nil {
		return domain.LiveTurn{}, domain.OperatorLease{}, err
	}
	if turn.Status != domain.TurnOperated || turn.Fence != fence || turn.Holder != actor.Principal {
		return domain.LiveTurn{}, domain.OperatorLease{}, fmt.Errorf("%w: turn %q is %s at fence %d, not operated by %s at %d",
			domain.ErrConflict, requestID, turn.Status, turn.Fence, actor.Principal.ID, fence)
	}
	return turn, domain.OperatorLease{TenantID: actor.TenantID, RequestID: requestID, Fence: fence, Holder: actor.Principal}, nil
}

// Act appends one entry to a turn the actor operates: a note, a withdrawal, a guarded reply
// published to the end user, or a tool call through the governed gateway.
func (s *StudioTurnControl) Act(ctx context.Context, actor domain.StudioActor, request domain.TurnActRequest) (domain.TurnActResult, error) {
	_, lease, err := s.operated(ctx, actor, request.AgentID, request.RequestID, request.Fence)
	if err != nil {
		return domain.TurnActResult{}, err
	}
	act := domain.OperatorAct{Lease: lease, Kind: request.Kind}
	result := domain.TurnActResult{}
	switch request.Kind {
	case domain.OperatorNote:
		act.Body.Text = strings.TrimSpace(request.Text)
	case domain.OperatorWithdraw:
		act.Body.CallID = strings.TrimSpace(request.CallID)
	case domain.OperatorReply:
		frame, err := s.guardedReply(ctx, actor, request.RequestID, []byte(request.Text))
		if err != nil {
			return domain.TurnActResult{}, err
		}
		act.Body.Frame = &frame
	case domain.OperatorTool:
		return s.toolAct(ctx, actor, request, lease)
	default:
		return domain.TurnActResult{}, fmt.Errorf("%w: an act is a note, a reply, a tool call, or a withdrawal", domain.ErrValidation)
	}
	acted, err := s.Control.Act(ctx, act)
	if err != nil {
		return domain.TurnActResult{}, err
	}
	result.Entry = acted.Entry
	if acted.Entry.Body.Frame != nil {
		result.Published = s.publishReply(ctx, *acted.Entry.Body.Frame)
	}
	if err = s.auditEntry(ctx, actor, request.AgentID, acted.Entry, ""); err != nil {
		return domain.TurnActResult{}, err
	}
	result.Turn, err = s.turn(ctx, actor.TenantID, request.AgentID, request.RequestID)
	return result, err
}

// toolAct makes the actor's tool call. Its authority is decision 5's intersection: the tool must be
// bound to the conversation's pinned release, and the gateway authorizes the turn's own acting
// principal and authority chain, decides policy as that principal and as the person, applies the
// release's guardrails, and resolves the person's credential. The call is admitted against the
// tenant's window and settled at what it spent, never against the agent's release budget.
func (s *StudioTurnControl) toolAct(ctx context.Context, actor domain.StudioActor, request domain.TurnActRequest, lease domain.OperatorLease) (domain.TurnActResult, error) {
	if s.Tools == nil || s.Gateway == nil || s.OperatorBudget == nil {
		return domain.TurnActResult{}, fmt.Errorf("%w: operator tool calls need a tool registry, a governed gateway, and an operator budget", domain.ErrNotReady)
	}
	if err := s.Dispatch.Validate(); err != nil {
		return domain.TurnActResult{}, fmt.Errorf("%w: operator tool calls need a dispatch window: %w", domain.ErrNotReady, err)
	}
	if strings.TrimSpace(request.Tool.ToolID) == "" || strings.TrimSpace(request.Tool.Version) == "" {
		return domain.TurnActResult{}, fmt.Errorf("%w: a tool call names a tool version", domain.ErrValidation)
	}
	pinned, err := s.pinnedTurn(ctx, actor.TenantID, request.RequestID)
	if err != nil {
		return domain.TurnActResult{}, err
	}
	agent := pinnedPrincipal(pinned.turn, pinned.version)
	definition, err := s.boundTool(ctx, actor.TenantID, pinned, request.Tool)
	if err != nil {
		return domain.TurnActResult{}, err
	}
	operator, err := s.Principals.Resolve(ctx, actor.TenantID, actor.Principal)
	if err != nil {
		return domain.TurnActResult{}, err
	}
	quote, err := s.Estimator.Estimate(ctx, pinned.turn)
	if err != nil {
		return domain.TurnActResult{}, fmt.Errorf("estimate turn budget: %w", err)
	}
	reservation, err := s.OperatorBudget.ReserveOperatorAction(ctx, domain.BudgetRequest{
		TenantID: actor.TenantID, RequestID: request.RequestID, Principal: actor.Principal,
		Tokens: operatorAdmissionTokens, Currency: quote.Currency,
	})
	if err != nil {
		return domain.TurnActResult{}, fmt.Errorf("admit operator tool call: %w", err)
	}
	intent, err := s.Control.IntendCall(ctx, lease, request.Tool, s.Dispatch.For(definition))
	if err != nil {
		return domain.TurnActResult{}, errors.Join(err, s.OperatorBudget.Release(context.WithoutCancel(ctx), reservation))
	}
	call := domain.ToolCall{
		TenantContext: pinned.turn.TenantContext, Principal: agent, Operator: operator, OnBehalfOf: pinned.onBehalfOf,
		RequestID: request.RequestID, ConversationID: pinned.turn.ConversationID, ToolID: request.Tool.ToolID, ToolVersion: request.Tool.Version,
		Arguments: request.Arguments, IdempotencyKey: intent.IdempotencyKey,
	}
	scoped := domain.WithDecisionScope(ctx, "turn/", request.RequestID, "/fence/", lease.Fence, "/op/", intent.EntryNo)
	toolResult, callErr := s.Gateway.Invoke(scoped, call)
	settleCtx := context.WithoutCancel(ctx)
	if errors.Is(callErr, domain.ErrNotSent) || errors.Is(callErr, domain.ErrApprovalPending) {
		// The call never left: nothing was spent and no entry records it.
		retractErr := s.Outcomes.Retract(settleCtx, intent.Key, lease.Fence)
		return domain.TurnActResult{}, errors.Join(callErr, retractErr, s.OperatorBudget.Release(settleCtx, reservation))
	}
	usage := toolResult.Usage
	usage.ToolCalls, usage.Models = max(usage.ToolCalls, 1), nil
	if usage.Currency == "" {
		usage.Currency = reservation.Currency
	}
	use, settlement := operatorCallOutcome(intent, request, toolResult, callErr)
	acted, err := s.Control.Act(settleCtx, domain.OperatorAct{
		Lease: lease, Kind: domain.OperatorTool, Body: domain.OperatorEntryBody{Tool: &use}, Call: &intent, Settlement: settlement,
		Reservation: reservation, Usage: usage,
	})
	if err != nil {
		if errors.Is(err, domain.ErrFenceLost) && settlement != nil {
			// The lease is gone: the answer is evidence about the call, not an act in the turn.
			late := *settlement
			late.Resolution = domain.ResolvedByWorker
			if reportErr := s.Outcomes.Report(settleCtx, late, lease.Fence); reportErr != nil && !errors.Is(reportErr, domain.ErrConflict) {
				err = errors.Join(err, reportErr)
			}
		}
		return domain.TurnActResult{}, err
	}
	if err = s.auditEntry(ctx, actor, request.AgentID, acted.Entry, string(use.Status)); err != nil {
		return domain.TurnActResult{}, err
	}
	if acted.HeldFence > 0 {
		if err = auditTurnControl(ctx, s.Audit, actor, request.AgentID, request.RequestID, observability.AuditCategoryTurnHeld, acted.HeldFence,
			stage.ErrorClass(domain.ErrOutcomeUnknown), s.now()); err != nil {
			return domain.TurnActResult{}, err
		}
	}
	result := domain.TurnActResult{Entry: acted.Entry}
	outcome, found, err := s.Outcomes.Find(ctx, intent.Key)
	if err != nil {
		return domain.TurnActResult{}, err
	}
	if found {
		result.Outcome = &outcome
	}
	result.Turn, err = s.turn(ctx, actor.TenantID, request.AgentID, request.RequestID)
	return result, err
}

// operatorCallOutcome is what the gateway's answer makes of an operator call that left: a definite
// settlement with the use the entry records, or none for a call nobody can establish. A call
// answered asynchronously is unknown too: an operator call is not looked up later on its own.
func operatorCallOutcome(intent domain.OperatorCallIntent, request domain.TurnActRequest, result domain.ToolResult, err error) (domain.OperatorToolUse, *domain.CallSettlement) {
	use := domain.OperatorToolUse{
		CallID: intent.Key.CallID, ToolID: request.Tool.ToolID, Version: request.Tool.Version, Arguments: rawJSON(request.Arguments),
		Status: domain.CallUnknown,
	}
	settle := func(status domain.CallOutcomeStatus, record domain.CallRecord) (domain.OperatorToolUse, *domain.CallSettlement) {
		use.Status, use.Output, use.IsError, use.ResourceURIs = status, rawJSON(record.Output), record.IsError, record.ResourceURIs
		return use, &domain.CallSettlement{Key: intent.Key, Status: status, Resolution: domain.ResolvedByGateway, Record: record}
	}
	switch {
	case err == nil && result.Acceptance != nil:
		return use, nil
	case err == nil:
		return settle(domain.CallCompleted, resultRecord(result))
	case errors.Is(err, domain.ErrOutputWithheld):
		return settle(domain.CallCompleted, failedResult(err, result))
	case !errors.Is(err, domain.ErrEffectUnknown) && (errors.Is(err, domain.ErrNoEffect) || errors.Is(err, domain.ErrEffectViolated)):
		return settle(domain.CallFailed, failedResult(err, result))
	}
	return use, nil
}

// pinnedTurnContext is the turn as it was admitted, at its conversation's pinned release.
type pinnedTurnContext struct {
	turn       domain.TurnRequest
	route      string
	version    string
	onBehalfOf domain.PrincipalRef
}

func (s *StudioTurnControl) pinnedTurn(ctx context.Context, tenantID int64, requestID string) (pinnedTurnContext, error) {
	dispatch, err := s.dispatch(ctx, s.qs, tenantID, requestID)
	if err != nil {
		return pinnedTurnContext{}, err
	}
	pinned, err := s.qs.Query(ctx, qOperateRelease, tenantID, dispatch.Turn.ConversationID)
	if err != nil {
		return pinnedTurnContext{}, fmt.Errorf("read release of conversation %q: %w", dispatch.Turn.ConversationID, err)
	}
	if len(pinned.Rows) == 0 || len(pinned.Rows[0]) < 2 || strings.TrimSpace(common.AsString(pinned.Rows[0][0])) == "" {
		return pinnedTurnContext{}, fmt.Errorf("%w: conversation %q has no pinned release", domain.ErrConflict, dispatch.Turn.ConversationID)
	}
	version := common.AsString(pinned.Rows[0][0])
	return pinnedTurnContext{
		turn: dispatch.Turn, route: dispatch.ReplyRoute, version: version,
		onBehalfOf: actingFor(dispatch.Turn, domain.SessionSnapshot{EndUserRef: common.AsString(pinned.Rows[0][1])}),
	}, nil
}

// boundTool is the release's binding of the tool the person calls; a tool the release does not
// bind is outside decision 5's intersection.
func (s *StudioTurnControl) boundTool(ctx context.Context, tenantID int64, pinned pinnedTurnContext, tool domain.ToolReference) (domain.ToolDefinition, error) {
	bound, err := s.Tools.List(ctx, tenantID, pinned.turn.AgentID, pinned.version)
	if err != nil {
		return domain.ToolDefinition{}, fmt.Errorf("list tools bound to %s@%s: %w", pinned.turn.AgentID, pinned.version, err)
	}
	for _, definition := range bound {
		if definition.ToolID == tool.ToolID && definition.Version == tool.Version {
			return definition, nil
		}
	}
	return domain.ToolDefinition{}, fmt.Errorf("%w: release %s of agent %q does not bind tool %s@%s", domain.ErrForbidden, pinned.version, pinned.turn.AgentID, tool.ToolID, tool.Version)
}

// guardedReply passes a person's reply through the release's output guardrails and evidence rule
// and returns the frame it is published as; the store gives the frame its sequence and fence.
func (s *StudioTurnControl) guardedReply(ctx context.Context, actor domain.StudioActor, requestID string, reply []byte) (domain.TurnReply, error) {
	if len(strings.TrimSpace(string(reply))) == 0 {
		return domain.TurnReply{}, fmt.Errorf("%w: a reply has text", domain.ErrValidation)
	}
	if s.Guardrails == nil || s.Definitions == nil || s.Publisher == nil {
		return domain.TurnReply{}, fmt.Errorf("%w: operator replies need guardrails, the release's definition, and a reply publisher", domain.ErrNotReady)
	}
	pinned, err := s.pinnedTurn(ctx, actor.TenantID, requestID)
	if err != nil {
		return domain.TurnReply{}, err
	}
	config := domain.GuardrailConfig{}
	if s.GuardrailConfigs != nil {
		if config, err = s.GuardrailConfigs.Get(ctx, actor.TenantID, pinned.turn.AgentID, pinned.version); err != nil {
			return domain.TurnReply{}, fmt.Errorf("load guardrail config: %w", err)
		}
	}
	subject := domain.GuardrailSubject{
		TenantID: actor.TenantID, Principal: actor.Principal, RequestID: requestID,
		ConversationID: pinned.turn.ConversationID, ReleaseVersion: pinned.version,
	}
	guarded, err := s.Guardrails.AfterModelChunk(ctx, config, subject, domain.ModelChunk{Payload: reply})
	if err != nil {
		return domain.TurnReply{}, stage.At(domain.StageGuardrail, err)
	}
	if err = s.evidenceRule(ctx, actor.TenantID, pinned, guarded.Payload); err != nil {
		return domain.TurnReply{}, err
	}
	events := []domain.TurnEvent{{Version: domain.TurnEventVersion, Kind: domain.TurnEventOperatorReply}}
	if len(guarded.Citations) > 0 {
		events = append(events, domain.TurnEvent{Version: domain.TurnEventVersion, Kind: domain.TurnEventCitations, Citations: guarded.Citations})
	}
	return domain.TurnReply{
		TenantID: actor.TenantID, RequestID: requestID, ConversationID: pinned.turn.ConversationID, ReplyRoute: pinned.route,
		Payload: guarded.Payload, Events: events, AgentVersion: pinned.version, EmittedAt: s.now(),
	}, nil
}

// evidenceRule holds a reply to the evidence rule the agent's answers meet: when a loop step of
// the pinned release requires evidence, the reply is an evidenced answer whose every claim the
// turn's validated tool results, the agent's and its operators', support.
func (s *StudioTurnControl) evidenceRule(ctx context.Context, tenantID int64, pinned pinnedTurnContext, reply []byte) error {
	graph, err := s.Definitions.Resolve(ctx, tenantID, pinned.turn.AgentID, pinned.version)
	if err != nil {
		return err
	}
	required, loopSteps := false, []int64{}
	for _, step := range graph.Steps {
		if step.Kind != domain.StepKindToolLoop {
			continue
		}
		loopSteps = append(loopSteps, step.ExecutionStepID)
		var config domain.ToolLoopConfig
		if len(step.Configuration) > 0 {
			if err := json.Unmarshal(step.Configuration, &config); err != nil {
				return fmt.Errorf("%w: tool loop configuration of step %q: %w", domain.ErrValidation, step.StepID, err)
			}
		}
		required = required || config.RequireEvidence
	}
	if !required {
		return nil
	}
	if s.Claims == nil {
		return fmt.Errorf("%w: the release requires evidence but no evidence validator is composed", domain.ErrDegraded)
	}
	var answer domain.EvidencedAnswer
	if err := json.Unmarshal(reply, &answer); err != nil {
		return fmt.Errorf("%w: the release requires an evidenced answer: %w", domain.ErrValidation, err)
	}
	scope := domain.EvidenceScope{TenantID: tenantID}
	for _, stepID := range loopSteps {
		entries, err := s.Journal.Load(ctx, domain.LoopKey{TenantID: tenantID, RequestID: pinned.turn.RequestID, ExecutionStepID: stepID})
		if err != nil {
			return fmt.Errorf("load loop journal: %w", err)
		}
		for _, entry := range entries {
			if entry.Kind == domain.LoopEntryObservation && entry.Observation != nil && !entry.Observation.IsError {
				scope.ToolResults = append(scope.ToolResults, domain.ToolEvidence{CallID: entry.Observation.CallID, ResourceURIs: entry.ResourceURIs})
			}
		}
	}
	operated, err := s.Control.OperatorEntries(ctx, tenantID, pinned.turn.RequestID)
	if err != nil {
		return err
	}
	for _, entry := range operated {
		if use := entry.Body.Tool; use != nil && use.Status == domain.CallCompleted && !use.IsError {
			scope.ToolResults = append(scope.ToolResults, domain.ToolEvidence{CallID: use.CallID, ResourceURIs: use.ResourceURIs})
		}
	}
	report, err := s.Claims.Validate(ctx, scope, answer)
	if err != nil {
		return stage.At(domain.StageGuardrail, err)
	}
	if len(report.Unsupported) > 0 {
		return stage.At(domain.StageGuardrail, unsupportedClaim(report.Unsupported[0]))
	}
	return nil
}

// publishReply publishes a reply frame now. The entry is durable already, and the agent's next
// delivery publishes the same frame at the same sequence, so a frame the stream refuses now, such as
// one that would leave a gap, is logged and reported unpublished rather than failing the act.
func (s *StudioTurnControl) publishReply(ctx context.Context, frame domain.TurnReply) bool {
	if err := s.Publisher.Publish(context.WithoutCancel(ctx), frame); err != nil {
		log.Printf("publish operator reply %d of turn %s for tenant %d: %v", frame.Sequence, frame.RequestID, frame.TenantID, err)
		return false
	}
	return true
}

// HandBack re-admits the turn's budget, then returns it to the agent: the instruction, when there
// is one, becomes the handback entry the agent's next step takes. A refused re-admission leaves the
// turn operated; a refused handback releases only a reservation this call newly took.
func (s *StudioTurnControl) HandBack(ctx context.Context, actor domain.StudioActor, request domain.TurnHandbackRequest) (domain.LiveTurn, error) {
	_, lease, err := s.operated(ctx, actor, request.AgentID, request.RequestID, request.Fence)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	turn, err := s.dispatched(ctx, s.qs, actor.TenantID, request.RequestID)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	quote, err := s.Estimator.Estimate(ctx, turn)
	if err != nil {
		return domain.LiveTurn{}, fmt.Errorf("estimate turn budget: %w", err)
	}
	reservation, err := s.Budget.Reserve(ctx, turnBudgetRequest(turn, quote))
	if err != nil {
		return domain.LiveTurn{}, fmt.Errorf("re-admit operated turn: %w", err)
	}
	handed, err := s.Control.HandBack(ctx, lease, request.Instruction)
	if err != nil {
		if reservation.ReservationID != "" && !reservation.Replayed {
			err = errors.Join(err, s.Budget.Release(context.WithoutCancel(ctx), reservation))
		}
		return domain.LiveTurn{}, err
	}
	if handed.Instruction != nil {
		if err = s.auditEntry(ctx, actor, request.AgentID, *handed.Instruction, ""); err != nil {
			return domain.LiveTurn{}, err
		}
	}
	if err = auditTurnControl(ctx, s.Audit, actor, request.AgentID, request.RequestID, observability.AuditCategoryTurnHandedBack, handed.Fence, "handed back", s.now()); err != nil {
		return domain.LiveTurn{}, err
	}
	return s.turn(ctx, actor.TenantID, request.AgentID, request.RequestID)
}

// Finish completes a turn the actor operates with their guarded reply (decision 4): the reply is
// the turn's response, recorded as the person's, the agent's held reservation settles at what the
// agent spent and the operators' calls make one operator usage line, all in one fenced unit. No
// agent run is recorded; the turn's completion is audited as the person's.
func (s *StudioTurnControl) Finish(ctx context.Context, actor domain.StudioActor, request domain.TurnFinishRequest) (domain.LiveTurn, error) {
	_, lease, err := s.operated(ctx, actor, request.AgentID, request.RequestID, request.Fence)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	frame, err := s.guardedReply(ctx, actor, request.RequestID, request.Reply)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	pinned, err := s.pinnedTurn(ctx, actor.TenantID, request.RequestID)
	if err != nil {
		return domain.LiveTurn{}, err
	}
	quote, err := s.Estimator.Estimate(ctx, pinned.turn)
	if err != nil {
		return domain.LiveTurn{}, fmt.Errorf("estimate turn budget: %w", err)
	}
	// The held agent reservation: a live hold replays, and a settled one refuses the finish.
	reservation, err := s.Budget.Reserve(ctx, turnBudgetRequest(pinned.turn, quote))
	if err != nil {
		return domain.LiveTurn{}, fmt.Errorf("settle operated turn: %w", err)
	}
	entry, err := s.Control.Finish(ctx, domain.OperatorFinish{
		Lease: lease, Frame: frame, Reservation: reservation, Currency: quote.Currency,
		SubjectRef: pinned.turn.AgentID + "@" + pinned.version, Attribution: usageAttribution(pinned.turn),
	})
	if err != nil {
		if reservation.ReservationID != "" && !reservation.Replayed {
			err = errors.Join(err, s.Budget.Release(context.WithoutCancel(ctx), reservation))
		}
		return domain.LiveTurn{}, err
	}
	if published := s.publishReply(ctx, *entry.Body.Frame); published {
		final := domain.TurnReply{
			TenantID: actor.TenantID, RequestID: request.RequestID, ConversationID: frame.ConversationID, ReplyRoute: frame.ReplyRoute,
			Sequence: entry.Body.Frame.Sequence + 1, Final: true, AgentVersion: frame.AgentVersion, EmittedAt: s.now(), Fence: lease.Fence,
		}
		s.publishReply(ctx, final)
	}
	if err = s.auditEntry(ctx, actor, request.AgentID, entry, ""); err != nil {
		return domain.LiveTurn{}, err
	}
	if err = s.auditFinish(ctx, actor, request.AgentID, request.RequestID, lease.Fence, entry.EntryNo); err != nil {
		return domain.LiveTurn{}, err
	}
	return s.turn(ctx, actor.TenantID, request.AgentID, request.RequestID)
}

// auditFinish records the turn's one terminal record as the person's. It takes the key the
// runtime's terminal record of the turn takes, so a stale delivery's terminal replay is a no-op.
func (s *StudioTurnControl) auditFinish(ctx context.Context, actor domain.StudioActor, agentID, requestID string, fence int64, entryNo int) error {
	if s.Audit == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"request_id": requestID, "agent_id": agentID, "fence": fence, "operator": true, "entry_no": entryNo})
	if err != nil {
		return fmt.Errorf("encode %s audit: %w", observability.AuditCategoryTurnCompleted, err)
	}
	scoped := domain.WithDecisionScope(context.WithoutCancel(ctx), "turn/", requestID)
	if err := s.Audit.Record(scoped, domain.DecisionRecord{
		TenantID: actor.TenantID, Principal: actor.Principal, Category: observability.AuditCategoryTurnCompleted, Action: "turn",
		Resource: agentID, Outcome: domain.DecisionAllow, Reason: "finished by operator", RequestID: requestID, Payload: payload, OccurredAt: s.now(),
	}); err != nil {
		return fmt.Errorf("record %s: %w", observability.AuditCategoryTurnCompleted, err)
	}
	return nil
}

// auditEntry records one operator entry under turn/<req>/fence/<f>/op/<n>, keyed per epoch.
func (s *StudioTurnControl) auditEntry(ctx context.Context, actor domain.StudioActor, agentID string, entry domain.OperatorEntry, status string) error {
	if s.Audit == nil {
		return nil
	}
	fields := map[string]any{
		"request_id": entry.RequestID, "agent_id": agentID, "fence": entry.Fence, "entry_no": entry.EntryNo, "kind": entry.Kind,
		"step_no": entry.Position.StepNo, "after_entry_no": entry.Position.AfterEntryNo,
	}
	if use := entry.Body.Tool; use != nil {
		fields["tool_id"], fields["call_id"], fields["status"] = use.ToolID, use.CallID, status
		fields["cost_minor_units"], fields["currency"], fields["exponent"] = entry.Usage.CostMinorUnits, entry.Usage.Currency, entry.Exponent
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode operator action audit: %w", err)
	}
	scoped := domain.WithDecisionScope(context.WithoutCancel(ctx), "turn/", entry.RequestID, "/fence/", entry.Fence, "/op/", entry.EntryNo)
	if err := s.Audit.Record(scoped, domain.DecisionRecord{
		TenantID: actor.TenantID, Principal: actor.Principal, Category: observability.AuditCategoryTurnOperatorAction, Action: "turn",
		Resource: agentID, Outcome: domain.DecisionAllow, Reason: string(entry.Kind), RequestID: entry.RequestID, Payload: payload, OccurredAt: s.now(),
	}); err != nil {
		return fmt.Errorf("record %s: %w", observability.AuditCategoryTurnOperatorAction, err)
	}
	return nil
}
