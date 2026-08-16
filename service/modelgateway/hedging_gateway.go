package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// AttemptRequestID derives the budget identity of one attempt of a logical request,
// so hedged attempts hold independently fenced reservations.
func AttemptRequestID(requestID string, attempt int) string {
	return requestID + "#" + strconv.Itoa(attempt)
}

// HedgingGateway decorates a ModelGateway: when the primary attempt yields no
// result within HedgeDelay it starts one more attempt on a different route, keeps
// the first to answer, and cancels the other. Only idempotent requests hedge, each
// started attempt holds its own budget reservation settled from confirmed usage,
// and Enabled is the fleet-wide kill switch.
type HedgingGateway struct {
	Inner   contract.ModelGateway
	Router  contract.ModelRouter
	Budgets contract.TenantBudgetManager
	// Pricer sizes each attempt's cost reservation in the catalog currency.
	Pricer contract.ModelPricer
	// HedgeBudget admits hedge attempts per tenant and fleet, e.g. isolation.NewTenantRateLimiter with only Model set.
	HedgeBudget contract.TenantRateLimiter
	HedgeDelay  time.Duration
	// Enabled is the kill switch; nil means enabled.
	Enabled      func() bool
	PromptTokens func([]byte) int64
	Now          func() time.Time

	once     sync.Once
	lifetime context.Context
	shutdown context.CancelFunc
	inFlight sync.WaitGroup
	closed   atomic.Bool
}

var _ contract.ModelGateway = (*HedgingGateway)(nil)

// NewHedgingGateway validates the required collaborators and delay.
func NewHedgingGateway(inner contract.ModelGateway, router contract.ModelRouter, budgets contract.TenantBudgetManager, pricer contract.ModelPricer, hedgeBudget contract.TenantRateLimiter, hedgeDelay time.Duration) (*HedgingGateway, error) {
	gateway := &HedgingGateway{Inner: inner, Router: router, Budgets: budgets, Pricer: pricer, HedgeBudget: hedgeBudget, HedgeDelay: hedgeDelay}
	if err := gateway.validate(); err != nil {
		return nil, err
	}
	return gateway, nil
}

func (gateway *HedgingGateway) validate() error {
	if gateway.Inner == nil || gateway.Router == nil || gateway.Budgets == nil || gateway.Pricer == nil || gateway.HedgeBudget == nil {
		return fmt.Errorf("hedging gateway: inner gateway, router, budget manager, pricer, and hedge budget are required")
	}
	if gateway.HedgeDelay <= 0 {
		return fmt.Errorf("hedging gateway: hedge delay must be positive")
	}
	gateway.once.Do(func() { gateway.lifetime, gateway.shutdown = context.WithCancel(context.Background()) })
	if gateway.closed.Load() {
		return fmt.Errorf("%w: hedging gateway is closed", domain.ErrNotReady)
	}
	return nil
}

func (gateway *HedgingGateway) enabled() bool {
	return gateway.Enabled == nil || gateway.Enabled()
}

// Close cancels every in-flight attempt and waits for their settlement; it is idempotent.
func (gateway *HedgingGateway) Close() error {
	gateway.once.Do(func() { gateway.lifetime, gateway.shutdown = context.WithCancel(context.Background()) })
	if gateway.closed.CompareAndSwap(false, true) {
		gateway.shutdown()
	}
	gateway.inFlight.Wait()
	return nil
}

// attempt is one reserved, cancellable try of a logical request.
type attempt struct {
	number      int
	selection   domain.ModelSelection
	reservation domain.BudgetReservation
	ctx         context.Context
	cancel      context.CancelFunc
	settled     atomic.Bool
}

// start reserves the attempt's budget and binds its context to the caller and gateway lifetime.
func (gateway *HedgingGateway) start(ctx context.Context, number int, selection domain.ModelSelection, request domain.ModelRequest) (*attempt, error) {
	tokens := promptTokens(gateway.PromptTokens, request.Prompt) + request.MaxOutputTokens
	cost, currency, err := gateway.Pricer.Cost(ctx, domain.ModelReference{ProviderID: selection.Provider, ModelID: selection.Model},
		domain.ModelUsage{InputTokens: tokens - request.MaxOutputTokens, OutputTokens: request.MaxOutputTokens})
	if err != nil {
		return nil, fmt.Errorf("price attempt %d: %w", number, err)
	}
	reservation, err := gateway.Budgets.Reserve(ctx, request.TenantContext.TenantID, AttemptRequestID(request.RequestID, number), tokens, cost, currency)
	if err != nil {
		return nil, fmt.Errorf("reserve attempt %d: %w", number, err)
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(gateway.lifetime, cancel)
	return &attempt{number: number, selection: selection, reservation: reservation, ctx: attemptCtx, cancel: func() { stop(); cancel() }}, nil
}

// settle commits confirmed usage or releases an unused hold, exactly once.
func (gateway *HedgingGateway) settle(ctx context.Context, attempt *attempt, usage domain.Usage) error {
	if !attempt.settled.CompareAndSwap(false, true) {
		return nil
	}
	defer attempt.cancel()
	ctx = context.WithoutCancel(ctx)
	if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CostMinorUnits > 0 {
		return gateway.Budgets.Commit(ctx, attempt.reservation, usage)
	}
	return gateway.Budgets.Release(ctx, attempt.reservation)
}

func (gateway *HedgingGateway) hedgeable(request domain.ModelRequest) bool {
	return gateway.enabled() && request.Idempotent
}

// hedgeSelection admits a hedge and routes it away from the primary route.
func (gateway *HedgingGateway) hedgeSelection(ctx context.Context, primary domain.ModelSelection, request domain.ModelRequest) (domain.ModelSelection, error) {
	if err := gateway.HedgeBudget.AllowModelCall(ctx, request); err != nil {
		return domain.ModelSelection{}, fmt.Errorf("hedge budget: %w", err)
	}
	if primary.RouteID != "" {
		request.ExcludedRouteIDs = append(append([]string(nil), request.ExcludedRouteIDs...), primary.RouteID)
	}
	alternative, err := gateway.Router.Select(ctx, request)
	if err != nil {
		return domain.ModelSelection{}, fmt.Errorf("hedge route: %w", err)
	}
	if alternative.RouteID != "" && alternative.RouteID == primary.RouteID {
		return domain.ModelSelection{}, fmt.Errorf("%w: router returned the primary route for the hedge", domain.ErrNoRoute)
	}
	return alternative, nil
}

func (gateway *HedgingGateway) checkRequest(selection domain.ModelSelection, request domain.ModelRequest) error {
	if err := gateway.validate(); err != nil {
		return err
	}
	if request.TenantContext.TenantID <= 0 || strings.TrimSpace(request.RequestID) == "" || request.MaxOutputTokens <= 0 ||
		strings.TrimSpace(selection.Provider) == "" || strings.TrimSpace(selection.Model) == "" {
		return fmt.Errorf("%w: tenant, request id, positive max output tokens, provider, and model are required", domain.ErrValidation)
	}
	return nil
}

type generateOutcome struct {
	attempt *attempt
	result  domain.ModelResult
	err     error
}

// Generate races the primary against one delayed hedge; the first success wins
// and the other attempt is canceled and settled in the background.
func (gateway *HedgingGateway) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	if err := gateway.checkRequest(selection, request); err != nil {
		return domain.ModelResult{}, err
	}
	primary, err := gateway.start(ctx, 1, selection, request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	race := &race[generateOutcome]{gateway: gateway, outcomes: make(chan generateOutcome, 2)}
	generate := func(current *attempt) generateOutcome {
		result, err := gateway.Inner.Generate(current.ctx, current.selection, request)
		return generateOutcome{attempt: current, result: result, err: err}
	}
	race.run(primary, generate)
	var failures []error
	hedgeTimer, stopTimer := gateway.hedgeTimer(ctx, request)
	defer stopTimer()
	for race.running > 0 {
		select {
		case <-hedgeTimer:
			hedgeTimer = nil
			hedge, hedgeErr := gateway.hedge(ctx, selection, request)
			if hedgeErr != nil {
				failures = append(failures, hedgeErr)
				continue
			}
			race.run(hedge, generate)
		case outcome := <-race.outcomes:
			race.running--
			settleErr := gateway.settle(ctx, outcome.attempt, outcome.result.Usage)
			if outcome.err != nil {
				failures = append(failures, outcome.err, settleErr)
				continue
			}
			race.abandonOthers(ctx, outcome.attempt, func(loser generateOutcome) {
				_ = gateway.settle(ctx, loser.attempt, loser.result.Usage)
			})
			return outcome.result, settleErr
		}
	}
	return domain.ModelResult{}, errors.Join(failures...)
}

// race tracks the attempts of one logical request and their in-flight goroutines.
type race[T any] struct {
	gateway  *HedgingGateway
	attempts []*attempt
	outcomes chan T
	running  int
}

func (r *race[T]) run(current *attempt, work func(*attempt) T) {
	r.attempts = append(r.attempts, current)
	r.running++
	r.gateway.inFlight.Add(1)
	go func() {
		defer r.gateway.inFlight.Done()
		r.outcomes <- work(current)
	}()
}

// abandonOthers cancels every attempt but the winner and settles the losers as they finish.
func (r *race[T]) abandonOthers(ctx context.Context, winner *attempt, discard func(T)) {
	for _, current := range r.attempts {
		if current != winner {
			current.cancel()
		}
	}
	for ; r.running > 0; r.running-- {
		r.gateway.inFlight.Add(1)
		go func() {
			defer r.gateway.inFlight.Done()
			discard(<-r.outcomes)
		}()
	}
}

// hedgeTimer fires once after HedgeDelay when the request may hedge; nil otherwise.
func (gateway *HedgingGateway) hedgeTimer(ctx context.Context, request domain.ModelRequest) (<-chan struct{}, func()) {
	if !gateway.hedgeable(request) {
		return nil, func() {}
	}
	delayCtx, cancel := context.WithTimeout(ctx, gateway.HedgeDelay)
	return delayCtx.Done(), cancel
}

func (gateway *HedgingGateway) hedge(ctx context.Context, primary domain.ModelSelection, request domain.ModelRequest) (*attempt, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	alternative, err := gateway.hedgeSelection(ctx, primary, request)
	if err != nil {
		return nil, err
	}
	return gateway.start(ctx, 2, alternative, request)
}

type streamOutcome struct {
	attempt *attempt
	stream  contract.ModelStream
	first   domain.ModelChunk
	err     error
}

// Stream races the primary against one delayed hedge until a first frame arrives;
// the winner's stream is returned with that frame replayed, the loser is closed and settled.
func (gateway *HedgingGateway) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	if err := gateway.checkRequest(selection, request); err != nil {
		return nil, err
	}
	primary, err := gateway.start(ctx, 1, selection, request)
	if err != nil {
		return nil, err
	}
	race := &race[streamOutcome]{gateway: gateway, outcomes: make(chan streamOutcome, 2)}
	open := func(current *attempt) streamOutcome { return gateway.openAttempt(current, request) }
	race.run(primary, open)
	var winner *streamOutcome
	var failures []error
	hedgeTimer, stopTimer := gateway.hedgeTimer(ctx, request)
	defer stopTimer()
	for race.running > 0 && winner == nil {
		select {
		case <-hedgeTimer:
			hedgeTimer = nil
			hedge, hedgeErr := gateway.hedge(ctx, selection, request)
			if hedgeErr != nil {
				failures = append(failures, hedgeErr)
				continue
			}
			race.run(hedge, open)
		case outcome := <-race.outcomes:
			race.running--
			if outcome.err == nil {
				winner = &outcome
				continue
			}
			failures = append(failures, outcome.err, gateway.settle(ctx, outcome.attempt, outcome.first.Usage))
		}
	}
	if winner == nil {
		return nil, errors.Join(failures...)
	}
	race.abandonOthers(ctx, winner.attempt, func(loser streamOutcome) { gateway.discard(ctx, loser) })
	return &leasedModelStream{
		stream:     &prefixedStream{first: winner.first, inner: winner.stream},
		lease:      &attemptLease{gateway: gateway, attempt: winner.attempt},
		releaseCtx: context.WithoutCancel(ctx),
	}, nil
}

// openAttempt opens the stream and waits for its first frame under the attempt context.
func (gateway *HedgingGateway) openAttempt(current *attempt, request domain.ModelRequest) streamOutcome {
	stream, err := gateway.Inner.Stream(current.ctx, current.selection, request)
	if err == nil && stream == nil {
		err = errors.New("model gateway returned a nil stream")
	}
	if err != nil {
		return streamOutcome{attempt: current, err: err}
	}
	first, err := stream.Receive(current.ctx)
	if err != nil {
		return streamOutcome{attempt: current, first: first, err: errors.Join(err, stream.Close())}
	}
	return streamOutcome{attempt: current, stream: stream, first: first}
}

// discard closes a canceled loser and settles whatever usage its first frame confirmed.
func (gateway *HedgingGateway) discard(ctx context.Context, loser streamOutcome) {
	if loser.stream != nil {
		_ = loser.stream.Close()
	}
	_ = gateway.settle(ctx, loser.attempt, loser.first.Usage)
}

// attemptLease settles the winning attempt's reservation when its stream finishes.
type attemptLease struct {
	gateway *HedgingGateway
	attempt *attempt
}

func (lease *attemptLease) Pool() string { return "hedge" }

func (lease *attemptLease) Release(ctx context.Context, usage domain.Usage) error {
	return lease.gateway.settle(ctx, lease.attempt, usage)
}

// prefixedStream replays the frame consumed while racing before delegating.
type prefixedStream struct {
	mu       sync.Mutex
	first    domain.ModelChunk
	replayed bool
	inner    contract.ModelStream
}

func (stream *prefixedStream) Receive(ctx context.Context) (domain.ModelChunk, error) {
	stream.mu.Lock()
	replayed := stream.replayed
	stream.replayed = true
	stream.mu.Unlock()
	if !replayed {
		return stream.first, nil
	}
	return stream.inner.Receive(ctx)
}

func (stream *prefixedStream) Close() error { return stream.inner.Close() }

var _ contract.CapacityLease = (*attemptLease)(nil)
var _ contract.ModelStream = (*prefixedStream)(nil)
