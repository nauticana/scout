// Package clk adapts Scout's injected clock functions to keel's clock.Clock.
package clk

import (
	"context"
	"time"

	"github.com/nauticana/keel/clock"
)

// Of adapts a clock function; nil is the system clock. Waiting stays on real time — callers that inject Now
// control when a cache entry expires, not how long a background sweeper sleeps.
func Of(now func() time.Time) clock.Clock {
	if now == nil {
		return clock.System{}
	}
	return fn(now)
}

type fn func() time.Time

var _ clock.Clock = fn(nil)

func (f fn) Now() time.Time                                   { return f() }
func (f fn) Since(t time.Time) time.Duration                  { return f().Sub(t) }
func (f fn) NewTimer(d time.Duration) clock.Timer             { return clock.System{}.NewTimer(d) }
func (f fn) NewTicker(d time.Duration) clock.Ticker           { return clock.System{}.NewTicker(d) }
func (f fn) Sleep(ctx context.Context, d time.Duration) error { return clock.System{}.Sleep(ctx, d) }
