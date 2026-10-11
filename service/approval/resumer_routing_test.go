package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/keel/actiontoken"
	"github.com/nauticana/keel/idempotency"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// tableResumer decides requests in the table-backed store with an authorizer allowing everyone.
func tableResumer(store *TableStore, resumed *int) *Resumer {
	return &Resumer{
		Store: store, Inbox: store, Now: func() time.Time { return gateNow },
		Authorizer: fake.ApprovalAuthorizerFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) error { return nil }),
		Records: &fake.TurnRecordStore{
			ResumeFunc: func(context.Context, int64, string) error { *resumed++; return nil },
			FailFunc:   func(context.Context, int64, string, domain.TurnStatus, string) error { return nil },
		},
		Queue: requeueFunc(func(context.Context, int64, string) error { return nil }),
	}
}

func TestAVacantRoutedRequestCannotBeDecided(t *testing.T) {
	_, store := newApprovalStore(false)
	ctx := context.Background()
	request, err := store.Open(ctx, routedOwed("turn-1", "", 0))
	if err != nil {
		t.Fatal(err)
	}
	resumed := 0
	resumer := tableResumer(store, &resumed)
	key := domain.ApprovalKey{TenantID: 7, RequestID: "turn-1", ExecutionStepID: 3}
	_, err = resumer.Resolve(ctx, domain.ApprovalVerdict{
		RequestKey: key, Status: domain.ApprovalStatusApproved, Decider: person("42").Principal, ProposedDigest: request.ProposedDigest,
	})
	if !errors.Is(err, domain.ErrForbidden) || resumed != 0 {
		t.Fatalf("a DECIDE holder decided a vacancy: %v, resumed %d", err, resumed)
	}
	resumer.Links = &actiontoken.Service{DB: &tokenDB{rows: map[string][][]any{}}, Ledger: &idempotency.MemoryLedger{}}
	resumer.LinkMaxRisk = domain.RiskCritical
	if _, err := resumer.MintLink(ctx, key, 42, domain.ApprovalStatusApproved, time.Hour); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a link for a vacancy: %v", err)
	}
	if _, err := resumer.RedeemLink(ctx, linkToken, key, domain.ApprovalStatusApproved, ""); err == nil {
		t.Fatal("a link must not decide a vacancy")
	}
	if stored, _ := store.Get(ctx, key); stored.Status != domain.ApprovalStatusPending {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestAStaleApproversVerdictLosesToAReassignment(t *testing.T) {
	_, store := newApprovalStore(false)
	ctx := context.Background()
	request, err := store.Open(ctx, routedOwed("turn-1", "77", 0))
	if err != nil {
		t.Fatal(err)
	}
	resumed := 0
	resumer := tableResumer(store, &resumed)
	// The reassignment lands between the decider's read and its verdict.
	resumer.Authorizer = fake.ApprovalAuthorizerFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) error {
		return store.Reassign(ctx, request, domain.EscalationStep{Backup: person("88").Principal, Extension: time.Hour, Route: routedAuthority(1)})
	})
	_, err = resumer.Resolve(ctx, domain.ApprovalVerdict{
		RequestKey:     domain.ApprovalKey{TenantID: 7, RequestID: "turn-1", ExecutionStepID: 3},
		Status:         domain.ApprovalStatusApproved,
		Decider:        person("77").Principal,
		ProposedDigest: request.ProposedDigest,
		// A client cannot choose the fence; the server sets it from the request it read.
		Assignment: domain.ApprovalFence{Step: 1, Approver: person("88").Principal},
	})
	if !errors.Is(err, domain.ErrConflict) || resumed != 0 {
		t.Fatalf("stale verdict: %v, resumed %d", err, resumed)
	}
}

func TestRoleAuthorizerRefusesAVacantRoutedRequest(t *testing.T) {
	err := approvalAuthorizer(true).AuthorizeApproval(context.Background(), routedOwed("turn-1", "", 0), person("42").Principal)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want a vacancy refused to every DECIDE holder", err)
	}
	if err := approvalAuthorizer(true).AuthorizeApproval(context.Background(), owed("turn-1", ""), person("42").Principal); err != nil {
		t.Fatalf("an unrouted request still goes to whoever holds DECIDE: %v", err)
	}
}

func TestPendingLeavesOutVacantRoutedRequests(t *testing.T) {
	resumer := &Resumer{
		Inbox: &fake.ApprovalInbox{PendingFunc: func(context.Context, domain.ApprovalFilter) ([]domain.ApprovalRequest, error) {
			return []domain.ApprovalRequest{routedOwed("vacant", "", 0), owed("scope", "")}, nil
		}},
		Authorizer: approvalAuthorizer(true),
	}
	requests, err := resumer.Pending(context.Background(), 7, person("42").Principal, 10)
	if err != nil || len(requests) != 1 || requests[0].RequestID != "scope" {
		t.Fatalf("requests = %+v, %v", requests, err)
	}
}
