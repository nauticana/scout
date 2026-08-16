package modelgateway

import (
	"context"
	"errors"
	"io"

	"github.com/nauticana/scout/domain"
)

// Bounded error classes used as observation and audit labels; never free text.
const (
	ErrorClassNone        = ""
	ErrorClassValidation  = "validation"
	ErrorClassRateLimited = "rate_limited"
	ErrorClassBudget      = "budget"
	ErrorClassForbidden   = "forbidden"
	ErrorClassNoRoute     = "no_route"
	ErrorClassStale       = "stale_evidence"
	ErrorClassInfeasible  = "deadline_infeasible"
	ErrorClassCanceled    = "canceled"
	ErrorClassDeadline    = "deadline"
	ErrorClassFirstToken  = "first_token_timeout"
	ErrorClassIdle        = "idle_timeout"
	ErrorClassDrained     = "route_drained"
	ErrorClassNotReady    = "not_ready"
	ErrorClassCircuitOpen = "circuit_open"
	ErrorClassProvider    = "provider"
)

var errorClasses = []struct {
	target error
	class  string
}{
	{domain.ErrValidation, ErrorClassValidation},
	{domain.ErrRateLimited, ErrorClassRateLimited},
	{domain.ErrBudgetExceeded, ErrorClassBudget},
	{domain.ErrForbidden, ErrorClassForbidden},
	{domain.ErrUnauthorized, ErrorClassForbidden},
	{domain.ErrNoRoute, ErrorClassNoRoute},
	{domain.ErrStaleEvidence, ErrorClassStale},
	{domain.ErrDeadlineInfeasible, ErrorClassInfeasible},
	{domain.ErrNotReady, ErrorClassNotReady},
	{domain.ErrCircuitOpen, ErrorClassCircuitOpen},
	{context.Canceled, ErrorClassCanceled},
	{context.DeadlineExceeded, ErrorClassDeadline},
}

// errorClass maps an error to its bounded label; a stream deadline keeps its own class.
func errorClass(err error) string {
	if err == nil || errors.Is(err, io.EOF) {
		return ErrorClassNone
	}
	var deadline *StreamDeadlineError
	if errors.As(err, &deadline) {
		return deadline.Kind.errorClass()
	}
	for _, candidate := range errorClasses {
		if errors.Is(err, candidate.target) {
			return candidate.class
		}
	}
	return ErrorClassProvider
}

// retryable reports whether a pre-token failure may be attempted again: only
// provider faults and first-token timeouts qualify; policy, budget, and caller
// cancellation never do.
func retryable(err error) bool {
	switch errorClass(err) {
	case ErrorClassProvider, ErrorClassFirstToken, ErrorClassNotReady:
		return true
	}
	return false
}
