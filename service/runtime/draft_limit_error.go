package runtime

import "github.com/nauticana/scout/domain"

// DraftLimitError is a draft run stopped by its frozen execution limit, carrying the
// usage already settled for the provider work that ran before the stop.
type DraftLimitError struct {
	Err   error
	Usage domain.Usage
}

func (e *DraftLimitError) Error() string            { return e.Err.Error() }
func (e *DraftLimitError) Unwrap() error            { return e.Err }
func (e *DraftLimitError) SpentUsage() domain.Usage { return e.Usage }
