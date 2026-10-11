package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type stubStore struct {
	stored   domain.ApprovalRequest
	opened   int
	resolved domain.ApprovalVerdict
	openErr  error
}

func (s *stubStore) Open(_ context.Context, request domain.ApprovalRequest) (domain.ApprovalRequest, error) {
	s.opened++
	if s.openErr != nil {
		return domain.ApprovalRequest{}, s.openErr
	}
	if s.stored.ProposedDigest == "" {
		s.stored = request
		s.stored.Status = domain.ApprovalStatusPending
	}
	return s.stored, nil
}

func (s *stubStore) Get(context.Context, domain.ApprovalKey) (domain.ApprovalRequest, error) {
	return s.stored, nil
}

func (s *stubStore) Resolve(_ context.Context, verdict domain.ApprovalVerdict) (domain.ApprovalRequest, error) {
	if verdict.ProposedDigest != s.stored.ProposedDigest {
		return domain.ApprovalRequest{}, domain.ErrConflict
	}
	s.resolved = verdict
	s.stored.Status = verdict.Status
	return s.stored, nil
}

func (s *stubStore) Revision(context.Context, domain.ApprovalKey) (domain.ProposalRevision, error) {
	return domain.ProposalRevision{}, domain.ErrNotFound
}

func toolCall(arguments string) domain.ToolCall {
	return domain.ToolCall{
		TenantContext: domain.TenantContext{TenantID: 7},
		Principal:     domain.Principal{Kind: domain.PrincipalAgent, ID: "agent", TenantID: 7, Release: "3"},
		RequestID:     "request", ConversationID: "conversation",
		ToolID: "wire", ToolVersion: "v1", Arguments: []byte(arguments),
	}
}

func gate(store *stubStore) *Gate {
	return &Gate{Store: store, Deadline: time.Hour, Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }}
}

func TestGateOpensOncePerProposalAndReturnsPending(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	for i := 0; i < 2; i++ {
		decision, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), "release.approve")
		if err != nil || decision != domain.ApprovalPending {
			t.Fatalf("decision = %q, error = %v", decision, err)
		}
	}
	// Open is idempotent, so a replayed turn re-attaches instead of asking twice.
	if store.stored.Status != domain.ApprovalStatusPending || store.opened != 2 {
		t.Fatalf("stored = %+v, opened = %d", store.stored, store.opened)
	}
}

func TestGateReturnsTheRecordedVerdict(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	call := toolCall(`{"amount":1}`)
	if _, err := g.Decide(context.Background(), call, "release.approve"); err != nil {
		t.Fatal(err)
	}
	for status, want := range map[domain.ApprovalStatus]domain.ApprovalDecision{
		domain.ApprovalStatusApproved: domain.ApprovalApproved,
		domain.ApprovalStatusRejected: domain.ApprovalDenied,
		domain.ApprovalStatusExpired:  domain.ApprovalDenied,
	} {
		store.stored.Status = status
		decision, err := g.Decide(context.Background(), call, "release.approve")
		if err != nil || decision != want {
			t.Fatalf("%s: decision = %q, error = %v, want %q", status, decision, err, want)
		}
	}
	// An edited proposal never runs as proposed; asking to run it ends the turn.
	store.stored.Status = domain.ApprovalStatusEdited
	if decision, err := g.Decide(context.Background(), call, "release.approve"); decision != domain.ApprovalDenied || !errors.Is(err, domain.ErrProposalRevised) {
		t.Fatalf("edited: decision = %q, error = %v, want denied with ErrProposalRevised", decision, err)
	}
}

func TestProposalDigestBindsEveryArgument(t *testing.T) {
	if ProposalDigest(toolCall(`{"amount":1}`)) == ProposalDigest(toolCall(`{"amount":1000}`)) {
		t.Fatal("a changed argument must produce a different proposal digest")
	}
}

func TestResolveRejectsAVerdictForAChangedProposal(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	if _, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), "release.approve"); err != nil {
		t.Fatal(err)
	}
	_, err := store.Resolve(context.Background(), domain.ApprovalVerdict{
		Status: domain.ApprovalStatusApproved, ProposedDigest: ProposalDigest(toolCall(`{"amount":1000}`)),
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want approving a changed action to fail", err)
	}
}

func TestEscalationRoutesToABackupThenAbandons(t *testing.T) {
	backup := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}
	policy := &BackupEscalation{
		Backups: map[domain.RiskTier]domain.PrincipalRef{domain.RiskHigh: backup}, Extension: time.Hour,
		Authorizer: fake.ApprovalAuthorizerFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) error { return nil }),
	}
	step, err := policy.Escalate(context.Background(), domain.ApprovalRequest{RiskTier: domain.RiskHigh})
	if err != nil || step.Backup != backup || step.Extension != time.Hour {
		t.Fatalf("step = %+v, error = %v", step, err)
	}
	// No backup for the tier means abandon, never a silent widening.
	none, err := policy.Escalate(context.Background(), domain.ApprovalRequest{RiskTier: domain.RiskCritical})
	if err != nil || none.Backup.ID != "" {
		t.Fatalf("step = %+v, error = %v, want abandonment", none, err)
	}
}

func TestEscalationDoesNotReassignToTheCurrentApprover(t *testing.T) {
	approver := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}
	policy := &BackupEscalation{
		Backups:    map[domain.RiskTier]domain.PrincipalRef{domain.RiskHigh: approver},
		Authorizer: fake.ApprovalAuthorizerFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) error { return nil }),
	}
	step, err := policy.Escalate(context.Background(), domain.ApprovalRequest{RiskTier: domain.RiskHigh, Approver: approver})
	if err != nil || step.Backup.ID != "" {
		t.Fatalf("step = %+v, want no self-escalation", step)
	}
}

type routerFunc func(context.Context, domain.ApprovalRequest) (domain.PrincipalRef, error)

func (function routerFunc) Route(ctx context.Context, request domain.ApprovalRequest) (domain.PrincipalRef, error) {
	return function(ctx, request)
}

func TestGateRecordsTheRoutedApproverAndRefusesAnUnroutableRequest(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	g.Router = routerFunc(func(_ context.Context, request domain.ApprovalRequest) (domain.PrincipalRef, error) {
		if request.RuleID != "autonomy" {
			t.Fatalf("rule = %q", request.RuleID)
		}
		return domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"}, nil
	})
	if decision, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), "autonomy"); err != nil || decision != domain.ApprovalPending || store.stored.Approver.ID != "77" {
		t.Fatalf("decision = %q, error = %v, approver = %+v", decision, err, store.stored.Approver)
	}
	unrouted := gate(&stubStore{})
	unrouted.Router = routerFunc(func(context.Context, domain.ApprovalRequest) (domain.PrincipalRef, error) {
		return domain.PrincipalRef{}, nil
	})
	if decision, err := unrouted.Decide(context.Background(), toolCall(`{"amount":1}`), "autonomy"); !errors.Is(err, domain.ErrForbidden) || decision != domain.ApprovalDenied {
		t.Fatalf("unrouted = %q, %v", decision, err)
	}
}
