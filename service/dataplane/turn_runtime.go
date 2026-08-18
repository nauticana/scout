package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

// TurnRuntime executes one durably delivered turn. Per step the order is
// idempotency claim or replay, execution, committed result, guardrail, durable
// checkpoint, published frame; the turn ends with settlement and the terminal
// record persisted before the final frame, so a crash replays the frame and
// never the side effect.
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
	// GuardrailConfigs pins the policy version of the running agent; nil uses an empty policy.
	GuardrailConfigs contract.GuardrailConfigRepository
	// Audit records one event per terminal transition; nil skips auditing.
	Audit contract.AuditSink
	// Observations records per-step stage observations; nil skips them.
	Observations contract.ObservationRecorder
	// MaxSteps bounds one turn independently of the tenant policy; required.
	MaxSteps int
	Now      func() time.Time
}

var _ contract.ConversationRuntime = (*TurnRuntime)(nil)

func (runtime *TurnRuntime) validate() error {
	if runtime.Records == nil || runtime.Sessions == nil || runtime.Definitions == nil || runtime.Policies == nil ||
		runtime.Governor == nil || runtime.Executors == nil || runtime.Idempotency == nil ||
		runtime.Guardrails == nil || runtime.Publisher == nil || runtime.Estimator == nil || runtime.Budget == nil {
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
	dispatch    domain.TurnDispatch
	turnNo      int64
	snapshot    domain.SessionSnapshot
	revision    int64
	published   int64
	checkpoints int
	usage       domain.Usage
	config      domain.GuardrailConfig
	reservation domain.BudgetReservation
	settled     bool
	// bounds are the delegation limits this turn may pass on; the zero value
	// means the turn acts on its own authority and delegates nothing.
	bounds domain.DelegationBounds
}

// HandleTurn executes the delivered turn and returns once its result is
// durable; a nil error means the delivery may be acknowledged.
func (runtime *TurnRuntime) HandleTurn(ctx context.Context, dispatch domain.TurnDispatch) (domain.TurnResult, error) {
	if err := validateDispatch(dispatch); err != nil {
		return domain.TurnResult{}, err
	}
	if err := runtime.validate(); err != nil {
		return domain.TurnResult{}, err
	}
	turn := dispatch.Turn
	tenantID := turn.TenantContext.TenantID
	turnNo, status, payload, err := runtime.Records.Find(ctx, tenantID, turn.RequestID)
	if err != nil {
		return domain.TurnResult{}, err
	}
	execution := &turnExecution{dispatch: dispatch, turnNo: turnNo, bounds: turn.DelegationBounds}
	if isTerminalTurnStatus(status) {
		return runtime.replayTerminal(ctx, execution, status, payload)
	}
	result, err := runtime.execute(ctx, execution)
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, domain.ErrApprovalPending):
		return domain.TurnResult{}, runtime.suspend(ctx, execution, err)
	default:
		return domain.TurnResult{}, runtime.fail(ctx, execution, err)
	}
}

// suspend parks a turn awaiting a human decision. The budget reservation stays
// held and no usage is settled, so the turn resumes with the budget it was
// admitted on. The delivery is acked; whoever resolves the approval re-dispatches.
func (runtime *TurnRuntime) suspend(ctx context.Context, execution *turnExecution, cause error) error {
	turn := execution.dispatch.Turn
	suspendCtx := context.WithoutCancel(ctx)
	if err := runtime.Records.Suspend(suspendCtx, turn.TenantContext.TenantID, turn.RequestID, stage.ErrorClass(cause)); err != nil {
		return err
	}
	runtime.audit(suspendCtx, execution, "turn_suspended", stage.ErrorClass(cause))
	if err := runtime.publish(suspendCtx, execution, nil, true, stage.ErrorClass(cause)); err != nil {
		return err
	}
	return nil
}

// replayTerminal republishes the final frame of an already terminal turn; it
// never re-executes and never settles again.
func (runtime *TurnRuntime) replayTerminal(ctx context.Context, execution *turnExecution, status string, payload []byte) (domain.TurnResult, error) {
	turn := execution.dispatch.Turn
	snapshot, err := runtime.Sessions.Load(ctx, turn.TenantContext.TenantID, turn.ConversationID)
	if err != nil {
		return domain.TurnResult{}, err
	}
	execution.snapshot = snapshot
	execution.published = finalFrameSequence(snapshot, execution.turnNo)
	errorCode := ""
	if status != "completed" {
		errorCode = string(payload)
		if errorCode == "" {
			errorCode = status
		}
	}
	if err := runtime.publish(ctx, execution, nil, true, errorCode); err != nil {
		return domain.TurnResult{}, err
	}
	if status != "completed" {
		return domain.TurnResult{}, fmt.Errorf("%w: turn %q is already %s (%s)", domain.ErrConflict, turn.RequestID, status, errorCode)
	}
	return domain.TurnResult{Response: payload, AgentVersion: snapshot.AgentVersion}, nil
}

func (runtime *TurnRuntime) execute(ctx context.Context, execution *turnExecution) (domain.TurnResult, error) {
	turn := execution.dispatch.Turn
	tenantID := turn.TenantContext.TenantID
	if err := runtime.Records.Start(ctx, tenantID, turn.RequestID); err != nil {
		return domain.TurnResult{}, err
	}
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

// reserve replays the reservation ingress already holds for this request; a
// settled reservation means an earlier attempt already paid for this turn.
func (runtime *TurnRuntime) reserve(ctx context.Context, execution *turnExecution) error {
	turn := execution.dispatch.Turn
	quote, err := runtime.Estimator.Estimate(ctx, turn)
	if err != nil {
		return fmt.Errorf("estimate turn budget: %w", err)
	}
	reservation, err := runtime.Budget.Reserve(ctx, turn.TenantContext.TenantID, turn.RequestID,
		quote.InputTokens+quote.OutputTokens, quote.CostMinorUnits, quote.Currency)
	if errors.Is(err, domain.ErrBudgetSettled) {
		execution.settled = true
		return nil
	}
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
	var lastState []byte
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
		lastState = result.State
		if strings.TrimSpace(result.NextStepID) == "" {
			return domain.TurnResult{
				Response:     lastState,
				AgentVersion: execution.snapshot.AgentVersion,
				CheckpointID: checkpointIdentity(execution.dispatch.Turn.ConversationID, execution.turnNo, stepNo),
				Usage:        execution.usage,
			}, nil
		}
		stepID = result.NextStepID
	}
}

func (runtime *TurnRuntime) runStep(ctx context.Context, execution *turnExecution, step domain.ExecutionStep, stepNo int, policy domain.TenantRuntimePolicy, permit contract.ExecutionPermit) (domain.StepResult, error) {
	turn := execution.dispatch.Turn
	tenantID := turn.TenantContext.TenantID
	started := runtime.now()
	if err := permit.BeforeStep(ctx, step); err != nil {
		return domain.StepResult{}, err
	}
	result, replayed, err := runtime.Idempotency.Begin(ctx, tenantID, turn.RequestID, step)
	if err != nil {
		return domain.StepResult{}, stage.At(domain.StageCheckpoint, err)
	}
	if !replayed {
		executor, err := runtime.Executors.ExecutorFor(ctx, step.Kind)
		if err != nil {
			return domain.StepResult{}, err
		}
		input := domain.StepInput{
			Step: step, Snapshot: execution.snapshot, RequestID: execution.dispatch.Turn.RequestID,
			Principal: execution.dispatch.Turn.Principal, Bounds: execution.bounds,
			WorkItemID: execution.dispatch.Turn.WorkItemID, WorkItemDepth: execution.dispatch.Turn.WorkItemDepth,
		}
		if result, err = executor.Execute(ctx, input); err != nil {
			runtime.observe(ctx, execution, step, started, domain.Usage{}, err)
			if abandonErr := runtime.Idempotency.Abandon(context.WithoutCancel(ctx), tenantID, turn.RequestID, step); abandonErr != nil {
				return domain.StepResult{}, errors.Join(err, abandonErr)
			}
			return domain.StepResult{}, err
		}
		if err = runtime.Idempotency.Commit(ctx, tenantID, turn.RequestID, step, result); err != nil {
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
		domain.ModelChunk{Sequence: execution.published, Payload: result.State})
	if err != nil {
		runtime.observe(ctx, execution, step, started, result.Usage, err)
		return domain.StepResult{}, stage.At(domain.StageGuardrail, err)
	}
	if err := runtime.checkpoint(ctx, execution, step, stepNo, result, policy); err != nil {
		runtime.observe(ctx, execution, step, started, result.Usage, err)
		return domain.StepResult{}, stage.At(domain.StageCheckpoint, err)
	}
	if err := runtime.publish(ctx, execution, guarded.Payload, false, ""); err != nil {
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
	if err := runtime.Sessions.Checkpoint(ctx, execution.dispatch.Turn.TenantContext.TenantID, execution.revision, checkpoint); err != nil {
		return err
	}
	execution.revision++
	execution.checkpoints++
	return nil
}

// settle records the settled usage, persists the terminal result, and only
// then publishes the final frame.
func (runtime *TurnRuntime) settle(ctx context.Context, execution *turnExecution, result *domain.TurnResult) error {
	turn := execution.dispatch.Turn
	tenantID := turn.TenantContext.TenantID
	settleCtx := context.WithoutCancel(ctx)
	if !execution.settled {
		if err := runtime.Budget.Commit(settleCtx, execution.reservation, execution.usage); err != nil {
			return fmt.Errorf("settle turn budget: %w", err)
		}
		execution.settled = true
	}
	subject := turn.AgentID + "@" + execution.snapshot.AgentVersion
	if err := runtime.Records.RecordUsage(settleCtx, tenantID, turn.ConversationID, execution.turnNo, subject, usageAttribution(turn), execution.usage); err != nil {
		return err
	}
	if err := runtime.Sessions.Complete(settleCtx, tenantID, turn.ConversationID, execution.revision, *result); err != nil {
		return err
	}
	runtime.audit(settleCtx, execution, "turn_completed", "")
	return runtime.publish(settleCtx, execution, nil, true, "")
}

// fail settles work already performed (or refunds an unused hold), records the
// terminal failure, and publishes a payload-free final frame.
func (runtime *TurnRuntime) fail(ctx context.Context, execution *turnExecution, cause error) error {
	turn := execution.dispatch.Turn
	tenantID := turn.TenantContext.TenantID
	failCtx := context.WithoutCancel(ctx)
	status, errorCode := "failed", stage.ErrorClass(cause)
	if errors.Is(cause, domain.ErrTurnCanceled) || errors.Is(cause, context.Canceled) {
		status = "cancelled"
	}
	errs := []error{cause}
	if !execution.settled && execution.reservation.ReservationID != "" {
		var settleErr error
		if hasUsage(execution.usage) {
			settleErr = runtime.Budget.Commit(failCtx, execution.reservation, execution.usage)
		} else {
			settleErr = runtime.Budget.Release(failCtx, execution.reservation)
		}
		if settleErr != nil {
			errs = append(errs, fmt.Errorf("settle failed turn budget: %w", settleErr))
		} else {
			execution.settled = true
		}
	}
	if hasUsage(execution.usage) {
		subject := turn.AgentID + "@" + execution.snapshot.AgentVersion
		if err := runtime.Records.RecordUsage(failCtx, tenantID, turn.ConversationID, execution.turnNo, subject, usageAttribution(turn), execution.usage); err != nil {
			errs = append(errs, err)
		}
	}
	if err := runtime.Records.Fail(failCtx, tenantID, turn.RequestID, status, errorCode); err != nil {
		errs = append(errs, err)
	}
	runtime.audit(failCtx, execution, "turn_"+status, errorCode)
	if err := runtime.publish(failCtx, execution, nil, true, errorCode); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Abandon refunds and terminates a delivery the scheduler is dead-lettering, so
// retry exhaustion settles like any other terminal path.
func (runtime *TurnRuntime) Abandon(ctx context.Context, dispatch domain.TurnDispatch, reason string) error {
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
	turnNo, status, _, err := runtime.Records.Find(ctx, tenantID, turn.RequestID)
	if err != nil {
		return err
	}
	execution := &turnExecution{dispatch: dispatch, turnNo: turnNo}
	if isTerminalTurnStatus(status) {
		return nil
	}
	snapshot, err := runtime.Sessions.Load(ctx, tenantID, turn.ConversationID)
	if err != nil {
		return err
	}
	execution.snapshot = snapshot
	execution.published = finalFrameSequence(snapshot, turnNo)
	quote, err := runtime.Estimator.Estimate(ctx, turn)
	if err != nil {
		return fmt.Errorf("estimate turn budget: %w", err)
	}
	reservation, err := runtime.Budget.Reserve(ctx, tenantID, turn.RequestID,
		quote.InputTokens+quote.OutputTokens, quote.CostMinorUnits, quote.Currency)
	switch {
	case err == nil:
		execution.reservation = reservation
	case errors.Is(err, domain.ErrBudgetSettled):
		execution.settled = true
	default:
		return fmt.Errorf("reserve turn budget: %w", err)
	}
	failure := runtime.fail(ctx, execution, fmt.Errorf("%w: %s", domain.ErrExecutionLimit, reason))
	if errors.Is(failure, domain.ErrExecutionLimit) {
		return nil
	}
	return failure
}

func hasUsage(usage domain.Usage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.ToolCalls > 0 || usage.CostMinorUnits > 0
}

func (runtime *TurnRuntime) publish(ctx context.Context, execution *turnExecution, payload []byte, final bool, errorCode string) error {
	turn := execution.dispatch.Turn
	err := runtime.Publisher.Publish(ctx, domain.TurnReply{
		TenantID:       turn.TenantContext.TenantID,
		RequestID:      turn.RequestID,
		ConversationID: turn.ConversationID,
		ReplyRoute:     execution.dispatch.ReplyRoute,
		Sequence:       execution.published,
		Payload:        payload,
		Final:          final,
		ErrorCode:      errorCode,
		AgentVersion:   execution.snapshot.AgentVersion,
		EmittedAt:      runtime.now(),
	})
	// An older sequence outside the retained window means the stream already advanced past it.
	if err != nil && !errors.Is(err, domain.ErrReplayExpired) {
		return err
	}
	execution.published++
	return nil
}

func (runtime *TurnRuntime) audit(ctx context.Context, execution *turnExecution, category, errorCode string) {
	if runtime.Audit == nil {
		return
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
	})
	if err != nil {
		return
	}
	outcome := domain.DecisionAllow
	if errorCode != "" {
		outcome = domain.DecisionDeny
	}
	if err := runtime.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: turn.TenantContext.TenantID, Principal: turnPrincipal(turn), ScopeID: turn.TenantContext.ScopeID,
		Category: category, Action: "turn", Resource: turn.AgentID, Outcome: outcome, Reason: errorCode,
		RequestID: turn.RequestID, ConversationID: turn.ConversationID, Payload: payload, OccurredAt: runtime.now(),
	}); err != nil {
		runtime.observeError(ctx, execution, err)
	}
}

func usageAttribution(turn domain.TurnRequest) domain.UsageAttribution {
	return domain.UsageAttribution{Principal: turnPrincipal(turn), ScopeID: turn.TenantContext.ScopeID}
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
