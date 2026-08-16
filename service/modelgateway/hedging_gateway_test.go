package modelgateway

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

// budgetRecorder records every reservation lifecycle event of a hedged request.
type budgetRecorder struct {
	mu       sync.Mutex
	reserved []string
	settled  map[string]domain.Usage
	released []string
	failFor  string
}

func newBudgetRecorder() *budgetRecorder {
	return &budgetRecorder{settled: map[string]domain.Usage{}}
}

func (recorder *budgetRecorder) manager() *fake.TenantBudgetManager {
	return &fake.TenantBudgetManager{
		ReserveFunc: func(_ context.Context, tenantID int64, requestID string, tokens, cost int64, currency string) (domain.BudgetReservation, error) {
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			if requestID == recorder.failFor {
				return domain.BudgetReservation{}, domain.ErrBudgetExceeded
			}
			recorder.reserved = append(recorder.reserved, requestID)
			return domain.BudgetReservation{TenantID: tenantID, ReservationID: requestID, RequestID: requestID,
				GrantedTokens: tokens, GrantedCostMinorUnits: cost, Currency: currency}, nil
		},
		CommitFunc: func(_ context.Context, reservation domain.BudgetReservation, usage domain.Usage) error {
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			recorder.settled[reservation.RequestID] = usage
			return nil
		},
		ReleaseFunc: func(_ context.Context, reservation domain.BudgetReservation) error {
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			recorder.released = append(recorder.released, reservation.RequestID)
			return nil
		},
	}
}

func (recorder *budgetRecorder) snapshot() ([]string, map[string]domain.Usage, []string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.reserved...), maps.Clone(recorder.settled), append([]string(nil), recorder.released...)
}

func hedgeRequest() domain.ModelRequest {
	request := validModelRequest()
	request.Idempotent = true
	request.Prompt = []byte("hello")
	return request
}

func fixedPricer() fake.ModelPricerFunc {
	return func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
		return 20, "USD", nil
	}
}

func hedgeRouter(alternative domain.ModelSelection, err error) fake.ModelRouterFunc {
	return func(_ context.Context, request domain.ModelRequest) (domain.ModelSelection, error) {
		if err != nil {
			return domain.ModelSelection{}, err
		}
		for _, excluded := range request.ExcludedRouteIDs {
			if excluded == alternative.RouteID {
				return domain.ModelSelection{}, domain.ErrNoRoute
			}
		}
		return alternative, nil
	}
}

func newHedgingGateway(t *testing.T, inner contract.ModelGateway, router contract.ModelRouter, recorder *budgetRecorder) *HedgingGateway {
	t.Helper()
	gateway, err := NewHedgingGateway(inner, router, recorder.manager(), fixedPricer(), &fake.TenantRateLimiter{}, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gateway.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := gateway.Close(); err != nil {
			t.Fatalf("Close must be idempotent: %v", err)
		}
	})
	return gateway
}

func TestHedgingGatewayValidatesConfig(t *testing.T) {
	recorder := newBudgetRecorder()
	inner := &fake.ModelGateway{}
	router := hedgeRouter(fullSelection(), nil)
	if _, err := NewHedgingGateway(nil, router, recorder.manager(), fixedPricer(), &fake.TenantRateLimiter{}, time.Second); err == nil {
		t.Fatal("expected inner gateway error")
	}
	if _, err := NewHedgingGateway(inner, router, recorder.manager(), fixedPricer(), &fake.TenantRateLimiter{}, 0); err == nil {
		t.Fatal("expected hedge delay error")
	}
	gateway := newHedgingGateway(t, inner, router, recorder)
	if _, err := gateway.Generate(context.Background(), domain.ModelSelection{}, hedgeRequest()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
}

func TestHedgingGatewayGenerateHedgesAndBillsEveryAttempt(t *testing.T) {
	alternative := domain.ModelSelection{Provider: "provider", Model: "model", Region: "us", RouteID: "us-1"}
	var routes []string
	var mu sync.Mutex
	inner := &fake.ModelGateway{GenerateFunc: func(ctx context.Context, selection domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
		mu.Lock()
		routes = append(routes, selection.RouteID)
		mu.Unlock()
		if selection.RouteID == "eu-1" {
			<-ctx.Done()
			return domain.ModelResult{Usage: domain.Usage{InputTokens: 2, Currency: "USD"}}, ctx.Err()
		}
		return domain.ModelResult{Output: []byte("hedge"), Usage: domain.Usage{OutputTokens: 4, Currency: "USD"}}, nil
	}}
	recorder := newBudgetRecorder()
	gateway := newHedgingGateway(t, inner, hedgeRouter(alternative, nil), recorder)

	result, err := gateway.Generate(context.Background(), fullSelection(), hedgeRequest())
	if err != nil || string(result.Output) != "hedge" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	gateway.inFlight.Wait()
	reserved, settled, released := recorder.snapshot()
	if len(reserved) != 2 || reserved[0] != AttemptRequestID("request", 1) || reserved[1] != AttemptRequestID("request", 2) {
		t.Fatalf("reserved = %v", reserved)
	}
	if usage, ok := settled[AttemptRequestID("request", 2)]; !ok || usage.OutputTokens != 4 {
		t.Fatalf("winner settlement = %+v", settled)
	}
	if usage, ok := settled[AttemptRequestID("request", 1)]; !ok || usage.InputTokens != 2 {
		t.Fatalf("cancelled loser must settle its confirmed usage: %+v %v", settled, released)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(routes) != 2 || routes[0] != "eu-1" || routes[1] != "us-1" {
		t.Fatalf("routes = %v", routes)
	}
}

func TestHedgingGatewayHedgesOnlyWhenAllowed(t *testing.T) {
	tests := []struct {
		name       string
		enabled    func() bool
		idempotent bool
		budget     error
		router     error
		wantRoutes int
	}{
		{name: "non-idempotent never hedges", wantRoutes: 1},
		{name: "kill switch", enabled: func() bool { return false }, idempotent: true, wantRoutes: 1},
		{name: "hedge budget exhausted", idempotent: true, budget: domain.ErrRateLimited, wantRoutes: 1},
		{name: "router refuses a second route", idempotent: true, router: domain.ErrNoRoute, wantRoutes: 1},
		{name: "hedged", idempotent: true, wantRoutes: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var routes []string
			release := make(chan struct{})
			inner := &fake.ModelGateway{GenerateFunc: func(ctx context.Context, selection domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
				mu.Lock()
				routes = append(routes, selection.RouteID)
				count := len(routes)
				mu.Unlock()
				if count == 1 {
					select {
					case <-release:
					case <-ctx.Done():
						return domain.ModelResult{}, ctx.Err()
					}
				}
				return domain.ModelResult{Output: []byte("ok")}, nil
			}}
			recorder := newBudgetRecorder()
			alternative := domain.ModelSelection{Provider: "provider", Model: "model", Region: "us", RouteID: "us-1"}
			gateway, err := NewHedgingGateway(inner, hedgeRouter(alternative, test.router), recorder.manager(), fixedPricer(),
				&fake.TenantRateLimiter{AllowModelCallFunc: func(context.Context, domain.ModelRequest) error { return test.budget }}, 20*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			gateway.Enabled = test.enabled
			request := hedgeRequest()
			request.Idempotent = test.idempotent

			done := make(chan error, 1)
			go func() {
				_, err := gateway.Generate(context.Background(), fullSelection(), request)
				done <- err
			}()
			if test.wantRoutes == 1 {
				time.Sleep(60 * time.Millisecond)
				close(release)
			}
			if err := <-done; err != nil {
				t.Fatalf("Generate: %v", err)
			}
			gateway.inFlight.Wait()
			mu.Lock()
			defer mu.Unlock()
			if len(routes) != test.wantRoutes {
				t.Fatalf("routes = %v, want %d", routes, test.wantRoutes)
			}
		})
	}
}

func TestHedgingGatewayStreamCancelsAndSettlesLoser(t *testing.T) {
	var opened, closed atomic.Int64
	blocking := make(chan struct{})
	t.Cleanup(func() { close(blocking) })
	inner := &fake.ModelGateway{StreamFunc: func(ctx context.Context, selection domain.ModelSelection, _ domain.ModelRequest) (contract.ModelStream, error) {
		opened.Add(1)
		primary := selection.RouteID == "eu-1"
		return &fake.ModelStream{
			ReceiveFunc: func(ctx context.Context) (domain.ModelChunk, error) {
				if primary {
					select {
					case <-blocking:
					case <-ctx.Done():
					}
					return domain.ModelChunk{Usage: domain.Usage{InputTokens: 3, Currency: "USD"}}, context.Canceled
				}
				return domain.ModelChunk{Sequence: 1, Payload: []byte("hedge"), Usage: domain.Usage{OutputTokens: 1, Currency: "USD"}}, nil
			},
			CloseFunc: func() error {
				closed.Add(1)
				return nil
			},
		}, nil
	}}
	recorder := newBudgetRecorder()
	alternative := domain.ModelSelection{Provider: "provider", Model: "model", Region: "us", RouteID: "us-1"}
	gateway := newHedgingGateway(t, inner, hedgeRouter(alternative, nil), recorder)

	stream, err := gateway.Stream(context.Background(), fullSelection(), hedgeRequest())
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := stream.Receive(context.Background())
	if err != nil || string(chunk.Payload) != "hedge" {
		t.Fatalf("first frame = %+v %v", chunk, err)
	}
	if _, err = stream.Receive(context.Background()); err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	gateway.inFlight.Wait()
	if opened.Load() != 2 || closed.Load() != 2 {
		t.Fatalf("opened = %d, closed = %d", opened.Load(), closed.Load())
	}
	_, settled, _ := recorder.snapshot()
	if usage, ok := settled[AttemptRequestID("request", 1)]; !ok || usage.InputTokens != 3 {
		t.Fatalf("loser settlement = %+v", settled)
	}
	if usage, ok := settled[AttemptRequestID("request", 2)]; !ok || usage.OutputTokens != 2 {
		t.Fatalf("winner settlement = %+v", settled)
	}
}

// A blocking provider must not survive Close: every attempt goroutine ends.
func TestHedgingGatewayCloseCancelsBlockedAttempts(t *testing.T) {
	started := make(chan struct{}, 2)
	inner := &fake.ModelGateway{GenerateFunc: func(ctx context.Context, _ domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return domain.ModelResult{}, ctx.Err()
	}}
	recorder := newBudgetRecorder()
	alternative := domain.ModelSelection{Provider: "provider", Model: "model", Region: "us", RouteID: "us-1"}
	gateway, err := NewHedgingGateway(inner, hedgeRouter(alternative, nil), recorder.manager(), fixedPricer(), &fake.TenantRateLimiter{}, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := gateway.Generate(context.Background(), fullSelection(), hedgeRequest())
		done <- err
	}()
	<-started
	<-started
	if err := gateway.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	_, _, released := recorder.snapshot()
	if len(released) != 2 {
		t.Fatalf("every started attempt must be released: %v", released)
	}
	if _, err := gateway.Generate(context.Background(), fullSelection(), hedgeRequest()); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("closed gateway = %v", err)
	}
}

func TestHedgingGatewayReportsReservationFailure(t *testing.T) {
	recorder := newBudgetRecorder()
	recorder.failFor = AttemptRequestID("request", 1)
	inner := &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
		t.Fatal("provider must not be called without a reservation")
		return domain.ModelResult{}, nil
	}}
	gateway := newHedgingGateway(t, inner, hedgeRouter(fullSelection(), nil), recorder)
	_, err := gateway.Generate(context.Background(), fullSelection(), hedgeRequest())
	if !errors.Is(err, domain.ErrBudgetExceeded) || !strings.Contains(err.Error(), "attempt 1") {
		t.Fatalf("error = %v", err)
	}
}
