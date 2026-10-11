package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestReleaseRouterTakesItsCandidateLimit(t *testing.T) {
	f, router := newRouterFixture(t, paymentsRouting)
	for id := 100; id < 103; id++ {
		f.holders[controller] = append(f.holders[controller], person(fmt.Sprint(id)))
	}
	f.forbidden["100"], f.forbidden["101"] = domain.ErrForbidden, domain.ErrForbidden
	router.MaxCandidates = 2
	assignment, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{})
	if err != nil || assignment.Approver.ID != "" || f.queries[0].Limit != 2 {
		t.Fatalf("a candidate past the limit is ignored: %+v, %v, limit %d", assignment, err, f.queries[0].Limit)
	}
	router.MaxCandidates = 3
	if assignment, err = router.Route(context.Background(), newAsk(), domain.PrincipalRef{}); err != nil || assignment.Approver.ID != "102" {
		t.Fatalf("within the limit: %+v, %v", assignment, err)
	}
	router.MaxCandidates = -1
	if _, err = router.Route(context.Background(), newAsk(), domain.PrincipalRef{}); err == nil {
		t.Fatal("a negative candidate limit must be refused")
	}
	if _, err = router.Escalate(context.Background(), routedRequest(0, "", gateNow.Add(-1))); err == nil {
		t.Fatal("a negative candidate limit must refuse escalation too")
	}
}

func TestGateTakesItsAskLimit(t *testing.T) {
	ask := func(limit int) (*chainStore, *Gate, domain.ToolCall) {
		store := &chainStore{requests: map[int64]domain.ApprovalRequest{}}
		g := chainGate(store)
		routed := 0
		g.Router = fake.ApproverRouterFunc(routesBySource(&routed))
		g.MaxAsksPerCall = limit
		call := toolCall(`{"amount":1}`)
		if _, err := g.Decide(context.Background(), call, autonomyAsk); err != nil {
			t.Fatal(err)
		}
		store.set(stepIDAt(call, 0), domain.ApprovalStatusApproved)
		return store, g, call
	}
	_, g, call := ask(0)
	if decision, err := g.Decide(context.Background(), call, guardrailAsk); err != nil || decision != domain.ApprovalPending {
		t.Fatalf("zero takes %d: %q, %v", DefaultMaxAsksPerCall, decision, err)
	}
	store, g, call := ask(1)
	decision, err := g.Decide(context.Background(), call, guardrailAsk)
	if !errors.Is(err, domain.ErrConflict) || decision != domain.ApprovalDenied || !strings.Contains(err.Error(), "more than 1 approvals") || len(store.opened) != 1 {
		t.Fatalf("a second ask past a limit of 1: %q, %v, opened %d", decision, err, len(store.opened))
	}
	g.MaxAsksPerCall = -1
	if _, err := g.Decide(context.Background(), call, autonomyAsk); err == nil {
		t.Fatal("a negative ask limit must be refused")
	}
	if _, _, err := g.Revision(context.Background(), call); err == nil {
		t.Fatal("a negative ask limit must refuse reading a revision too")
	}
}

func TestResumerRefusesANegativeRevisionLimit(t *testing.T) {
	f := newRevisionFixture(t)
	f.resumer.MaxRevisedArgumentBytes = -1
	if _, err := f.edit(`{"amount":5}`); err == nil || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative limit is a composition error: %v", err)
	}
	f = newRevisionFixture(t)
	if _, err := f.edit(`{"note":"` + strings.Repeat("x", DefaultMaxRevisedArgumentBytes) + `"}`); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero takes %d bytes: %v", DefaultMaxRevisedArgumentBytes, err)
	}
	f = newRevisionFixture(t)
	f.resumer.MaxRevisedArgumentBytes = 1 << 20
	if _, err := f.edit(`{"note":"` + strings.Repeat("x", DefaultMaxRevisedArgumentBytes) + `"}`); err != nil {
		t.Fatalf("a raised limit admits it: %v", err)
	}
}
