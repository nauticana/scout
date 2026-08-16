package stage

import (
	"context"
	"errors"
	"time"

	"github.com/nauticana/scout/domain"
)

// ErrorClassInternal is the bounded class for errors matching no domain sentinel.
const ErrorClassInternal = "internal"

var errorClasses = []struct {
	err   error
	class string
}{
	{context.Canceled, "canceled"},
	{domain.ErrTurnCanceled, "canceled"},
	{context.DeadlineExceeded, "deadline"},
	{domain.ErrDeadlineInfeasible, "deadline_infeasible"},
	{domain.ErrRateLimited, "rate_limited"},
	{domain.ErrBudgetExceeded, "budget_exceeded"},
	{domain.ErrExecutionLimit, "execution_limit"},
	{domain.ErrLoopDetected, "loop_detected"},
	{domain.ErrCircuitOpen, "circuit_open"},
	{domain.ErrRevisionConflict, "revision_conflict"},
	{domain.ErrContractFailed, "contract_failed"},
	{domain.ErrValidation, "validation"},
	{domain.ErrNotFound, "not_found"},
	{domain.ErrConflict, "conflict"},
	{domain.ErrUnauthorized, "unauthorized"},
	{domain.ErrForbidden, "forbidden"},
	{domain.ErrNotReady, "not_ready"},
	{domain.ErrNoPrompts, "no_prompts"},
	{domain.ErrReplayExpired, "replay_expired"},
	{domain.ErrNoRoute, "no_route"},
	{domain.ErrStaleEvidence, "stale_evidence"},
	{domain.ErrDegraded, "degraded"},
}

// ErrorClass maps an error to a bounded class by sentinel identity, never by text.
func ErrorClass(err error) string {
	if err == nil {
		return ""
	}
	for _, candidate := range errorClasses {
		if errors.Is(err, candidate.err) {
			return candidate.class
		}
	}
	return ErrorClassInternal
}

// Span measures one stage from Begin to End; callers fill the tenant, route,
// and streaming fields of Observation before End.
type Span struct {
	Observation domain.Observation
}

// Begin opens a span for one stage of one component at now.
func Begin(now time.Time, turnStage domain.TurnStage, component string, versions domain.ComponentVersions) *Span {
	return &Span{Observation: domain.Observation{
		Stage:     turnStage,
		Component: component,
		Versions:  versions,
		StartedAt: now,
	}}
}

// End closes the span. An empty outcome is derived from err (ok, canceled, or
// error); a stage.Error re-attributes the observation to the failing stage.
func (span *Span) End(now time.Time, outcome domain.ObservationOutcome, usage domain.Usage, err error) domain.Observation {
	observation := span.Observation
	observation.Duration = now.Sub(observation.StartedAt)
	observation.Usage = usage
	observation.ErrorClass = ErrorClass(err)
	if outcome == "" {
		outcome = derivedOutcome(observation.ErrorClass)
	}
	observation.Outcome = outcome
	var stageErr *Error
	if errors.As(err, &stageErr) {
		observation.Stage = stageErr.Stage
	}
	return observation
}

func derivedOutcome(errorClass string) domain.ObservationOutcome {
	switch errorClass {
	case "":
		return domain.OutcomeOK
	case "canceled":
		return domain.OutcomeCanceled
	default:
		return domain.OutcomeError
	}
}
