package handler

import (
	"errors"
	"net/http"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/domain"
)

// errorStatuses is the HTTP status of each Scout sentinel a handler answers; the first match wins.
var errorStatuses = []struct {
	err    error
	status int
}{
	{domain.ErrValidation, http.StatusBadRequest},
	{domain.ErrNoPrompts, http.StatusBadRequest},
	{domain.ErrUnauthorized, http.StatusUnauthorized},
	{domain.ErrPrincipalUnknown, http.StatusUnauthorized},
	{domain.ErrForbidden, http.StatusForbidden},
	{domain.ErrAuthorityExceeded, http.StatusForbidden},
	{domain.ErrNotFound, http.StatusNotFound},
	{domain.ErrConflict, http.StatusConflict},
	{domain.ErrRevisionConflict, http.StatusConflict},
	{domain.ErrSealed, http.StatusConflict},
	{domain.ErrFenceLost, http.StatusConflict},
	{domain.ErrTurnHeld, http.StatusConflict},
	{domain.ErrRateLimited, http.StatusTooManyRequests},
	{domain.ErrBudgetExceeded, http.StatusTooManyRequests},
	{domain.ErrNotReady, http.StatusServiceUnavailable},
	{domain.ErrCircuitOpen, http.StatusServiceUnavailable},
	{domain.ErrReplayExpired, http.StatusGone},
	{domain.ErrExecutionLimit, http.StatusUnprocessableEntity},
}

// init registers Scout's sentinels with keel's error registry, so keel's WriteServiceError answers them.
func init() {
	for _, entry := range errorStatuses {
		keelhandler.RegisterErrorStatus(entry.err, entry.status)
	}
}

// errorStatus is the status of err's first matching sentinel, or 0 for an unclassified error.
func errorStatus(err error) int {
	for _, entry := range errorStatuses {
		if errors.Is(err, entry.err) {
			return entry.status
		}
	}
	return 0
}
