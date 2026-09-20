package dataplane

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// hopLedger holds per principal, like BudgetLedger with a PrincipalBudgetPolicy.
type hopLedger struct {
	ceilings map[string]int64
	held     map[string]int64
}

func (ledger *hopLedger) manager() *fake.TenantBudgetManager {
	return &fake.TenantBudgetManager{
		ReserveFunc: func(_ context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
			who := request.Principal.ID
			if ceiling, bounded := ledger.ceilings[who]; bounded && ledger.held[who]+request.CostMinorUnits > ceiling {
				return domain.BudgetReservation{}, fmt.Errorf("%w: %s", domain.ErrBudgetExceeded, who)
			}
			ledger.held[who] += request.CostMinorUnits
			return domain.BudgetReservation{TenantID: request.TenantID, ReservationID: who, RequestID: request.RequestID, GrantedCostMinorUnits: request.CostMinorUnits, Currency: request.Currency}, nil
		},
		CommitFunc:  func(context.Context, domain.BudgetReservation, domain.Usage) error { return nil },
		ReleaseFunc: func(context.Context, domain.BudgetReservation) error { return nil },
	}
}

// hopIngress admits each hop through the real ingress; the delegate answers at once.
func hopIngress(t *testing.T, ledger *hopLedger, admitted *[]domain.TurnRequest) *TurnIngress {
	ingress := newTestIngress(t, &ingressRecorder{}, nil)
	ingress.Budget = ledger.manager()
	ingress.Estimator = &fake.TurnBudgetEstimator{EstimateFunc: func(context.Context, domain.TurnRequest) (domain.Usage, error) {
		return domain.Usage{InputTokens: 10, OutputTokens: 10, CostMinorUnits: 90, Currency: "USD"}, nil
	}}
	ingress.Replies = fake.TurnReplySubscriberFunc(func(context.Context, int64, string) (contract.TurnReplySubscription, error) {
		return &scriptedSubscription{frames: []domain.TurnReply{{Payload: []byte("answer")}, {Sequence: 1, Final: true}}}, nil
	})
	ingress.Dispatcher = &fake.TurnDispatcher{EnqueueFunc: func(_ context.Context, dispatch domain.TurnDispatch) error {
		*admitted = append(*admitted, dispatch.Turn)
		return nil
	}}
	return ingress
}

type scriptedSubscription struct{ frames []domain.TurnReply }

func (*scriptedSubscription) Route() string { return "route" }
func (*scriptedSubscription) Close() error  { return nil }
func (subscription *scriptedSubscription) Receive(context.Context) (domain.TurnReply, error) {
	frame := subscription.frames[0]
	subscription.frames = subscription.frames[1:]
	return frame, nil
}

// A chain whose budget runs out at hop two settles per hop: each delegate holds under its own
// principal, within what its delegator passed down, and nothing lands on the root delegator.
func TestDelegationChainSettlesPerHopAndStopsWhereItsBudgetEnds(t *testing.T) {
	ledger := &hopLedger{ceilings: map[string]int64{"researcher": 20}, held: map[string]int64{}}
	var admitted []domain.TurnRequest
	invoker := &TurnAgentInvoker{Ingress: hopIngress(t, ledger, &admitted)}
	lead := domain.Principal{Kind: domain.PrincipalAgent, ID: "lead", TenantID: 7}
	hop := func(caller domain.Principal, target string, budget int64) (domain.StepResult, error) {
		return invoker.Invoke(context.Background(), domain.DelegatedCall{
			Caller: caller, Target: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: target},
			Authority: domain.AuthorityHop{GrantID: "grant-" + target, Grantor: domain.PrincipalRef{Kind: caller.Kind, ID: caller.ID}},
			Bounds:    domain.DelegationBounds{RemainingDepth: 1, BudgetMinorUnits: budget, Currency: "USD"},
			WorkItem:  domain.WorkItem{ID: 3, Depth: 1}, Input: []byte("task"), RequestID: "turn:1:" + target,
		})
	}

	result, err := hop(lead, "specialist", 40)
	if err != nil || string(result.State) != "answer" || result.Usage != (domain.Usage{}) {
		t.Fatalf("hop one = %+v, %v; the delegate settles its own spend, so the step reports none", result, err)
	}
	specialist := admitted[0].Principal
	if specialist.ID != "specialist" || len(specialist.Authority) != 1 || specialist.Authority[0].Grantor.ID != "lead" || admitted[0].WorkItemID != 3 {
		t.Fatalf("the hop must run as the delegate under the delegator's authority: %+v", admitted[0])
	}
	if _, err = hop(specialist, "researcher", 30); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("hop two must stop at its own budget, got %v", err)
	}
	// The quote was 90: hop one held its 40 under the specialist, hop two held nothing, the lead nothing.
	if ledger.held["specialist"] != 40 || ledger.held["researcher"] != 0 || ledger.held["lead"] != 0 {
		t.Fatalf("holds = %v", ledger.held)
	}
}

func TestTurnAgentInvokerSurfacesTheDelegatesOutcome(t *testing.T) {
	outcome := func(frames ...domain.TurnReply) error {
		ingress := newTestIngress(t, &ingressRecorder{}, nil)
		ingress.Replies = fake.TurnReplySubscriberFunc(func(context.Context, int64, string) (contract.TurnReplySubscription, error) {
			return &scriptedSubscription{frames: frames}, nil
		})
		_, err := (&TurnAgentInvoker{Ingress: ingress}).Invoke(context.Background(), domain.DelegatedCall{
			Caller: domain.Principal{Kind: domain.PrincipalAgent, ID: "lead", TenantID: 7},
			Target: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "specialist"}, Input: []byte("task"), RequestID: "r",
		})
		return err
	}
	pending := domain.TurnReply{Final: true, ErrorCode: "internal", Events: []domain.TurnEvent{{Kind: domain.TurnEventApprovalPending}}}
	if err := outcome(pending); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatalf("a parked delegate parks its delegator, got %v", err)
	}
	if err := outcome(domain.TurnReply{Final: true, ErrorCode: "canceled"}); !errors.Is(err, domain.ErrTurnCanceled) {
		t.Fatalf("want ErrTurnCanceled, got %v", err)
	}
	if _, err := (&TurnAgentInvoker{Ingress: newTestIngress(t, &ingressRecorder{}, nil)}).Invoke(context.Background(), domain.DelegatedCall{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}
