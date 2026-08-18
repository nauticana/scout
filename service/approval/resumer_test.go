package approval

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func resumerFor(store *stubStore, resumed, failed, dispatched *int) *Resumer {
	return &Resumer{
		Store: store,
		Authorizer: fake.ApprovalAuthorizerFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) error {
			return nil
		}),
		Records: &fake.TurnRecordStore{
			ResumeFunc: func(context.Context, int64, string) error { *resumed++; return nil },
			FailFunc:   func(context.Context, int64, string, string, string) error { *failed++; return nil },
		},
		Dispatcher: &fake.TurnDispatcher{EnqueueFunc: func(context.Context, domain.TurnDispatch) error { *dispatched++; return nil }},
	}
}

func openRequest(t *testing.T) (*stubStore, domain.ApprovalVerdict) {
	t.Helper()
	store := &stubStore{}
	call := toolCall(`{"amount":1}`)
	if _, err := gate(store).Decide(context.Background(), call, "release.approve"); err != nil {
		t.Fatal(err)
	}
	return store, domain.ApprovalVerdict{
		RequestKey:     domain.ApprovalKey{TenantID: 7, RequestID: "request", ExecutionStepID: store.stored.ExecutionStepID},
		Decider:        domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"},
		ProposedDigest: ProposalDigest(call),
	}
}

func TestResolveResumesAndRedispatchesAnApprovedTurn(t *testing.T) {
	store, verdict := openRequest(t)
	verdict.Status = domain.ApprovalStatusApproved
	var resumed, failed, dispatched int
	if _, err := resumerFor(store, &resumed, &failed, &dispatched).Resolve(context.Background(), verdict, domain.TurnDispatch{}); err != nil {
		t.Fatal(err)
	}
	// Resuming without re-dispatching would leave the turn parked forever.
	if resumed != 1 || dispatched != 1 || failed != 0 {
		t.Fatalf("resumed = %d, dispatched = %d, failed = %d", resumed, dispatched, failed)
	}
}

func TestResolveFailsTheTurnOnARejection(t *testing.T) {
	store, verdict := openRequest(t)
	verdict.Status = domain.ApprovalStatusRejected
	var resumed, failed, dispatched int
	if _, err := resumerFor(store, &resumed, &failed, &dispatched).Resolve(context.Background(), verdict, domain.TurnDispatch{}); err != nil {
		t.Fatal(err)
	}
	if failed != 1 || resumed != 0 || dispatched != 0 {
		t.Fatalf("failed = %d, resumed = %d, dispatched = %d, want a rejected turn stopped", failed, resumed, dispatched)
	}
}
