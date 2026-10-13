package domain

import (
	"errors"
)

var (
	// ErrNotFound indicates that a requested domain object does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict indicates that a requested state transition conflicts with current state.
	ErrConflict = errors.New("conflict")
	// ErrUnauthorized indicates that valid authentication is required.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden indicates that the tenant is not allowed to perform the operation.
	ErrForbidden = errors.New("forbidden")
	// ErrRateLimited indicates that a tenant admission limit was exceeded.
	ErrRateLimited = errors.New("rate limited")
	// ErrBudgetExceeded indicates that a token or cost budget was exceeded.
	ErrBudgetExceeded = errors.New("budget exceeded")
	// ErrBudgetSettled indicates that a request's reservation was already settled.
	ErrBudgetSettled = errors.New("budget reservation already settled")
	// ErrExecutionLimit indicates that a turn exceeded a configured execution limit.
	ErrExecutionLimit = errors.New("execution limit exceeded")
	// ErrLoopDetected indicates that repeated agent behavior was detected.
	ErrLoopDetected = errors.New("agent loop detected")
	// ErrCircuitOpen indicates that a protected dependency is unavailable.
	ErrCircuitOpen = errors.New("circuit open")
	// ErrRevisionConflict indicates that durable state changed before checkpointing.
	ErrRevisionConflict = errors.New("revision conflict")
	// ErrContractFailed indicates that a compatibility contract did not pass.
	ErrContractFailed = errors.New("contract failed")
	// ErrValidation indicates that submitted domain data failed validation.
	ErrValidation = errors.New("validation failed")
	// ErrNotReady indicates that an agent has no executable published definition.
	ErrNotReady = errors.New("not ready")
	// ErrNoPrompts indicates that no prompt source exists for a requested language.
	ErrNoPrompts = errors.New("no prompts")
	// ErrReplayExpired indicates a reply cursor fell outside the retained window.
	ErrReplayExpired = errors.New("replay window expired")
	// ErrTurnCanceled indicates a turn was canceled by request while running.
	ErrTurnCanceled = errors.New("turn canceled")
	// ErrDeadlineInfeasible indicates the remaining deadline cannot cover the minimum path.
	ErrDeadlineInfeasible = errors.New("deadline infeasible")
	// ErrNoRoute indicates no candidate route satisfies policy, capacity, and deadline.
	ErrNoRoute = errors.New("no eligible route")
	// ErrStaleEvidence indicates telemetry or a decision is too old to act on.
	ErrStaleEvidence = errors.New("stale evidence")
	// ErrDegraded indicates a dependency is serving with reduced guarantees.
	ErrDegraded = errors.New("degraded mode")
	// ErrPrincipalUnknown indicates a transport identity resolved to no principal.
	ErrPrincipalUnknown = errors.New("principal unknown")
	// ErrAuthorityExceeded indicates a binding or delegation broadens what it inherits.
	ErrAuthorityExceeded = errors.New("authority exceeded")
	// ErrDelegationDepth indicates a delegation chain is longer than its grant allows.
	ErrDelegationDepth = errors.New("delegation depth exceeded")
	// ErrGrantExpired indicates a delegation grant is outside its validity window.
	ErrGrantExpired = errors.New("delegation grant expired")
	// ErrSealed indicates a narrower scope tried to override a sealed binding.
	ErrSealed = errors.New("binding sealed")
	// ErrApprovalPending indicates work is parked awaiting a human decision. It is
	// control flow, not a denial: the turn suspends and resumes on the verdict.
	ErrApprovalPending = errors.New("approval pending")
	// ErrProposalRevised indicates a decider edited the proposal; only its revision may run.
	ErrProposalRevised = errors.New("proposal revised")
	// ErrCapabilityUnsupported indicates a route or adapter cannot honor a required
	// capability; the request is refused instead of degraded.
	ErrCapabilityUnsupported = errors.New("capability unsupported")
	// ErrInvalidModelOutput indicates tool arguments or constrained output failed their schema.
	ErrInvalidModelOutput = errors.New("invalid model output")
	// ErrUnsupportedClaim indicates an answer makes a claim its evidence does not verify.
	ErrUnsupportedClaim = errors.New("unsupported claim")
	// ErrEffectViolated indicates a mutation was accepted but its observed effect does not hold.
	ErrEffectViolated = errors.New("effect violated")
	// ErrFenceLost indicates the writer no longer owns the turn: its fence moved on, so the
	// write is refused and the stale worker leaves the turn to its current owner.
	ErrFenceLost = errors.New("turn fence lost")
	// ErrTurnHeld indicates the turn is held; nothing runs it until it is released.
	ErrTurnHeld = errors.New("turn held")
	// ErrEffectUnknown indicates a mutation's effect could not be observed; the outcome is
	// unknown, never success, and the mutation is not invoked again until it can be.
	ErrEffectUnknown = errors.New("effect unknown")
	// ErrOutcomeUnknown indicates a dispatched call of the turn has no definite outcome: the call
	// is recorded unknown and the turn holds until the outcome is reconciled.
	ErrOutcomeUnknown = errors.New("call outcome unknown")
	// ErrNotSent indicates a tool call was refused before it left the platform, so it had no effect.
	ErrNotSent = errors.New("tool call not sent")
	// ErrNoEffect indicates a sent tool call definitely had no effect: its transport proved it never
	// reached the tool, or the tool refused it before acting. A transport marks such a failure;
	// any other failure of a sent mutating call is an unknown outcome.
	ErrNoEffect = errors.New("tool call had no effect")
	// ErrOutputWithheld indicates a tool call took effect but its output cannot be shown, such as
	// output a guardrail blocked; the call completed and the model sees only the failure class.
	ErrOutputWithheld = errors.New("tool output withheld")
	// ErrBudgetUnsettled indicates a turn ended but its reservation was no longer held, so it was
	// not settled; the turn's end stands.
	ErrBudgetUnsettled = errors.New("budget unsettled")
)

// TurnStage identifies the turn-lifecycle boundary that produced an error.
type TurnStage string

const (
	StageAdmission  TurnStage = "admission"
	StageRetrieval  TurnStage = "retrieval"
	StagePrompt     TurnStage = "prompt"
	StageModel      TurnStage = "model"
	StageGuardrail  TurnStage = "guardrail"
	StageTool       TurnStage = "tool"
	StagePublish    TurnStage = "publish"
	StageCheckpoint TurnStage = "checkpoint"
)

// BudgetUnenforceableError reports a bounded release budget that cannot be enforced; it is
// ErrDegraded, so the work it would bound fails closed.
type BudgetUnenforceableError struct {
	Problem BudgetProblem
	Detail  string
}

func (e *BudgetUnenforceableError) Error() string {
	return "budget unenforceable (" + string(e.Problem) + "): " + e.Detail
}

func (e *BudgetUnenforceableError) Unwrap() error { return ErrDegraded }

// AgentNotReadyReason says why an agent cannot serve.
type AgentNotReadyReason string

const (
	AgentNotReadyInactive    AgentNotReadyReason = "inactive"
	AgentNotReadyDisabled    AgentNotReadyReason = "disabled"
	AgentNotReadyUnpublished AgentNotReadyReason = "unpublished"
	AgentNotReadyNoTextModel AgentNotReadyReason = "no_text_model"
	AgentNotReadyNoLanguage  AgentNotReadyReason = "no_language"
)

// AgentNotReadyError is ErrNotReady with the agent and the reason; AgentID is empty when an
// alias has no published target.
type AgentNotReadyError struct {
	AgentID string
	Reason  AgentNotReadyReason
}

func (e *AgentNotReadyError) Error() string {
	if e.AgentID == "" {
		return "agent not ready: " + string(e.Reason)
	}
	return "agent " + e.AgentID + " not ready: " + string(e.Reason)
}

func (e *AgentNotReadyError) Unwrap() error { return ErrNotReady }
