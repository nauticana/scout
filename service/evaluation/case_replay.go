package evaluation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
	"github.com/nauticana/scout/service/dataplane"
)

// Trajectory state names a replay records.
const (
	ReplayCompleted = "completed"
	ReplaySuspended = "suspended"
	ReplayBlocked   = "blocked"
	// ReplayTurn marks the start of every turn after the first; its payload names the turn.
	ReplayTurn = "turn"
)

// replayOutcomes are failures of the agent under test rather than of the
// replay: they are scored as a failed trajectory, not returned as errors.
var replayOutcomes = []error{
	domain.ErrExecutionLimit, domain.ErrBudgetExceeded, domain.ErrLoopDetected, domain.ErrInvalidModelOutput, domain.ErrUnsupportedClaim,
}

// errReplayDuration is the cause of a turn stopped by its subject's max duration.
var errReplayDuration = errors.New("replay duration elapsed")

// caseReplay walks one subject's graph for one example as a conversation: one conversation
// id, a request id per turn, and the snapshot carrying the previous turn's final state. The
// subject's limits, per-turn budget, and output guardrails apply to every turn, and a turn
// that ends suspended, blocked, or failed ends the case.
type caseReplay struct {
	loop    *dataplane.ToolLoopExecutor
	journal contract.LoopJournal
	// fences grants each replayed turn the owner its journal writes present.
	fences *dataplane.MemoryTurnFences
	graph  domain.ExecutionGraph
	// enforcer applies guardrails to every step's output; nil applies none.
	enforcer       contract.GuardrailEnforcer
	guardrails     domain.GuardrailConfig
	principal      domain.Principal
	limits         domain.ExecutionLimits
	budget         domain.ReleaseBudget
	maxSteps       int
	conversationID string
	now            func() time.Time
	started        time.Time

	output     []byte
	usage      domain.Usage
	trajectory []domain.TrajectoryEvent
}

func (replay *caseReplay) clock() time.Time {
	if replay.now != nil {
		return replay.now()
	}
	return time.Now()
}

// run replays every turn in order; ctx is the caller's, whose own end is never scored as the agent's.
func (replay *caseReplay) run(ctx context.Context, inputs [][]byte) error {
	replay.started = replay.clock()
	snapshot := domain.SessionSnapshot{ConversationID: replay.conversationID, AgentVersion: replay.principal.Release}
	for i, input := range inputs {
		turnNo := int64(i + 1)
		if turnNo > 1 {
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: ReplayTurn,
				Payload: []byte(`{"turn":` + strconv.FormatInt(turnNo, 10) + `}`), Offset: replay.offset()})
		}
		completed, err := replay.turn(ctx, turnNo, input, &snapshot)
		if err != nil || !completed {
			return err
		}
	}
	return nil
}

// requestID keeps the first turn's request the conversation's id, as a single-turn replay has always had it.
func (replay *caseReplay) requestID(turnNo int64) string {
	if turnNo == 1 {
		return replay.conversationID
	}
	return replay.conversationID + "/turn/" + strconv.FormatInt(turnNo, 10)
}

// turn walks the graph for one turn within the subject's duration and reports whether it completed.
func (replay *caseReplay) turn(ctx context.Context, turnNo int64, input []byte, snapshot *domain.SessionSnapshot) (bool, error) {
	steps := make(map[string]domain.ExecutionStep, len(replay.graph.Steps))
	for _, step := range replay.graph.Steps {
		steps[step.StepID] = step
	}
	maxSteps := replay.maxSteps
	if maxSteps == 0 {
		maxSteps = len(replay.graph.Steps)
	}
	if limit := replay.limits.MaxSteps; limit > 0 && limit < maxSteps {
		maxSteps = limit
	}
	turnStarted := replay.clock()
	walkCtx := ctx
	if duration := replay.limits.MaxDuration; duration > 0 {
		var cancel context.CancelFunc
		walkCtx, cancel = context.WithTimeoutCause(ctx, duration, errReplayDuration)
		defer cancel()
	}
	requestID := replay.requestID(turnNo)
	owner, err := replay.fences.Acquire(replay.principal.TenantID, requestID)
	if err != nil {
		return false, err
	}
	replay.output = nil
	var turnUsage domain.Usage
	stepID := replay.graph.EntryStepID
	for stepNo := 1; ; stepNo++ {
		if stepNo > maxSteps {
			return false, replay.end(ctx, fmt.Errorf("%w: replay exceeded %d steps", domain.ErrExecutionLimit, maxSteps))
		}
		if duration := replay.limits.MaxDuration; duration > 0 && replay.clock().Sub(turnStarted) >= duration {
			return false, replay.end(ctx, fmt.Errorf("%w: replay exceeded its %s duration", domain.ErrExecutionLimit, duration))
		}
		step, found := steps[stepID]
		if !found {
			return false, fmt.Errorf("%w: execution step %q of agent %q", domain.ErrNotFound, stepID, replay.graph.AgentID)
		}
		if step.Kind != domain.StepKindToolLoop {
			return false, fmt.Errorf("%w: replay runs %s steps, not %q", domain.ErrCapabilityUnsupported, domain.StepKindToolLoop, step.Kind)
		}
		// The per-turn budget bounds a replayed turn as it bounds a live one.
		spend, err := replay.budget.StepSpend(turnUsage)
		if err != nil {
			return false, replay.end(ctx, err)
		}
		result, err := replay.loop.Execute(walkCtx, domain.StepInput{
			Step: step, Snapshot: *snapshot, Principal: replay.principal, RequestID: requestID, Input: input,
			Limits: replay.limits, Spend: spend, Owner: owner,
		})
		if recordErr := replay.record(ctx, requestID, step); recordErr != nil {
			return false, errors.Join(err, recordErr)
		}
		var spent interface{ SpentUsage() domain.Usage }
		if errors.As(err, &spent) {
			replay.usage = addUsage(replay.usage, spent.SpentUsage())
		}
		if err != nil {
			return false, replay.end(ctx, durationOutcome(walkCtx, err))
		}
		replay.usage, turnUsage = addUsage(replay.usage, result.Usage), addUsage(turnUsage, result.Usage)
		state, blocked, err := replay.guard(walkCtx, requestID, result.State)
		if err != nil && errors.Is(context.Cause(walkCtx), errReplayDuration) {
			return false, replay.end(ctx, durationOutcome(walkCtx, err))
		}
		if err != nil || blocked {
			return false, err
		}
		snapshot.State, snapshot.LastCompletedStepID = state, step.StepID
		snapshot.LatestTurnNo, snapshot.LatestStepNo = turnNo, stepNo
		if strings.TrimSpace(result.NextStepID) == "" {
			replay.output = state
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: ReplayCompleted, Payload: state, Offset: replay.offset()})
			return true, nil
		}
		stepID = result.NextStepID
	}
}

// guard applies the subject's output guardrails; a blocked output ends the replay.
func (replay *caseReplay) guard(ctx context.Context, requestID string, state []byte) ([]byte, bool, error) {
	if replay.enforcer == nil {
		return state, false, nil
	}
	guarded, err := replay.enforcer.AfterModelChunk(ctx, replay.guardrails, domain.GuardrailSubject{
		TenantID: replay.principal.TenantID, Principal: domain.PrincipalRef{Kind: replay.principal.Kind, ID: replay.principal.ID},
		RequestID: requestID, ConversationID: replay.conversationID, ReleaseVersion: replay.principal.Release,
	}, domain.ModelChunk{Payload: state})
	if errors.Is(err, domain.ErrForbidden) {
		replay.sanitizeAnswer(nil, true)
		replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryPolicy, Name: "guardrail", Failed: true, Offset: replay.offset()})
		replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: ReplayBlocked, Failed: true, Offset: replay.offset()})
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("apply output guardrails: %w", err)
	}
	replay.sanitizeAnswer(guarded.Payload, false)
	return guarded.Payload, false, nil
}

func (replay *caseReplay) sanitizeAnswer(payload []byte, failed bool) {
	for i := len(replay.trajectory) - 1; i >= 0; i-- {
		if event := &replay.trajectory[i]; event.Kind == domain.TrajectoryState && event.Name == "answer" {
			event.Payload, event.Failed = append([]byte(nil), payload...), failed
			return
		}
	}
}

// durationOutcome marks a failure caused by the turn's duration running out as the
// agent's execution limit.
func durationOutcome(walkCtx context.Context, err error) error {
	if errors.Is(context.Cause(walkCtx), errReplayDuration) && !errors.Is(err, domain.ErrExecutionLimit) {
		return fmt.Errorf("%w: replay exceeded its duration: %w", domain.ErrExecutionLimit, err)
	}
	return err
}

// end records a failure of the agent under test as the trajectory's final
// state and returns every other failure, including the caller's own cancellation.
func (replay *caseReplay) end(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	if errors.Is(err, domain.ErrApprovalPending) {
		replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: ReplaySuspended, Offset: replay.offset()})
		return nil
	}
	for _, outcome := range replayOutcomes {
		if errors.Is(err, outcome) {
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: stage.ErrorClass(err), Failed: true, Offset: replay.offset()})
			return nil
		}
	}
	return err
}

// record reads the step's loop journal onto the trajectory, which also keeps
// the calls of a loop that failed before returning its events.
func (replay *caseReplay) record(ctx context.Context, requestID string, step domain.ExecutionStep) error {
	entries, err := replay.journal.Load(ctx, domain.LoopKey{TenantID: replay.principal.TenantID, RequestID: requestID, ExecutionStepID: step.ExecutionStepID})
	if err != nil {
		return fmt.Errorf("read replay journal: %w", err)
	}
	toolIDs := make(map[string]string)
	for _, entry := range entries {
		if entry.Kind == domain.LoopEntryObservation && entry.Observation != nil {
			toolIDs[entry.Observation.CallID] = entry.Tool.ToolID
		}
	}
	failed := make(map[string]bool)
	for _, entry := range entries {
		switch entry.Kind {
		case domain.LoopEntryModel:
			if entry.Model == nil {
				continue
			}
			if len(entry.Model.ToolCalls) == 0 {
				replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: "answer", Payload: entry.Model.Output, Offset: entry.Offset, Usage: entry.Usage})
			}
			for i, call := range entry.Model.ToolCalls {
				name := toolIDs[call.CallID]
				if name == "" {
					name = call.Name
				}
				usage := entry.Usage
				if i > 0 {
					usage = domain.Usage{}
				}
				replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryToolCall, Name: name, Payload: call.Arguments, Offset: entry.Offset, Usage: usage})
			}
		case domain.LoopEntryApprovalPending:
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryPolicy, Name: entry.Tool.ToolID, Offset: entry.Offset})
		case domain.LoopEntryRevision:
			if entry.Revision != nil {
				replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryPolicy, Name: entry.Tool.ToolID, Payload: entry.Revision.Arguments, Offset: entry.Offset})
			}
		case domain.LoopEntryObservation:
			if entry.Observation == nil {
				continue
			}
			toolID, observation := entry.Tool.ToolID, entry.Observation
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryObservation, Name: toolID, Payload: observation.Output, Offset: entry.Offset,
				Usage: entry.Usage, Failed: observation.IsError, Recovered: !observation.IsError && failed[toolID]})
			failed[toolID] = observation.IsError
		}
	}
	return nil
}

func (replay *caseReplay) event(event domain.TrajectoryEvent) {
	event.Sequence = int64(len(replay.trajectory) + 1)
	replay.trajectory = append(replay.trajectory, event)
}

func (replay *caseReplay) offset() time.Duration {
	return replay.clock().Sub(replay.started)
}

// evaluationCase is the replayed case: the last turn's output, every turn's trajectory and usage.
func (replay *caseReplay) evaluationCase(example domain.GoldenExample, matches []domain.KnowledgeMatch) domain.EvaluationCase {
	return domain.EvaluationCase{
		Example: example, Output: replay.output, Retrieval: matches, Trajectory: replay.trajectory,
		Latency: replay.offset(), Usage: replay.usage,
	}
}
