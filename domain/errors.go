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
