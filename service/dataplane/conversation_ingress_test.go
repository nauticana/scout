package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

type ingressRecorder struct {
	calls        []string
	subscription *fake.TurnReplySubscription
	released     int
	failed       []string
}

func newTestIngress(t *testing.T, recorder *ingressRecorder, enqueueErr error) *TurnIngress {
	t.Helper()
	recorder.subscription = &fake.TurnReplySubscription{RouteValue: "route:request-1"}
	return &TurnIngress{
		Limiter: &fake.TenantRateLimiter{AllowTurnFunc: func(context.Context, domain.TenantContext) error {
			recorder.calls = append(recorder.calls, "limit")
			return nil
		}},
		Records: &fake.TurnRecordStore{
			FindFunc: func(context.Context, int64, string) (int64, string, []byte, error) {
				recorder.calls = append(recorder.calls, "find")
				return 0, "", nil, domain.ErrNotFound
			},
			OpenFunc: func(context.Context, domain.TurnRequest, domain.ObjectRef) (int64, error) {
				recorder.calls = append(recorder.calls, "open")
				return 1, nil
			},
			FailFunc: func(_ context.Context, _ int64, _ string, status, errorCode string) error {
				recorder.calls = append(recorder.calls, "fail")
				recorder.failed = append(recorder.failed, status+":"+errorCode)
				return nil
			},
		},
		Objects:   &ObjectStateStore{Storage: &fake.ObjectStorage{}, Bucket: "turns", MaxBytes: 1 << 20},
		Estimator: &fake.TurnBudgetEstimator{},
		Budget: &fake.TenantBudgetManager{
			ReserveFunc: func(_ context.Context, tenantID int64, requestID string, tokens, cost int64, currency string) (domain.BudgetReservation, error) {
				recorder.calls = append(recorder.calls, "reserve")
				return domain.BudgetReservation{TenantID: tenantID, ReservationID: "reservation-1", RequestID: requestID, Attempt: 1, GrantedTokens: tokens, Currency: currency}, nil
			},
			CommitFunc: func(context.Context, domain.BudgetReservation, domain.Usage) error { return nil },
			ReleaseFunc: func(context.Context, domain.BudgetReservation) error {
				recorder.calls = append(recorder.calls, "release")
				recorder.released++
				return nil
			},
		},
		Replies: fake.TurnReplySubscriberFunc(func(context.Context, int64, string) (contract.TurnReplySubscription, error) {
			recorder.calls = append(recorder.calls, "subscribe")
			return recorder.subscription, nil
		}),
		Dispatcher: &fake.TurnDispatcher{EnqueueFunc: func(context.Context, domain.TurnDispatch) error {
			recorder.calls = append(recorder.calls, "enqueue")
			return enqueueErr
		}},
	}
}

func ingressRequest() domain.TurnRequest {
	return domain.TurnRequest{
		TenantContext:  domain.TenantContext{TenantID: 7},
		RequestID:      "request-1",
		ConversationID: "conversation-1",
		AgentID:        "agent",
		Input:          []byte("hello"),
	}
}

func TestTurnIngressAdmitsInOrder(t *testing.T) {
	recorder := &ingressRecorder{}
	ingress := newTestIngress(t, recorder, nil)
	subscription, err := ingress.OpenTurn(context.Background(), ingressRequest())
	if err != nil {
		t.Fatal(err)
	}
	if subscription.Route() != "route:request-1" {
		t.Fatalf("route = %q", subscription.Route())
	}
	want := []string{"limit", "find", "open", "reserve", "subscribe", "enqueue"}
	if len(recorder.calls) != len(want) {
		t.Fatalf("calls = %v", recorder.calls)
	}
	for i, call := range want {
		if recorder.calls[i] != call {
			t.Fatalf("calls = %v, want %v", recorder.calls, want)
		}
	}
}

func TestTurnIngressRefundsAndClosesOnDispatchFailure(t *testing.T) {
	recorder := &ingressRecorder{}
	dispatchErr := errors.New("queue unavailable")
	ingress := newTestIngress(t, recorder, dispatchErr)
	if _, err := ingress.OpenTurn(context.Background(), ingressRequest()); !errors.Is(err, dispatchErr) {
		t.Fatalf("error = %v", err)
	}
	if recorder.released != 1 || len(recorder.failed) != 1 || recorder.failed[0] != "failed:dispatch_failed" {
		t.Fatalf("released = %d, failed = %v", recorder.released, recorder.failed)
	}
	if !recorder.subscription.Closed {
		t.Fatal("subscription was left open after a failed dispatch")
	}
}

func TestTurnIngressResubscribesTerminalRequest(t *testing.T) {
	recorder := &ingressRecorder{}
	ingress := newTestIngress(t, recorder, nil)
	ingress.Records = &fake.TurnRecordStore{FindFunc: func(context.Context, int64, string) (int64, string, []byte, error) {
		recorder.calls = append(recorder.calls, "find")
		return 1, "completed", []byte("response"), nil
	}}
	if _, err := ingress.OpenTurn(context.Background(), ingressRequest()); err != nil {
		t.Fatal(err)
	}
	for _, call := range recorder.calls {
		if call == "reserve" || call == "enqueue" {
			t.Fatalf("terminal request re-admitted: %v", recorder.calls)
		}
	}
}

func TestTurnIngressRejectsRateLimitedTenant(t *testing.T) {
	recorder := &ingressRecorder{}
	ingress := newTestIngress(t, recorder, nil)
	ingress.Limiter = &fake.TenantRateLimiter{AllowTurnFunc: func(context.Context, domain.TenantContext) error {
		return domain.ErrRateLimited
	}}
	if _, err := ingress.OpenTurn(context.Background(), ingressRequest()); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("error = %v, want rate limited", err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("calls after rejection = %v", recorder.calls)
	}
}

func TestTurnIngressRequiresCompleteRequest(t *testing.T) {
	ingress := newTestIngress(t, &ingressRecorder{}, nil)
	request := ingressRequest()
	request.Input = nil
	if _, err := ingress.OpenTurn(context.Background(), request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}
