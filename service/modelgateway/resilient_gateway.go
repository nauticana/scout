package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

// DeadlineKind names which streaming budget was exhausted.
type DeadlineKind string

const (
	DeadlineFirstToken DeadlineKind = "first_token"
	DeadlineIdle       DeadlineKind = "idle"
	DeadlineTotal      DeadlineKind = "total"
	DeadlineDrain      DeadlineKind = "drain"
)

func (kind DeadlineKind) errorClass() string {
	switch kind {
	case DeadlineFirstToken:
		return ErrorClassFirstToken
	case DeadlineIdle:
		return ErrorClassIdle
	case DeadlineDrain:
		return ErrorClassDrained
	}
	return ErrorClassDeadline
}

// StreamDeadlineError is the typed class of one exhausted streaming budget; it
// unwraps to context.DeadlineExceeded and is attributed to StageModel.
type StreamDeadlineError struct {
	Kind  DeadlineKind
	Limit time.Duration
}

func (e *StreamDeadlineError) Error() string {
	return fmt.Sprintf("model stream %s deadline %s exceeded", e.Kind, e.Limit)
}

func (e *StreamDeadlineError) Unwrap() error { return context.DeadlineExceeded }

// StreamDeadlines are the three independent streaming budgets, all required.
type StreamDeadlines struct {
	// FirstToken bounds connect plus time to the first payload frame.
	FirstToken time.Duration
	// Idle bounds the gap between consecutive frames after the first token.
	Idle time.Duration
	// Total bounds the whole call including pre-token retries.
	Total time.Duration
}

func (deadlines StreamDeadlines) validate() error {
	if deadlines.FirstToken <= 0 || deadlines.Idle <= 0 || deadlines.Total <= 0 {
		return fmt.Errorf("resilient gateway: first-token, idle, and total deadlines must be positive")
	}
	if deadlines.Total < deadlines.FirstToken || deadlines.Total < deadlines.Idle {
		return fmt.Errorf("resilient gateway: total deadline must cover the first-token and idle deadlines")
	}
	return nil
}

// ResilientGateway decorates a ModelGateway with split streaming deadlines, a
// bounded pre-token retry, drain enforcement, and interrupted partial completions.
// After the first token nothing is retried: the stream ends with
// FinishReasonInterrupted and the typed cause.
type ResilientGateway struct {
	Inner     contract.ModelGateway
	Deadlines StreamDeadlines
	// MaxPreTokenRetries bounds re-attempts before any token; zero disables retry.
	MaxPreTokenRetries int
	// RetryBackoff is the base of the doubling, fully jittered backoff between attempts.
	RetryBackoff time.Duration
	// Routes enforces drain admission and drain deadlines when set.
	Routes contract.RouteSnapshotLookup
	// Sleep waits between attempts; nil uses a ctx-bounded timer.
	Sleep func(ctx context.Context, delay time.Duration) error
	// Rand yields the jitter fraction in [0,1); nil uses math/rand/v2.
	Rand func() float64
	Now  func() time.Time
}

var _ contract.ModelGateway = (*ResilientGateway)(nil)

// NewResilientGateway builds the decorator with validated deadlines and retry policy.
func NewResilientGateway(inner contract.ModelGateway, deadlines StreamDeadlines, maxPreTokenRetries int, retryBackoff time.Duration) (*ResilientGateway, error) {
	gateway := &ResilientGateway{Inner: inner, Deadlines: deadlines, MaxPreTokenRetries: maxPreTokenRetries, RetryBackoff: retryBackoff}
	if err := gateway.validate(); err != nil {
		return nil, err
	}
	return gateway, nil
}

func (gateway *ResilientGateway) validate() error {
	if gateway.Inner == nil {
		return fmt.Errorf("resilient gateway: inner gateway is required")
	}
	if err := gateway.Deadlines.validate(); err != nil {
		return err
	}
	if gateway.MaxPreTokenRetries < 0 || gateway.MaxPreTokenRetries > 0 && gateway.RetryBackoff <= 0 {
		return fmt.Errorf("resilient gateway: retries need a non-negative count and a positive backoff")
	}
	return nil
}

func (gateway *ResilientGateway) now() time.Time {
	if gateway.Now == nil {
		return time.Now()
	}
	return gateway.Now()
}

func (gateway *ResilientGateway) sleep(ctx context.Context, delay time.Duration) error {
	if gateway.Sleep != nil {
		return gateway.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoff waits RetryBackoff·2^(attempt-1) scaled by a jitter fraction, capped by the budget left.
func (gateway *ResilientGateway) backoff(ctx context.Context, attempt int, remaining time.Duration) error {
	fraction := rand.Float64
	if gateway.Rand != nil {
		fraction = gateway.Rand
	}
	delay := gateway.RetryBackoff << (attempt - 1)
	if delay <= 0 || delay > remaining {
		delay = remaining
	}
	delay = time.Duration(float64(delay) * fraction())
	if delay <= 0 {
		return ctx.Err()
	}
	return gateway.sleep(ctx, delay)
}

// admit rejects new work for a route that is draining.
func (gateway *ResilientGateway) admit(ctx context.Context, selection domain.ModelSelection) error {
	if gateway.Routes == nil {
		return nil
	}
	snapshot, ok, err := gateway.Routes.SnapshotFor(ctx, selection)
	if err != nil {
		return fmt.Errorf("route snapshot: %w", err)
	}
	if ok && snapshot.Draining {
		return fmt.Errorf("%w: route %s is draining", domain.ErrNoRoute, selectionRouteKey(selection))
	}
	return nil
}

func (gateway *ResilientGateway) drainDeadline(ctx context.Context, selection domain.ModelSelection) (time.Time, error) {
	if gateway.Routes == nil {
		return time.Time{}, nil
	}
	snapshot, ok, err := gateway.Routes.SnapshotFor(ctx, selection)
	if err != nil {
		return time.Time{}, fmt.Errorf("route snapshot: %w", err)
	}
	if !ok || !snapshot.Draining {
		return time.Time{}, nil
	}
	return snapshot.DrainDeadline, nil
}

// Generate retries a failed call within the total deadline; the whole result
// arrives at once, so every failure is pre-token.
func (gateway *ResilientGateway) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	if err := gateway.validate(); err != nil {
		return domain.ModelResult{}, err
	}
	if err := gateway.admit(ctx, selection); err != nil {
		return domain.ModelResult{}, stageModel(err)
	}
	started := gateway.now()
	for attempt := 0; ; attempt++ {
		remaining := gateway.Deadlines.Total - gateway.now().Sub(started)
		if remaining <= 0 {
			return domain.ModelResult{}, stageModel(&StreamDeadlineError{Kind: DeadlineTotal, Limit: gateway.Deadlines.Total})
		}
		callCtx, cancel := context.WithTimeout(ctx, remaining)
		result, err := gateway.Inner.Generate(callCtx, selection, request)
		cancel()
		if err == nil {
			return result, nil
		}
		err = gateway.classify(ctx, callCtx, err, DeadlineTotal, gateway.Deadlines.Total)
		if attempt >= gateway.MaxPreTokenRetries || !retryable(err) {
			return result, stageModel(err)
		}
		remaining = gateway.Deadlines.Total - gateway.now().Sub(started)
		if sleepErr := gateway.backoff(ctx, attempt+1, remaining); sleepErr != nil {
			return domain.ModelResult{}, stageModel(errors.Join(err, sleepErr))
		}
	}
}

// classify turns a timeout on the derived context into the typed deadline class,
// leaving caller cancellation and provider errors untouched.
func (gateway *ResilientGateway) classify(parent, derived context.Context, err error, kind DeadlineKind, limit time.Duration) error {
	if parent.Err() == nil && derived.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
		return &StreamDeadlineError{Kind: kind, Limit: limit}
	}
	return err
}

// Stream opens the first attempt; pre-token retries continue inside Receive.
func (gateway *ResilientGateway) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	if err := gateway.validate(); err != nil {
		return nil, err
	}
	if err := gateway.admit(ctx, selection); err != nil {
		return nil, stageModel(err)
	}
	stream := &resilientStream{gateway: gateway, streamCtx: ctx, selection: selection, request: request, started: gateway.now()}
	if err := stream.open(); err != nil {
		return nil, err
	}
	return stream, nil
}

func stageModel(err error) error {
	var staged *stage.Error
	if err == nil || errors.As(err, &staged) {
		return err
	}
	return stage.At(domain.StageModel, err)
}
