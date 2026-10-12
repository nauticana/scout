package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
	"github.com/nauticana/scout/service/observability"
)

// TurnRuntime executes one durably delivered turn. It first acquires the turn with the
// delivery's queue claim, which moves the turn's fence, and presents that owner on every
// write; a write refused with domain.ErrFenceLost ends the delivery with no terminal record,
// settlement, or final frame. Per step the order is idempotency claim or replay, execution,
// committed result, guardrail, durable checkpoint, published frame; the turn ends with its
// settlement and terminal record committed in one transaction before the final frame, so a crash
// replays the frame and never the side effect, and a reservation is never settled while the end
// can still be refused. A requested hold is taken at a durable checkpoint, and a call whose
// outcome is unknown holds the turn.
type TurnRuntime struct {
	Records     contract.TurnRecordStore
	Sessions    contract.SessionCoordinator
	Definitions contract.DefinitionResolver
	Policies    contract.TenantPolicyRepository
	Governor    contract.ExecutionGovernor
	Executors   contract.StepExecutorRegistry
	Idempotency contract.StepIdempotencyStore
	Guardrails  contract.GuardrailEnforcer
	Publisher   contract.TurnReplyPublisher
	Estimator   contract.TurnBudgetEstimator
	Budget      contract.TenantBudgetManager
	// Limits reads the execution limits frozen into the turn's release, which narrow the tenant runtime policy.
	Limits contract.PrincipalLimits
	// Control is optional; when set a requested cancellation or a moved fence stops the running
	// turn and a requested hold is taken at its next durable checkpoint.
	Control contract.TurnControlWatcher
	// Operator reads the entries people made while they operated the turn; nil reads none. A step
	// takes the entries positioned at it, and each reply frame is published again at its sequence.
	Operator contract.OperatorEntryReader
	// GuardrailConfigs pins the policy version of the running agent; nil uses an empty policy.
	GuardrailConfigs contract.GuardrailConfigRepository
	// Audit records each turn transition once; nil skips auditing. A failed write fails the
	// delivery, and the terminal replay offers the record again.
	Audit contract.AuditSink
	// Runs records each settled turn as an agent run under TurnTaskKind; nil skips.
	// It is best-effort: the turn is already final when it runs.
	Runs contract.AgentRunRecorder
	// Review offers each completed turn to production review; nil skips. Like Runs it is
	// best-effort: a failure is logged and the final turn stands.
	Review contract.ProductionReviewIntake
	// Observations records per-step stage observations; nil skips them.
	Observations contract.ObservationRecorder
	// OnSettled runs before a turn's settlement commits, on success and on a billed failure, for
	// product-side usage mirrors. It is not transactional with the settlement: an error fails the
	// turn, and a settlement refused after it ran is offered again by the turn's next owner, so a
	// mirror keys on the turn.
	OnSettled func(ctx context.Context, turn domain.TurnRequest, turnNo int64, agentVersion string, usage domain.Usage) error
	// MaxSteps bounds one turn independently of the tenant policy; required.
	MaxSteps int
	Now      func() time.Time
}

var _ contract.ConversationRuntime = (*TurnRuntime)(nil)

func (runtime *TurnRuntime) validate() error {
	if runtime.Records == nil || runtime.Sessions == nil || runtime.Definitions == nil || runtime.Policies == nil ||
		runtime.Governor == nil || runtime.Executors == nil || runtime.Idempotency == nil ||
		runtime.Guardrails == nil || runtime.Publisher == nil || runtime.Estimator == nil || runtime.Budget == nil || runtime.Limits == nil {
		return fmt.Errorf("%w: turn runtime is missing a required collaborator", domain.ErrValidation)
	}
	if runtime.MaxSteps <= 0 {
		return fmt.Errorf("%w: turn runtime max steps must be positive", domain.ErrValidation)
	}
	return nil
}

func (runtime *TurnRuntime) now() time.Time {
	if runtime.Now != nil {
		return runtime.Now()
	}
	return time.Now()
}

// turnExecution is the mutable state of one HandleTurn call.
type turnExecution struct {
	dispatch domain.TurnDispatch
	// owner is the acquired fence every write presents; a hold or suspension moves it to the
	// fence the final frame is published under.
	owner         domain.TurnOwner
	holdRequested func() bool
	turnNo        int64
	snapshot      domain.SessionSnapshot
	revision      int64
	published     int64
	checkpoints   int
	usage         domain.Usage
	config        domain.GuardrailConfig
	reservation   domain.BudgetReservation
	// mirrored is set once OnSettled ran for this delivery.
	mirrored bool
	limits   domain.ExecutionLimits
	budget   domain.ReleaseBudget
	// bounds are the delegation limits this turn may pass on; the zero value
	// means the turn acts on its own authority and delegates nothing.
	bounds domain.DelegationBounds
	// operator are the turn's operator entries, and replies the frames of its replies by sequence.
	operator []domain.OperatorEntry
	replies  map[int64]domain.TurnReply
}

// loadOperator reads the turn's operator entries once per delivery: a turn no worker runs gains
// none while this one owns it.
func (runtime *TurnRuntime) loadOperator(ctx context.Context, execution *turnExecution) error {
	if runtime.Operator == nil {
		return nil
	}
	turn := execution.dispatch.Turn
	entries, err := runtime.Operator.OperatorEntries(ctx, turn.TenantContext.TenantID, turn.RequestID)
	if err != nil {
		return fmt.Errorf("read operator entries: %w", err)
	}
	execution.operator, execution.replies = entries, make(map[int64]domain.TurnReply)
	for _, entry := range entries {
		if entry.Kind == domain.OperatorReply && entry.Body.Frame != nil {
			execution.replies[entry.Body.Frame.Sequence] = *entry.Body.Frame
		}
	}
	return nil
}

// stepOperator are the operator entries positioned at the turn's stepNo-th step.
func (execution *turnExecution) stepOperator(stepNo int) []domain.OperatorEntry {
	var entries []domain.OperatorEntry
	for _, entry := range execution.operator {
		if entry.Position.StepNo == stepNo {
			entries = append(entries, entry)
		}
	}
	return entries
}

// HandleTurn acquires the delivered turn with its claim, executes it, and returns once its
// result is durable; a nil error means the delivery may be acknowledged. A held or suspended
// turn is domain.ErrTurnHeld and a claim that is not the current lease domain.ErrFenceLost,
// both before anything runs.
func (runtime *TurnRuntime) HandleTurn(ctx context.Context, claim domain.TurnClaim) (domain.TurnResult, error) {
	if err := claim.Validate(); err != nil {
		return domain.TurnResult{}, err
	}
	dispatch := claim.Dispatch
	if err := validateDispatch(dispatch); err != nil {
		return domain.TurnResult{}, err
	}
	if err := runtime.validate(); err != nil {
		return domain.TurnResult{}, err
	}
	turn := dispatch.Turn
	ctx = domain.WithDecisionScope(ctx, "turn/", turn.RequestID)
	record, err := runtime.Records.Find(ctx, turn.TenantContext.TenantID, turn.RequestID)
	if err != nil {
		return domain.TurnResult{}, err
	}
	execution := &turnExecution{dispatch: dispatch, turnNo: record.TurnNo, bounds: turn.DelegationBounds, holdRequested: func() bool { return false }}
	if record.Status.Terminal() {
		return runtime.replayTerminal(ctx, execution, record)
	}
	if execution.owner, err = runtime.Records.Acquire(ctx, claim); err != nil {
		return domain.TurnResult{}, err
	}
	if err = runtime.loadOperator(ctx, execution); err != nil {
		return domain.TurnResult{}, err
	}
	turnCtx := ctx
	if runtime.Control != nil {
		watch, err := runtime.Control.Watch(ctx, execution.owner)
		if err != nil {
			return domain.TurnResult{}, err
		}
		defer watch.Release()
		turnCtx, execution.holdRequested = watch.Context(), watch.HoldRequested
	}
	switch cause := context.Cause(turnCtx); {
	case errors.Is(cause, domain.ErrFenceLost):
		return domain.TurnResult{}, cause
	case errors.Is(cause, domain.ErrTurnCanceled):
		// Cancelled before pickup: a suspended or held turn still holds a reservation, which fail releases once in hand.
		if err = runtime.reserve(ctx, execution); err != nil {
			cause = errors.Join(cause, err)
		}
		return domain.TurnResult{}, runtime.fail(ctx, execution, cause)
	}
	result, err := runtime.execute(turnCtx, execution)
	if cause := context.Cause(turnCtx); err != nil && (errors.Is(cause, domain.ErrTurnCanceled) || errors.Is(cause, domain.ErrFenceLost)) {
		err = errors.Join(cause, err)
	}
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, domain.ErrFenceLost):
		// The turn has another owner now: no terminal record, settlement, or final frame from this one.
		return domain.TurnResult{}, err
	case errors.Is(err, domain.ErrApprovalPending):
		return domain.TurnResult{}, runtime.suspend(ctx, execution, err)
	case errors.Is(err, domain.ErrOutcomeUnknown):
		// The call's row is unknown: hold the turn for reconciliation, keeping its reservation.
		return domain.TurnResult{}, runtime.hold(ctx, execution, domain.HoldUnknownOutcome, err)
	case errors.Is(err, domain.ErrTurnHeld):
		return domain.TurnResult{}, runtime.hold(ctx, execution, domain.HoldCheckpoint, err)
	default:
		return domain.TurnResult{}, runtime.fail(ctx, execution, err)
	}
}

// suspend parks a turn awaiting a human decision. The budget reservation stays
// held and no usage is settled, so the turn resumes with the budget it was
// admitted on. The delivery is acked; whoever resolves the approval re-dispatches.
func (runtime *TurnRuntime) suspend(ctx context.Context, execution *turnExecution, cause error) error {
	suspendCtx := context.WithoutCancel(ctx)
	fence, err := runtime.Records.Suspend(suspendCtx, execution.owner, stage.ErrorClass(cause))
	if errors.Is(err, domain.ErrTurnCanceled) {
		// A cancellation requested before the turn parked wins: it ends cancelled, never parked out of reach.
		return runtime.fail(ctx, execution, err)
	}
	if err != nil {
		return err
	}
	execution.owner.Fence = fence
	// A turn may suspend more than once; the frame it suspends at tells the suspensions apart.
	if err := runtime.audit(domain.WithDecisionScope(suspendCtx, "frame/", execution.published), execution, observability.AuditCategoryTurnSuspended, stage.ErrorClass(cause)); err != nil {
		return err
	}
	pending := domain.TurnEvent{Version: domain.TurnEventVersion, Kind: domain.TurnEventApprovalPending, Approval: &domain.TurnApprovalEvent{}}
	return runtime.publish(suspendCtx, execution, nil, true, stage.ErrorClass(cause), pending)
}

// hold takes the turn to held under code: at the durable checkpoint where a hold request was
// honored, or on a call whose outcome is unknown. Like a suspension it keeps the reservation and
// settles nothing; the held frame ends this delivery, and a released turn continues its stream at
// that sequence. The audit records why the turn held.
func (runtime *TurnRuntime) hold(ctx context.Context, execution *turnExecution, code domain.TurnHoldCode, cause error) error {
	holdCtx := context.WithoutCancel(ctx)
	fence, err := runtime.Records.Hold(holdCtx, execution.owner, code)
	if errors.Is(err, domain.ErrTurnCanceled) {
		return runtime.fail(ctx, execution, err)
	}
	if err != nil {
		return err
	}
	execution.owner.Fence = fence
	errorCode, events := heldFrame()
	if err := runtime.audit(domain.WithDecisionScope(holdCtx, "fence/", fence), execution, observability.AuditCategoryTurnHeld, stage.ErrorClass(cause)); err != nil {
		return err
	}
	return runtime.publish(holdCtx, execution, nil, true, errorCode, events...)
}

// replayTerminal re-offers the terminal record and republishes the final frame of an
// already terminal turn under its last fence; it never re-executes and never settles again.
func (runtime *TurnRuntime) replayTerminal(ctx context.Context, execution *turnExecution, record domain.TurnRecord) (domain.TurnResult, error) {
	turn := execution.dispatch.Turn
	status, payload := record.Status, record.Payload
	execution.owner = domain.TurnOwner{TenantID: turn.TenantContext.TenantID, RequestID: turn.RequestID, Fence: record.Fence}
	snapshot, err := runtime.Sessions.Load(ctx, turn.TenantContext.TenantID, turn.ConversationID)
	if err != nil {
		return domain.TurnResult{}, err
	}
	execution.snapshot = snapshot
	if err = runtime.loadOperator(ctx, execution); err != nil {
		return domain.TurnResult{}, err
	}
	// Every reply a person sent precedes the final frame.
	execution.published = finalFrameSequence(snapshot, execution.turnNo) + int64(len(execution.replies))
	errorCode := ""
	if status != domain.TurnCompleted {
		errorCode = terminalTurnErrorCode(status, payload)
	}
	// The key of the first record makes this a no-op unless that write was lost.
	if err := runtime.audit(context.WithoutCancel(ctx), execution, turnAuditCategory(status), errorCode); err != nil {
		return domain.TurnResult{}, err
	}
	if err := runtime.publish(ctx, execution, nil, true, errorCode); err != nil {
		return domain.TurnResult{}, err
	}
	if status != domain.TurnCompleted {
		return domain.TurnResult{}, fmt.Errorf("%w: turn %q is already terminal (%s)", domain.ErrConflict, turn.RequestID, errorCode)
	}
	return domain.TurnResult{Response: payload, AgentVersion: snapshot.AgentVersion}, nil
}

func (runtime *TurnRuntime) execute(ctx context.Context, execution *turnExecution) (domain.TurnResult, error) {
	turn := execution.dispatch.Turn
	tenantID := turn.TenantContext.TenantID
	snapshot, err := runtime.Sessions.Load(ctx, tenantID, turn.ConversationID)
	if err != nil {
		return domain.TurnResult{}, err
	}
	if strings.TrimSpace(snapshot.AgentVersion) == "" {
		return domain.TurnResult{}, fmt.Errorf("%w: conversation %q has no pinned agent version", domain.ErrNotReady, turn.ConversationID)
	}
	execution.snapshot = snapshot
	execution.revision = snapshot.Revision
	graph, err := runtime.Definitions.Resolve(ctx, tenantID, turn.AgentID, snapshot.AgentVersion)
	if err != nil {
		return domain.TurnResult{}, err
	}
	policy, err := runtime.Policies.GetRuntimePolicy(ctx, tenantID)
	if err != nil {
		return domain.TurnResult{}, fmt.Errorf("load runtime policy: %w", err)
	}
	if execution.config, err = runtime.guardrailConfig(ctx, tenantID, turn.AgentID, snapshot.AgentVersion); err != nil {
		return domain.TurnResult{}, err
	}
	if err = runtime.reserve(ctx, execution); err != nil {
		return domain.TurnResult{}, err
	}
	if execution.limits, err = runtime.Limits.ExecutionLimits(ctx, pinnedPrincipal(turn, snapshot.AgentVersion)); err != nil {
		return domain.TurnResult{}, fmt.Errorf("load release limits: %w", err)
	}
	policy = narrowRuntimePolicy(policy, execution.limits)
	if execution.budget, err = runtime.Limits.Budget(ctx, pinnedPrincipal(turn, snapshot.AgentVersion)); err != nil {
		return domain.TurnResult{}, fmt.Errorf("load release budget: %w", err)
	}
	if policy, err = narrowRuntimeBudget(policy, execution.budget); err != nil {
		return domain.TurnResult{}, err
	}
	permit, err := runtime.Governor.Start(ctx, turn, policy)
	if err != nil {
		return domain.TurnResult{}, err
	}
	result, err := runtime.runSteps(ctx, execution, graph, policy, permit)
	closeErr := permit.Close(context.WithoutCancel(ctx), execution.usage)
	if err != nil {
		return domain.TurnResult{}, err
	}
	if closeErr != nil {
		return domain.TurnResult{}, closeErr
	}
	return result, runtime.settle(ctx, execution, &result)
}

func (runtime *TurnRuntime) guardrailConfig(ctx context.Context, tenantID int64, agentID, agentVersion string) (domain.GuardrailConfig, error) {
	if runtime.GuardrailConfigs == nil {
		return domain.GuardrailConfig{}, nil
	}
	config, err := runtime.GuardrailConfigs.Get(ctx, tenantID, agentID, agentVersion)
	if err != nil {
		return domain.GuardrailConfig{}, fmt.Errorf("load guardrail config: %w", err)
	}
	return config, nil
}

// reserve replays the reservation ingress already holds for this request. A live turn's
// reservation is settled only with its end, so a settled one fails closed: no step runs unheld.
func (runtime *TurnRuntime) reserve(ctx context.Context, execution *turnExecution) error {
	turn := execution.dispatch.Turn
	quote, err := runtime.Estimator.Estimate(ctx, turn)
	if err != nil {
		return fmt.Errorf("estimate turn budget: %w", err)
	}
	reservation, err := runtime.Budget.Reserve(ctx, turnBudgetRequest(turn, quote))
	if err != nil {
		return fmt.Errorf("reserve turn budget: %w", err)
	}
	execution.reservation = reservation
	return nil
}

func (runtime *TurnRuntime) runSteps(ctx context.Context, execution *turnExecution, graph domain.ExecutionGraph, policy domain.TenantRuntimePolicy, permit contract.ExecutionPermit) (domain.TurnResult, error) {
	steps := make(map[string]domain.ExecutionStep, len(graph.Steps))
	for _, step := range graph.Steps {
		steps[step.StepID] = step
	}
	stepID := graph.EntryStepID
	for stepNo := 1; ; stepNo++ {
		if stepNo > runtime.MaxSteps {
			return domain.TurnResult{}, fmt.Errorf("%w: turn exceeded %d steps", domain.ErrExecutionLimit, runtime.MaxSteps)
		}
		step, found := steps[stepID]
		if !found {
			return domain.TurnResult{}, fmt.Errorf("%w: execution step %q of agent %q", domain.ErrNotFound, stepID, graph.AgentID)
		}
		result, err := runtime.runStep(ctx, execution, step, stepNo, policy, permit)
		if err != nil {
			return domain.TurnResult{}, err
		}
		if strings.TrimSpace(result.NextStepID) == "" {
			return domain.TurnResult{
				Response:     result.State,
				AgentVersion: execution.snapshot.AgentVersion,
				CheckpointID: checkpointIdentity(execution.dispatch.Turn.ConversationID, execution.turnNo, stepNo),
				Citations:    result.Citations,
				Usage:        execution.usage,
			}, nil
		}
		// The step is checkpointed and its frame published: a durable boundary to hold at.
		if execution.holdRequested != nil && execution.holdRequested() {
			return domain.TurnResult{}, fmt.Errorf("%w: after step %d", domain.ErrTurnHeld, stepNo)
		}
		stepID = result.NextStepID
	}
}

func (runtime *TurnRuntime) runStep(ctx context.Context, execution *turnExecution, step domain.ExecutionStep, stepNo int, policy domain.TenantRuntimePolicy, permit contract.ExecutionPermit) (domain.StepResult, error) {
	turn := execution.dispatch.Turn
	started := runtime.now()
	ctx = domain.WithDecisionScope(ctx, "step/", stepNo)
	if err := permit.BeforeStep(ctx, step); err != nil {
		return domain.StepResult{}, err
	}
	result, replayed, err := runtime.Idempotency.Begin(ctx, execution.owner, step)
	if err != nil {
		return domain.StepResult{}, stage.At(domain.StageCheckpoint, err)
	}
	if !replayed {
		spend, err := stepSpend(execution)
		if err != nil {
			return domain.StepResult{}, err
		}
		executor, err := runtime.Executors.ExecutorFor(ctx, step.Kind)
		if err != nil {
			return domain.StepResult{}, err
		}
		input := domain.StepInput{
			Step: step, Snapshot: execution.snapshot, RequestID: execution.dispatch.Turn.RequestID,
			Input:      execution.dispatch.Turn.Input,
			Principal:  pinnedPrincipal(execution.dispatch.Turn, execution.snapshot.AgentVersion),
			OnBehalfOf: actingFor(execution.dispatch.Turn, execution.snapshot), Bounds: execution.bounds, Limits: execution.limits,
			WorkItemID: execution.dispatch.Turn.WorkItemID, WorkItemDepth: execution.dispatch.Turn.WorkItemDepth, Spend: spend,
			Owner: execution.owner, HoldRequested: execution.holdRequested, Operator: execution.stepOperator(stepNo),
		}
		if result, err = executor.Execute(ctx, input); err != nil {
			spent := spentUsage(err)
			// A parked step is replayed from its journal, which accounts its usage again.
			if !errors.Is(err, domain.ErrApprovalPending) && !errors.Is(err, domain.ErrTurnHeld) && !errors.Is(err, domain.ErrOutcomeUnknown) {
				err = errors.Join(err, addChunkUsage(&execution.usage, spent))
			}
			runtime.observe(ctx, execution, step, started, spent, err)
			if abandonErr := runtime.Idempotency.Abandon(context.WithoutCancel(ctx), execution.owner, step); abandonErr != nil {
				return domain.StepResult{}, errors.Join(err, abandonErr)
			}
			return domain.StepResult{}, err
		}
		if err = runtime.Idempotency.Commit(ctx, execution.owner, step, result); err != nil {
			return domain.StepResult{}, stage.At(domain.StageCheckpoint, err)
		}
	}
	if err := permit.AfterStep(ctx, result); err != nil {
		return domain.StepResult{}, err
	}
	if err := addChunkUsage(&execution.usage, result.Usage); err != nil {
		return domain.StepResult{}, err
	}
	guarded, err := runtime.Guardrails.AfterModelChunk(ctx, execution.config,
		guardrailSubject(turn, execution.snapshot.AgentVersion),
		domain.ModelChunk{Sequence: execution.published, Payload: result.State, Citations: result.Citations})
	if err != nil {
		runtime.observe(ctx, execution, step, started, result.Usage, err)
		return domain.StepResult{}, stage.At(domain.StageGuardrail, err)
	}
	transformed := !bytes.Equal(result.State, guarded.Payload)
	result.State, result.Citations = guarded.Payload, guarded.Citations
	result.Fingerprint = DigestBytes(result.State)
	guardedEvents := result.Events[:0]
	for _, event := range result.Events {
		if transformed && event.Kind == domain.TurnEventTextDelta {
			continue
		}
		if event.Kind == domain.TurnEventResult {
			event.Text = string(result.State)
		}
		if event.Kind == domain.TurnEventCitations {
			if len(result.Citations) == 0 {
				continue
			}
			event.Citations = result.Citations
		}
		guardedEvents = append(guardedEvents, event)
	}
	result.Events = guardedEvents
	if err := runtime.checkpoint(ctx, execution, step, stepNo, result, policy); err != nil {
		runtime.observe(ctx, execution, step, started, result.Usage, err)
		return domain.StepResult{}, stage.At(domain.StageCheckpoint, err)
	}
	if err := runtime.publish(ctx, execution, result.State, false, "", result.Events...); err != nil {
		runtime.observe(ctx, execution, step, started, result.Usage, err)
		return domain.StepResult{}, stage.At(domain.StagePublish, err)
	}
	execution.snapshot.State = result.State
	execution.snapshot.LastCompletedStepID = step.StepID
	runtime.observe(ctx, execution, step, started, result.Usage, nil)
	return result, nil
}

// checkpoint persists the step unless the durable snapshot already covers it,
// which is how a redelivered turn resumes without rewriting history.
func (runtime *TurnRuntime) checkpoint(ctx context.Context, execution *turnExecution, step domain.ExecutionStep, stepNo int, result domain.StepResult, policy domain.TenantRuntimePolicy) error {
	if checkpointed(execution.snapshot, execution.turnNo, stepNo) {
		return nil
	}
	usage := result.Usage
	if usage.Currency == "" {
		usage.Currency = policy.CostCurrency
	}
	fingerprint := result.Fingerprint
	if len(fingerprint) != 64 {
		fingerprint = DigestBytes(result.State)
	}
	checkpoint := domain.StepCheckpoint{
		ConversationID:  execution.dispatch.Turn.ConversationID,
		TurnNo:          execution.turnNo,
		StepNo:          stepNo,
		ExecutionStepID: step.ExecutionStepID,
		StepID:          step.StepID,
		IdempotencyKey:  common.TruncateRunes(fmt.Sprintf("%s:%d", execution.dispatch.Turn.RequestID, step.ExecutionStepID), 200),
		Fingerprint:     fingerprint,
		State:           result.State,
		Usage:           usage,
	}
	if err := runtime.Sessions.Checkpoint(ctx, execution.owner, execution.revision, checkpoint); err != nil {
		return err
	}
	execution.revision++
	execution.checkpoints++
	return nil
}

// settle commits the terminal result with its settlement in one transaction, and only then
// publishes the final frame.
func (runtime *TurnRuntime) settle(ctx context.Context, execution *turnExecution, result *domain.TurnResult) error {
	turn := execution.dispatch.Turn
	settleCtx := context.WithoutCancel(ctx)
	if err := runtime.settled(settleCtx, execution); err != nil {
		return err
	}
	if err := runtime.Sessions.Complete(settleCtx, execution.owner, turn.ConversationID, execution.revision, *result, runtime.settlement(execution, false)); err != nil {
		return err
	}
	if err := runtime.audit(settleCtx, execution, observability.AuditCategoryTurnCompleted, ""); err != nil {
		return err
	}
	runtime.recordRun(settleCtx, execution, domain.RunCompleted)
	if err := runtime.publish(settleCtx, execution, nil, true, ""); err != nil {
		return err
	}
	// After the final frame, so a slow review store never delays the reply.
	runtime.offerReview(settleCtx, execution)
	return nil
}

// fail records the terminal failure with its settlement in one transaction: the work already
// performed is settled, or an unused hold refunded. Only once that record stands does it write
// the terminal audit and publish a payload-free final frame; a lost fence stops it before any
// terminal write.
func (runtime *TurnRuntime) fail(ctx context.Context, execution *turnExecution, cause error) error {
	failCtx := context.WithoutCancel(ctx)
	status, errorCode := domain.TurnFailed, stage.ErrorClass(cause)
	if errors.Is(cause, domain.ErrTurnCanceled) || errors.Is(cause, context.Canceled) {
		status = domain.TurnCancelled
	}
	errs := []error{cause}
	billed := hasUsage(execution.usage)
	if billed {
		if err := runtime.settled(failCtx, execution); err != nil {
			errs = append(errs, err)
		}
	}
	// A refused terminal record means another writer owns the turn's end: its audit and frame are not this one's.
	if err := runtime.Records.Fail(failCtx, execution.owner, status, errorCode, runtime.settlement(execution, !billed)); errors.Is(err, domain.ErrBudgetUnsettled) {
		errs = append(errs, err)
	} else if err != nil {
		return errors.Join(append(errs, err)...)
	}
	runtime.recordRun(failCtx, execution, domain.RunStatus(status))
	if err := runtime.audit(failCtx, execution, turnAuditCategory(status), errorCode); err != nil {
		errs = append(errs, err)
	}
	if err := runtime.publish(failCtx, execution, nil, true, errorCode); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Abandon refunds and terminates a delivery the scheduler is dead-lettering, so retry
// exhaustion settles like any other terminal path. It acquires the turn with the delivery's
// claim first; a turn held, suspended, or owned by a newer delivery is left as it is.
func (runtime *TurnRuntime) Abandon(ctx context.Context, claim domain.TurnClaim, reason string) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	dispatch := claim.Dispatch
	if err := validateDispatch(dispatch); err != nil {
		return err
	}
	if err := runtime.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: abandon reason is required", domain.ErrValidation)
	}
	turn := dispatch.Turn
	tenantID := turn.TenantContext.TenantID
	ctx = domain.WithDecisionScope(ctx, "turn/", turn.RequestID)
	record, err := runtime.Records.Find(ctx, tenantID, turn.RequestID)
	if err != nil {
		return err
	}
	if record.Status.Terminal() {
		return nil
	}
	execution := &turnExecution{dispatch: dispatch, turnNo: record.TurnNo}
	execution.owner, err = runtime.Records.Acquire(ctx, claim)
	if errors.Is(err, domain.ErrTurnHeld) || errors.Is(err, domain.ErrFenceLost) {
		return nil
	}
	if err != nil {
		return err
	}
	snapshot, err := runtime.Sessions.Load(ctx, tenantID, turn.ConversationID)
	if err != nil {
		return err
	}
	execution.snapshot = snapshot
	if err = runtime.loadOperator(ctx, execution); err != nil {
		return err
	}
	execution.published = finalFrameSequence(snapshot, record.TurnNo) + int64(len(execution.replies))
	quote, err := runtime.Estimator.Estimate(ctx, turn)
	if err != nil {
		return fmt.Errorf("estimate turn budget: %w", err)
	}
	reservation, err := runtime.Budget.Reserve(ctx, turnBudgetRequest(turn, quote))
	switch {
	case err == nil:
		execution.reservation = reservation
	case errors.Is(err, domain.ErrBudgetSettled), errors.Is(err, domain.ErrBudgetExceeded), errors.Is(err, domain.ErrDegraded):
		// No live hold remains to release: it is settled, or admission refused a new attempt.
	default:
		return fmt.Errorf("reserve turn budget: %w", err)
	}
	failure := runtime.fail(ctx, execution, fmt.Errorf("%w: %s", domain.ErrExecutionLimit, reason))
	if errors.Is(failure, domain.ErrExecutionLimit) {
		return nil
	}
	return failure
}

// pinnedPrincipal resolves the acting principal and pins the turn's own agent to the conversation's agent version,
// so its tool bindings and its execution graph come from the same release even
// when a newer one was deployed mid-conversation.
func pinnedPrincipal(turn domain.TurnRequest, agentVersion string) domain.Principal {
	principal := turn.Principal
	if principal.Kind == "" {
		// A turn dispatched without a principal acts as its agent on its own authority.
		principal = domain.Principal{
			Kind: domain.PrincipalAgent, ID: turn.AgentID,
			TenantID: turn.TenantContext.TenantID, ScopeID: turn.TenantContext.ScopeID,
		}
	}
	if principal.Kind == domain.PrincipalAgent && principal.ID == turn.AgentID {
		principal.Release = agentVersion
	}
	return principal
}

// actingFor falls back to the conversation's human for a turn dispatched without one.
func actingFor(turn domain.TurnRequest, snapshot domain.SessionSnapshot) domain.PrincipalRef {
	if turn.OnBehalfOf.ID != "" || snapshot.EndUserRef == "" {
		return turn.OnBehalfOf
	}
	return domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: snapshot.EndUserRef}
}

// settled runs OnSettled once per delivery.
func (runtime *TurnRuntime) settled(ctx context.Context, execution *turnExecution) error {
	if runtime.OnSettled == nil || execution.mirrored {
		return nil
	}
	if err := runtime.OnSettled(ctx, execution.dispatch.Turn, execution.turnNo, execution.snapshot.AgentVersion, execution.usage); err != nil {
		return fmt.Errorf("settle hook: %w", err)
	}
	execution.mirrored = true
	return nil
}

// settlement is what the turn's end settles: its reservation at the turn's usage, or refunded.
func (runtime *TurnRuntime) settlement(execution *turnExecution, refund bool) domain.TurnSettlement {
	turn := execution.dispatch.Turn
	return domain.TurnSettlement{
		Reservation: execution.reservation, Usage: execution.usage, Refund: refund,
		SubjectRef: turn.AgentID + "@" + execution.snapshot.AgentVersion, Attribution: usageAttribution(turn),
	}
}

// spentUsage is what a failed step reports it already consumed, so the turn
// settles it instead of refunding it.
func spentUsage(err error) domain.Usage {
	var spent interface{ SpentUsage() domain.Usage }
	if errors.As(err, &spent) {
		return spent.SpentUsage()
	}
	return domain.Usage{}
}

func hasUsage(usage domain.Usage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.ToolCalls > 0 || usage.CostMinorUnits > 0
}

func (runtime *TurnRuntime) publish(ctx context.Context, execution *turnExecution, payload []byte, final bool, errorCode string, events ...domain.TurnEvent) error {
	turn := execution.dispatch.Turn
	// A person's reply took its sequence while they operated the turn; it is published again there
	// under this owner's fence, so a frame the stream refused then is not lost.
	for {
		reply, found := execution.replies[execution.published]
		if !found {
			break
		}
		reply.Fence, reply.EmittedAt = execution.owner.Fence, runtime.now()
		if err := runtime.Publisher.Publish(ctx, reply); err != nil && !errors.Is(err, domain.ErrReplayExpired) {
			return err
		}
		execution.published++
	}
	err := runtime.Publisher.Publish(ctx, domain.TurnReply{
		TenantID:       turn.TenantContext.TenantID,
		RequestID:      turn.RequestID,
		ConversationID: turn.ConversationID,
		ReplyRoute:     execution.dispatch.ReplyRoute,
		Sequence:       execution.published,
		Payload:        payload,
		Events:         events,
		Final:          final,
		ErrorCode:      errorCode,
		AgentVersion:   execution.snapshot.AgentVersion,
		EmittedAt:      runtime.now(),
		Fence:          execution.owner.Fence,
	})
	// An older sequence outside the retained window means the stream already advanced past it.
	if err != nil && !errors.Is(err, domain.ErrReplayExpired) {
		return err
	}
	execution.published++
	return nil
}

// TurnTaskKind is the task kind a data-plane turn is recorded under.
const TurnTaskKind = "conversation_turn"

func (runtime *TurnRuntime) recordRun(ctx context.Context, execution *turnExecution, status domain.RunStatus) {
	// A turn that failed before loading its pinned version ran no release.
	if runtime.Runs == nil || execution.snapshot.AgentVersion == "" {
		return
	}
	turn := execution.dispatch.Turn
	run := domain.AgentRun{
		Release:  domain.AgentReleaseReference{AgentID: turn.AgentID, Version: execution.snapshot.AgentVersion},
		TaskKind: TurnTaskKind, RequestID: turn.RequestID, Status: status,
	}
	if err := runtime.Runs.Record(ctx, turn.TenantContext.TenantID, run); err != nil {
		log.Printf("record %s agent run %s for tenant %d: %v", status, turn.RequestID, turn.TenantContext.TenantID, err)
	}
}

func (runtime *TurnRuntime) offerReview(ctx context.Context, execution *turnExecution) {
	if runtime.Review == nil || execution.snapshot.AgentVersion == "" {
		return
	}
	turn := execution.dispatch.Turn
	if err := runtime.Review.TurnCompleted(ctx, domain.ReviewTurn{
		TenantContext: turn.TenantContext, RequestID: turn.RequestID, AgentID: turn.AgentID, AgentVersion: execution.snapshot.AgentVersion,
	}); err != nil {
		log.Printf("offer turn %s of tenant %d to review: %v", turn.RequestID, turn.TenantContext.TenantID, err)
	}
}

func (runtime *TurnRuntime) audit(ctx context.Context, execution *turnExecution, category, errorCode string) error {
	if runtime.Audit == nil {
		return nil
	}
	turn := execution.dispatch.Turn
	payload, err := json.Marshal(map[string]any{
		"request_id":       turn.RequestID,
		"conversation_id":  turn.ConversationID,
		"turn_no":          execution.turnNo,
		"agent_id":         turn.AgentID,
		"agent_version":    execution.snapshot.AgentVersion,
		"input_tokens":     execution.usage.InputTokens,
		"output_tokens":    execution.usage.OutputTokens,
		"cost_minor_units": execution.usage.CostMinorUnits,
		"currency":         execution.usage.Currency,
		"error_code":       errorCode,
		"fence":            execution.owner.Fence,
	})
	if err != nil {
		return fmt.Errorf("encode turn audit: %w", err)
	}
	outcome := domain.DecisionAllow
	if errorCode != "" {
		outcome = domain.DecisionDeny
	}
	if err := runtime.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: turn.TenantContext.TenantID, Principal: turnPrincipal(turn), ScopeID: turn.TenantContext.ScopeID,
		Category: category, Action: "turn", Resource: turn.AgentID, Outcome: outcome, Reason: errorCode, WorkRef: turn.WorkRef,
		RequestID: turn.RequestID, ConversationID: turn.ConversationID, Payload: payload, OccurredAt: runtime.now(),
	}); err != nil {
		return fmt.Errorf("record %s: %w", category, err)
	}
	return nil
}

func usageAttribution(turn domain.TurnRequest) domain.UsageAttribution {
	return domain.UsageAttribution{Principal: turnPrincipal(turn), ScopeID: turn.TenantContext.ScopeID, OnBehalfOf: turn.OnBehalfOf, WorkRef: turn.WorkRef}
}

// turnBudgetRequest is the hold one turn asks for, the same at ingress and in the runtime so a
// replay re-attaches to it. It is attributed to the acting principal, and a delegated hop never
// holds more cost than the budget its delegator passed down. The turn's governors stop its work
// at a per-turn budget, so the ledger clamps the hold to that budget.
func turnBudgetRequest(turn domain.TurnRequest, quote domain.Usage) domain.BudgetRequest {
	cost := quote.CostMinorUnits
	if bound := turn.DelegationBounds; bound.BudgetMinorUnits > 0 && bound.Currency == quote.Currency && cost > bound.BudgetMinorUnits {
		cost = bound.BudgetMinorUnits
	}
	return domain.BudgetRequest{
		TenantID: turn.TenantContext.TenantID, RequestID: turn.RequestID, Principal: turnPrincipal(turn),
		Tokens: quote.InputTokens + quote.OutputTokens, CostMinorUnits: cost, Currency: quote.Currency, TurnGoverned: true,
	}
}

func turnPrincipal(turn domain.TurnRequest) domain.PrincipalRef {
	if turn.Principal.Kind != "" {
		return domain.PrincipalRef{Kind: turn.Principal.Kind, ID: turn.Principal.ID}
	}
	return domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: turn.AgentID}
}

func guardrailSubject(turn domain.TurnRequest, release string) domain.GuardrailSubject {
	return domain.GuardrailSubject{
		TenantID: turn.TenantContext.TenantID, Principal: turnPrincipal(turn), RequestID: turn.RequestID,
		ConversationID: turn.ConversationID, ReleaseVersion: release,
	}
}

func (runtime *TurnRuntime) observe(ctx context.Context, execution *turnExecution, step domain.ExecutionStep, started time.Time, usage domain.Usage, err error) {
	if runtime.Observations == nil {
		return
	}
	turn := execution.dispatch.Turn
	span := stage.Begin(started, domain.StageTool, step.Kind, domain.ComponentVersions{Agent: execution.snapshot.AgentVersion})
	span.Observation.TenantID = turn.TenantContext.TenantID
	span.Observation.Principal = turnPrincipal(turn)
	span.Observation.ScopeID = turn.TenantContext.ScopeID
	span.Observation.TenantTier = turn.TenantContext.Tier
	span.Observation.Region = turn.TenantContext.Region
	span.Observation.QueueWait = started.Sub(execution.dispatch.EnqueuedAt)
	runtime.Observations.RecordObservation(ctx, span.End(runtime.now(), "", usage, err))
}

func (runtime *TurnRuntime) observeError(ctx context.Context, execution *turnExecution, err error) {
	if runtime.Observations == nil {
		return
	}
	span := stage.Begin(runtime.now(), domain.StagePublish, "audit", domain.ComponentVersions{Agent: execution.snapshot.AgentVersion})
	span.Observation.TenantID = execution.dispatch.Turn.TenantContext.TenantID
	span.Observation.Principal = turnPrincipal(execution.dispatch.Turn)
	span.Observation.ScopeID = execution.dispatch.Turn.TenantContext.ScopeID
	runtime.Observations.RecordObservation(ctx, span.End(runtime.now(), domain.OutcomeError, domain.Usage{}, err))
}

// checkpointed reports whether the durable snapshot already covers this step.
func checkpointed(snapshot domain.SessionSnapshot, turnNo int64, stepNo int) bool {
	return snapshot.LatestTurnNo > turnNo || (snapshot.LatestTurnNo == turnNo && snapshot.LatestStepNo >= stepNo)
}

// finalFrameSequence derives the sequence a replayed final frame must reuse:
// one frame was published per checkpointed step of this turn.
func finalFrameSequence(snapshot domain.SessionSnapshot, turnNo int64) int64 {
	if snapshot.LatestTurnNo != turnNo {
		return 0
	}
	return int64(snapshot.LatestStepNo)
}

func checkpointIdentity(conversationID string, turnNo int64, stepNo int) string {
	return fmt.Sprintf("%s:%d:%d", conversationID, turnNo, stepNo)
}
