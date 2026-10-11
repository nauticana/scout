package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
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
		Roles: fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
			return domain.AuthorizationGrant{Allowed: true}, nil
		}),
		Principals: fake.PrincipalResolverFunc(func(_ context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
			return domain.Principal{Kind: ref.Kind, ID: ref.ID, TenantID: tenantID, Release: "v1"}, nil
		}),
		Limiter: &fake.TenantRateLimiter{AllowTurnFunc: func(context.Context, domain.TenantContext) error {
			recorder.calls = append(recorder.calls, "limit")
			return nil
		}},
		Records: &fake.TurnRecordStore{
			FindFunc: func(context.Context, int64, string) (int64, domain.TurnStatus, []byte, error) {
				recorder.calls = append(recorder.calls, "find")
				return 0, "", nil, domain.ErrNotFound
			},
			OpenFunc: func(context.Context, domain.TurnRequest, domain.ObjectRef) (int64, error) {
				recorder.calls = append(recorder.calls, "open")
				return 1, nil
			},
			FailFunc: func(_ context.Context, _ int64, _ string, status domain.TurnStatus, errorCode string) error {
				recorder.calls = append(recorder.calls, "fail")
				recorder.failed = append(recorder.failed, string(status)+":"+errorCode)
				return nil
			},
		},
		Objects:   &ObjectStateStore{Storage: &fake.ObjectStorage{Name: "turns"}, MaxBytes: 1 << 20},
		Estimator: &fake.TurnBudgetEstimator{},
		Budget: &fake.TenantBudgetManager{
			ReserveFunc: func(_ context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
				tenantID, requestID, tokens, currency := request.TenantID, request.RequestID, request.Tokens, request.Currency
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
	if recorder.released != 1 || len(recorder.failed) != 1 || recorder.failed[0] != string(domain.TurnFailed)+":dispatch_failed" {
		t.Fatalf("released = %d, failed = %v", recorder.released, recorder.failed)
	}
	if !recorder.subscription.Closed {
		t.Fatal("subscription was left open after a failed dispatch")
	}
}

func TestTurnIngressResubscribesTerminalRequest(t *testing.T) {
	recorder := &ingressRecorder{}
	ingress := newTestIngress(t, recorder, nil)
	ingress.Records = &fake.TurnRecordStore{FindFunc: func(context.Context, int64, string) (int64, domain.TurnStatus, []byte, error) {
		recorder.calls = append(recorder.calls, "find")
		return 1, domain.TurnCompleted, []byte("response"), nil
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

func TestTurnIngressResolvesTheAliasAndAuthorizesThePerson(t *testing.T) {
	recorder := &ingressRecorder{}
	ingress := newTestIngress(t, recorder, nil)
	ingress.Records.(*fake.TurnRecordStore).AddressFunc = func(_ context.Context, _ int64, aliasID, agentID, _ string) (string, error) {
		if aliasID != "writer" || agentID != "" {
			t.Fatalf("address(%q, %q)", aliasID, agentID)
		}
		return "writer-b", nil
	}
	var checked []string
	ingress.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, principal domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
		checked = append(checked, principal.ID+" "+object+"/"+action+"/"+value)
		return domain.AuthorizationGrant{Allowed: true}, nil
	})
	var opened domain.TurnRequest
	ingress.Records.(*fake.TurnRecordStore).OpenFunc = func(_ context.Context, request domain.TurnRequest, _ domain.ObjectRef) (int64, error) {
		opened = request
		return 1, nil
	}
	request := ingressRequest()
	request.AliasID, request.AgentID = "writer", ""
	request.OnBehalfOf = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}
	if _, err := ingress.OpenTurn(context.Background(), request); err != nil {
		t.Fatalf("OpenTurn: %v", err)
	}
	if opened.AgentID != "writer-b" || opened.Principal.ID != "writer-b" || opened.Principal.Kind != domain.PrincipalAgent {
		t.Fatalf("opened = %+v", opened)
	}
	if len(checked) != 1 || checked[0] != "42 AGENT_CONVERSATION/USE/agent:writer-b" {
		t.Fatalf("checked = %v", checked)
	}
}

func TestTurnIngressRefusesUnauthorizedAndUngrantedPins(t *testing.T) {
	recorder := &ingressRecorder{}
	ingress := newTestIngress(t, recorder, nil)
	ingress.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, _, action, _ string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{Allowed: action == ConversationActionUse}, nil
	})
	request := ingressRequest()
	request.OnBehalfOf = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}
	request.Release, request.ReleaseReason = "3", "audit replay"
	if _, err := ingress.OpenTurn(context.Background(), request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("pin error = %v, want forbidden", err)
	}
	request.ReleaseReason = ""
	if _, err := ingress.OpenTurn(context.Background(), request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("reasonless pin = %v, want validation", err)
	}
	request = ingressRequest()
	request.Release, request.ReleaseReason = "3", "no person"
	if _, err := ingress.OpenTurn(context.Background(), request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("pin without a person = %v, want forbidden", err)
	}
	request = ingressRequest()
	request.Principal = domain.Principal{Kind: domain.PrincipalAgent, ID: "other", TenantID: 7}
	if _, err := ingress.OpenTurn(context.Background(), request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("mismatched principal = %v, want forbidden", err)
	}
	for _, call := range recorder.calls {
		if call == "open" {
			t.Fatal("a refused turn must not be opened")
		}
	}
}

func TestTurnIngressSelectableFiltersByGrant(t *testing.T) {
	ingress := newTestIngress(t, &ingressRecorder{}, nil)
	ingress.Records.(*fake.TurnRecordStore).SelectableFunc = func(context.Context, int64, string) ([]domain.SelectableAgent, error) {
		return []domain.SelectableAgent{{AgentID: "writer-a", Default: true}, {AgentID: "writer-b"}}, nil
	}
	ingress.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, _, _, value string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{Allowed: value == "agent:writer-b"}, nil
	})
	agents, err := ingress.Selectable(context.Background(), domain.TenantContext{TenantID: 7}, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, "writer")
	if err != nil || len(agents) != 1 || agents[0].AgentID != "writer-b" {
		t.Fatalf("agents = %+v, error = %v", agents, err)
	}
}

func TestTurnIngressRefusesOversizedReferences(t *testing.T) {
	ingress := newTestIngress(t, &ingressRecorder{}, nil)
	request := ingressRequest()
	request.WorkRef = strings.Repeat("w", 201)
	if _, err := ingress.OpenTurn(context.Background(), request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}
