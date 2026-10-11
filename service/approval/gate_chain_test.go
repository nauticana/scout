package approval

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// chainStore keeps every request of a call by its step key.
type chainStore struct {
	requests map[int64]domain.ApprovalRequest
	opened   []int64
}

func (s *chainStore) Open(_ context.Context, request domain.ApprovalRequest) (domain.ApprovalRequest, error) {
	if stored, ok := s.requests[request.ExecutionStepID]; ok {
		return stored, nil
	}
	request.Status = domain.ApprovalStatusPending
	s.requests[request.ExecutionStepID] = request
	s.opened = append(s.opened, request.ExecutionStepID)
	return request, nil
}

func (s *chainStore) Get(_ context.Context, key domain.ApprovalKey) (domain.ApprovalRequest, error) {
	if stored, ok := s.requests[key.ExecutionStepID]; ok {
		return stored, nil
	}
	return domain.ApprovalRequest{}, domain.ErrNotFound
}

func (s *chainStore) Resolve(context.Context, domain.ApprovalVerdict) (domain.ApprovalRequest, error) {
	return domain.ApprovalRequest{}, errors.New("unused")
}

func (s *chainStore) Revision(context.Context, domain.ApprovalKey) (domain.ProposalRevision, error) {
	return domain.ProposalRevision{}, domain.ErrNotFound
}

func (s *chainStore) set(step int64, status domain.ApprovalStatus) {
	request := s.requests[step]
	request.Status = status
	s.requests[step] = request
}

var guardrailAsk = domain.ApprovalAsk{Source: domain.ApprovalSourceGuardrail, RuleID: "irreversible"}

// routesBySource sends autonomy and policy asks to role clerk and guardrail asks to position cfo.
func routesBySource(routed *int) func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error) {
	return func(_ context.Context, request domain.ApprovalRequest, _ domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		*routed++
		if request.Source == domain.ApprovalSourceGuardrail {
			return domain.ApprovalAssignment{
				Approver:  domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "90"},
				Authority: domain.ApprovalRouteAuthority{RouteID: "irrev", Reference: domain.ApproverReference{Kind: domain.ApproverPosition, Ref: "cfo"}},
			}, nil
		}
		return domain.ApprovalAssignment{
			Approver:  domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"},
			Authority: domain.ApprovalRouteAuthority{RouteID: "ops", Reference: domain.ApproverReference{Kind: domain.ApproverRole, Ref: "clerk"}},
		}, nil
	}
}

func chainGate(store *chainStore) *Gate {
	return &Gate{Store: store, Deadline: time.Hour, Now: func() time.Time { return gateNow }}
}

func TestAnApprovalUnderAnotherRouteNeverDecidesALaterAsk(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	routed, notified := 0, 0
	g.Router = fake.ApproverRouterFunc(routesBySource(&routed))
	g.Notifier = fake.NotifierFunc(func(context.Context, domain.Notification) error { notified++; return nil })
	call := toolCall(`{"amount":1}`)
	if decision, err := g.Decide(context.Background(), call, autonomyAsk); err != nil || decision != domain.ApprovalPending {
		t.Fatalf("autonomy = %q, %v", decision, err)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
	decision, err := g.Decide(context.Background(), call, guardrailAsk)
	if err != nil || decision != domain.ApprovalPending || len(store.opened) != 2 || notified != 2 {
		t.Fatalf("guardrail = %q, %v, opened %d, notified %d", decision, err, len(store.opened), notified)
	}
	second := store.requests[stepIDAt(call, 1)]
	if second.Source != domain.ApprovalSourceGuardrail || second.Route.RouteID != "irrev" || second.Approver.ID != "90" {
		t.Fatalf("second request = %+v, want the guardrail ask under its own route", second)
	}
	store.set(stepIDAt(call, 1), domain.ApprovalStatusApproved)
	for _, ask := range []domain.ApprovalAsk{autonomyAsk, guardrailAsk} {
		if decision, err := g.Decide(context.Background(), call, ask); err != nil || decision != domain.ApprovalApproved {
			t.Fatalf("replay of %s = %q, %v", ask.Source, decision, err)
		}
	}
	if len(store.opened) != 2 || notified != 2 {
		t.Fatalf("a replay opened %d or notified %d", len(store.opened), notified)
	}
}

func TestALowerTierApprovalNeverDecidesARiskierAsk(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	routed := 0
	g.Router = fake.ApproverRouterFunc(routesBySource(&routed))
	call := toolCall(`{"amount":1}`)
	low := domain.ApprovalAsk{Source: domain.ApprovalSourcePolicy, RuleID: "policy", RiskTier: domain.RiskLow}
	if _, err := g.Decide(context.Background(), call, low); err != nil {
		t.Fatal(err)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
	// The mode's ask shares the route but asks at high risk.
	if decision, err := g.Decide(context.Background(), call, autonomyAsk); err != nil || decision != domain.ApprovalPending {
		t.Fatalf("autonomy = %q, %v", decision, err)
	}
	if second := store.requests[stepIDAt(call, 1)]; second.RiskTier != domain.RiskHigh || second.Source != domain.ApprovalSourceAutonomy {
		t.Fatalf("second request = %+v", second)
	}
}

func TestAnApprovalCoversAnAskOfTheSameRouteAndNoHigherRisk(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	routed, notified := 0, 0
	g.Router = fake.ApproverRouterFunc(routesBySource(&routed))
	g.Notifier = fake.NotifierFunc(func(context.Context, domain.Notification) error { notified++; return nil })
	call := toolCall(`{"amount":1}`)
	if _, err := g.Decide(context.Background(), call, policyAsk); err != nil {
		t.Fatal(err)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
	if decision, err := g.Decide(context.Background(), call, autonomyAsk); err != nil || decision != domain.ApprovalApproved {
		t.Fatalf("autonomy = %q, %v", decision, err)
	}
	if len(store.opened) != 1 || notified != 1 || routed != 2 {
		t.Fatalf("opened %d, notified %d, routed %d", len(store.opened), notified, routed)
	}
}

func TestCoverageWithoutARouterFollowsRiskAlone(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	call := toolCall(`{"amount":1}`)
	if _, err := g.Decide(context.Background(), call, autonomyAsk); err != nil {
		t.Fatal(err)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
	if decision, err := g.Decide(context.Background(), call, guardrailAsk); err != nil || decision != domain.ApprovalApproved {
		t.Fatalf("an equal-tier ask decided by scope = %q, %v", decision, err)
	}
	critical := domain.ApprovalAsk{Source: domain.ApprovalSourcePolicy, RuleID: "policy", RiskTier: domain.RiskCritical}
	if decision, err := g.Decide(context.Background(), call, critical); err != nil || decision != domain.ApprovalPending || len(store.opened) != 2 {
		t.Fatalf("a riskier ask = %q, %v, opened %d", decision, err, len(store.opened))
	}
}

func TestAPendingOrRefusedRequestHoldsEveryLaterAsk(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	routed := 0
	g.Router = fake.ApproverRouterFunc(routesBySource(&routed))
	call := toolCall(`{"amount":1}`)
	if _, err := g.Decide(context.Background(), call, autonomyAsk); err != nil {
		t.Fatal(err)
	}
	if decision, err := g.Decide(context.Background(), call, guardrailAsk); err != nil || decision != domain.ApprovalPending || routed != 1 {
		t.Fatalf("pending = %q, %v, routed %d", decision, err, routed)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusRejected)
	if decision, err := g.Decide(context.Background(), call, guardrailAsk); err != nil || decision != domain.ApprovalDenied || len(store.opened) != 1 {
		t.Fatalf("rejected = %q, %v, opened %d", decision, err, len(store.opened))
	}
}

func TestACoverageCheckFailsClosedWhenRoutingFails(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	routed := 0
	g.Router = fake.ApproverRouterFunc(routesBySource(&routed))
	call := toolCall(`{"amount":1}`)
	if _, err := g.Decide(context.Background(), call, autonomyAsk); err != nil {
		t.Fatal(err)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
	g.Router = fake.ApproverRouterFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		return domain.ApprovalAssignment{}, domain.ErrForbidden
	})
	if decision, err := g.Decide(context.Background(), call, guardrailAsk); !errors.Is(err, domain.ErrForbidden) || decision != domain.ApprovalDenied || len(store.opened) != 1 {
		t.Fatalf("unroutable = %q, %v, opened %d", decision, err, len(store.opened))
	}
}

func TestARevisionIsFoundPastTheApprovedRequestsOfACall(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	routed := 0
	g.Router = fake.ApproverRouterFunc(routesBySource(&routed))
	call := toolCall(`{"amount":1}`)
	if _, err := g.Decide(context.Background(), call, autonomyAsk); err != nil {
		t.Fatal(err)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
	if _, err := g.Decide(context.Background(), call, guardrailAsk); err != nil {
		t.Fatal(err)
	}
	if _, _, edited, err := g.edited(context.Background(), call); err != nil || edited {
		t.Fatalf("nothing edited yet: %t, %v", edited, err)
	}
	store.set(stepIDAt(call, 1), domain.ApprovalStatusEdited)
	request, key, edited, err := g.edited(context.Background(), call)
	if err != nil || !edited || key.ExecutionStepID != stepIDAt(call, 1) || request.Source != domain.ApprovalSourceGuardrail {
		t.Fatalf("edited = %t, key %+v, %v", edited, key, err)
	}
	if decision, err := g.Decide(context.Background(), call, guardrailAsk); decision != domain.ApprovalDenied || !errors.Is(err, domain.ErrProposalRevised) {
		t.Fatalf("the edited original = %q, %v", decision, err)
	}
}

func TestStepKeysNameEveryRequestOfACallByItsIndex(t *testing.T) {
	call := toolCall(`{}`)
	call.IdempotencyKey = "call-1"
	for index := range 2 {
		sum := sha256.Sum256(fmt.Appendf(nil, "wire\x1fv1\x1fcall-1\x1fask:%d", index))
		var want int64
		for _, b := range sum[:7] {
			want = want<<8 | int64(b)
		}
		if stepIDAt(call, index) != want+1 {
			t.Fatalf("request %d of a call is keyed by its identity and index", index)
		}
	}
	if stepIDAt(call, 0) == stepIDAt(call, 1) || stepIDAt(call, 1) <= 0 {
		t.Fatal("each request of a call needs its own positive key")
	}
}

func TestTheSameAskAtAHigherRiskIsAskedAgain(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	call := toolCall(`{"amount":1}`)
	medium := domain.ApprovalAsk{Source: domain.ApprovalSourcePolicy, RuleID: "policy", RiskTier: domain.RiskMedium}
	if _, err := g.Decide(context.Background(), call, medium); err != nil {
		t.Fatal(err)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
	if decision, err := g.Decide(context.Background(), call, policyAsk); err != nil || decision != domain.ApprovalPending || len(store.opened) != 2 {
		t.Fatalf("critical after medium = %q, %v, opened %d", decision, err, len(store.opened))
	}
	if decision, err := g.Decide(context.Background(), call, medium); err != nil || decision != domain.ApprovalApproved {
		t.Fatalf("the medium ask replayed = %q, %v", decision, err)
	}
}

func TestTheSameRuleUnderAnotherSourceIsAnotherAsk(t *testing.T) {
	store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
	g := chainGate(store)
	routed := 0
	g.Router = fake.ApproverRouterFunc(routesBySource(&routed))
	call := toolCall(`{"amount":1}`)
	shared := domain.ApprovalAsk{Source: domain.ApprovalSourceAutonomy, RuleID: "irreversible"}
	if _, err := g.Decide(context.Background(), call, shared); err != nil {
		t.Fatal(err)
	}
	store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
	shared.Source = domain.ApprovalSourceGuardrail
	if decision, err := g.Decide(context.Background(), call, shared); err != nil || decision != domain.ApprovalPending || len(store.opened) != 2 {
		t.Fatalf("a guardrail ask under the autonomy ask's rule = %q, %v, opened %d", decision, err, len(store.opened))
	}
}
