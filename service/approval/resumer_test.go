package approval

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type requeueFunc func(context.Context, int64, string) error

func (function requeueFunc) Requeue(ctx context.Context, tenantID int64, requestID string) error {
	return function(ctx, tenantID, requestID)
}

func resumerFor(store *stubStore, resumed, failed, dispatched *int) *Resumer {
	return &Resumer{
		Store: store,
		Authorizer: fake.ApprovalAuthorizerFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) error {
			return nil
		}),
		Records: &fake.TurnRecordStore{
			ResumeFunc: func(context.Context, int64, string) error { *resumed++; return nil },
			FailFunc:   func(context.Context, int64, string, domain.TurnStatus, string) error { *failed++; return nil },
		},
		Queue: requeueFunc(func(context.Context, int64, string) error { *dispatched++; return nil }),
	}
}

func openRequest(t *testing.T) (*stubStore, domain.ApprovalVerdict) {
	t.Helper()
	store := &stubStore{}
	call := toolCall(`{"amount":1}`)
	if _, err := gate(store).Decide(context.Background(), call, autonomyAsk); err != nil {
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
	if _, err := resumerFor(store, &resumed, &failed, &dispatched).Resolve(context.Background(), verdict); err != nil {
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
	if _, err := resumerFor(store, &resumed, &failed, &dispatched).Resolve(context.Background(), verdict); err != nil {
		t.Fatal(err)
	}
	if failed != 1 || resumed != 0 || dispatched != 0 {
		t.Fatalf("failed = %d, resumed = %d, dispatched = %d, want a rejected turn stopped", failed, resumed, dispatched)
	}
}

func TestResolveReplaysARecordedRejection(t *testing.T) {
	store, verdict := openRequest(t)
	verdict.Status = domain.ApprovalStatusRejected
	records := &fake.TurnRecordStore{
		FailFunc: func(context.Context, int64, string, domain.TurnStatus, string) error { return domain.ErrConflict },
		FindFunc: func(context.Context, int64, string) (int64, domain.TurnStatus, []byte, error) {
			return 1, domain.TurnFailed, []byte(domain.ApprovalStatusRejected), nil
		},
	}
	resumer := resumerFor(store, new(int), new(int), new(int))
	resumer.Records = records
	if _, err := resumer.Resolve(context.Background(), verdict); err != nil {
		t.Fatalf("replayed rejection = %v", err)
	}
	records.FindFunc = func(context.Context, int64, string) (int64, domain.TurnStatus, []byte, error) {
		return 1, domain.TurnFailed, []byte(domain.ApprovalStatusEdited), nil
	}
	if _, err := resumer.Resolve(context.Background(), verdict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("different terminal result = %v, want conflict", err)
	}
}
