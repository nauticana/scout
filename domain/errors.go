package domain

import "errors"

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
)
