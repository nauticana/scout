package limiter

import (
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
)

// LimitError attaches scope and retry advice to a limit sentinel.
type LimitError struct {
	Err   error
	Scope string
	After time.Duration
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%v: scope %s, retry after %s", e.Err, e.Scope, e.After)
}

func (e *LimitError) Unwrap() error { return e.Err }

func (e *LimitError) RetryAfter() time.Duration { return e.After }

var _ contract.RetryAfterError = (*LimitError)(nil)
