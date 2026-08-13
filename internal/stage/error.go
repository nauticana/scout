// Package stage attributes internal failures to turn lifecycle stages.
package stage

import (
	"fmt"

	"github.com/nauticana/scout/domain"
)

// Error wraps a failure with its lifecycle stage.
type Error struct {
	Stage domain.TurnStage
	Err   error
}

func (e *Error) Error() string { return fmt.Sprintf("%s stage failed: %v", e.Stage, e.Err) }

func (e *Error) Unwrap() error { return e.Err }

// At wraps a non-nil error.
func At(turnStage domain.TurnStage, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Stage: turnStage, Err: err}
}
