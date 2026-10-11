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
	gets     int
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
	s.gets++
	if s.stored.ProposedDigest == "" {
		return domain.ApprovalRequest{}, domain.ErrNotFound
	}
	return s.stored, nil
}

func (s *stubStore) Resolve(_ context.Context, verdict domain.ApprovalVerdict) (domain.ApprovalRequest, error) {
	if verdict.ProposedDigest != s.stored.ProposedDigest || verdict.Assignment != fenceOf(s.stored) {
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
		OnBehalfOf:    domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "5"},
		RequestID:     "request", ConversationID: "conversation",
		ToolID: "wire", ToolVersion: "v1", Arguments: []byte(arguments),
	}
}

var (
	gateNow     = time.Unix(1700000000, 0).UTC()
	policyAsk   = domain.ApprovalAsk{Source: domain.ApprovalSourcePolicy, RuleID: "policy", RiskTier: domain.RiskCritical}
	autonomyAsk = domain.ApprovalAsk{Source: domain.ApprovalSourceAutonomy, RuleID: "autonomy"}
)

func gate(store *stubStore) *Gate {
	return &Gate{Store: store, Deadline: time.Hour, Now: func() time.Time { return gateNow }}
}

func TestGateOpensOncePerProposalAndReturnsPending(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	for i := 0; i < 2; i++ {
		decision, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), autonomyAsk)
		if err != nil || decision != domain.ApprovalPending {
			t.Fatalf("decision = %q, error = %v", decision, err)
		}
	}
	// A replayed turn reads the request it opened instead of asking twice.
	if store.stored.Status != domain.ApprovalStatusPending || store.opened != 1 || store.gets != 2 {
		t.Fatalf("stored = %+v, opened = %d, gets = %d", store.stored, store.opened, store.gets)
	}
}

func TestGateRecordsTheAskAndTheTurnsRequester(t *testing.T) {
	store := &stubStore{}
	if _, err := gate(store).Decide(context.Background(), toolCall(`{"amount":1}`), policyAsk); err != nil {
		t.Fatal(err)
	}
	stored := store.stored
	if stored.Source != domain.ApprovalSourcePolicy || stored.RuleID != "policy" || stored.RiskTier != domain.RiskCritical ||
		stored.ReleaseVersion != "3" || stored.Requester.ID != "5" || !stored.DeadlineAt.Equal(gateNow.Add(time.Hour)) {
		t.Fatalf("stored = %+v", stored)
	}
	defaulted := &stubStore{}
	if _, err := gate(defaulted).Decide(context.Background(), toolCall(`{"amount":1}`), autonomyAsk); err != nil || defaulted.stored.RiskTier != domain.RiskHigh {
		t.Fatalf("an ask without a risk asks at high: %q, %v", defaulted.stored.RiskTier, err)
	}
	human := toolCall(`{"amount":1}`)
	human.Principal.Kind = domain.PrincipalHuman
	unpinned := &stubStore{}
	if _, err := gate(unpinned).Decide(context.Background(), human, autonomyAsk); err != nil || unpinned.stored.ReleaseVersion != "" {
		t.Fatalf("only an agent names its release: %q, %v", unpinned.stored.ReleaseVersion, err)
	}
}

func TestGateRefusesAMalformedAsk(t *testing.T) {
	for name, ask := range map[string]domain.ApprovalAsk{
		"unknown source": {Source: "magic", RuleID: "x"},
		"blank rule":     {Source: domain.ApprovalSourcePolicy, RuleID: " "},
		"long rule":      {Source: domain.ApprovalSourcePolicy, RuleID: string(make([]byte, 81))},
		"unknown tier":   {Source: domain.ApprovalSourcePolicy, RuleID: "policy", RiskTier: "extreme"},
	} {
		store := &stubStore{}
		if decision, err := gate(store).Decide(context.Background(), toolCall(`{}`), ask); !errors.Is(err, domain.ErrValidation) || decision != domain.ApprovalDenied || store.opened != 0 {
			t.Errorf("%s: %q, %v, opened %d", name, decision, err, store.opened)
		}
	}
}

func TestGateReturnsTheRecordedVerdict(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	call := toolCall(`{"amount":1}`)
	if _, err := g.Decide(context.Background(), call, autonomyAsk); err != nil {
		t.Fatal(err)
	}
	for status, want := range map[domain.ApprovalStatus]domain.ApprovalDecision{
		domain.ApprovalStatusApproved: domain.ApprovalApproved,
		domain.ApprovalStatusRejected: domain.ApprovalDenied,
		domain.ApprovalStatusExpired:  domain.ApprovalDenied,
	} {
		store.stored.Status = status
		decision, err := g.Decide(context.Background(), call, autonomyAsk)
		if err != nil || decision != want {
			t.Fatalf("%s: decision = %q, error = %v, want %q", status, decision, err, want)
		}
	}
	// An edited proposal never runs as proposed; asking to run it ends the turn.
	store.stored.Status = domain.ApprovalStatusEdited
	if decision, err := g.Decide(context.Background(), call, autonomyAsk); decision != domain.ApprovalDenied || !errors.Is(err, domain.ErrProposalRevised) {
		t.Fatalf("edited: decision = %q, error = %v, want denied with ErrProposalRevised", decision, err)
	}
}

func TestGateRefusesAReplayForAChangedProposal(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	if _, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), autonomyAsk); err != nil {
		t.Fatal(err)
	}
	if decision, err := g.Decide(context.Background(), toolCall(`{"amount":1000}`), autonomyAsk); !errors.Is(err, domain.ErrConflict) || decision != domain.ApprovalDenied {
		t.Fatalf("changed proposal = %q, %v", decision, err)
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
	if _, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), autonomyAsk); err != nil {
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
	if err != nil || step.Backup != backup || step.Extension != time.Hour || step.Resolved() != domain.EscalationReassign {
		t.Fatalf("step = %+v, error = %v", step, err)
	}
	// No backup for the tier means abandon, never a silent widening.
	none, err := policy.Escalate(context.Background(), domain.ApprovalRequest{RiskTier: domain.RiskCritical})
	if err != nil || none.Backup.ID != "" || none.Resolved() != domain.EscalationExpire {
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

func routedAuthority(step int) domain.ApprovalRouteAuthority {
	return domain.ApprovalRouteAuthority{RouteID: "payments", Reference: domain.ApproverReference{Kind: domain.ApproverRole, Ref: "controller"}, Step: step}
}

func TestGateRecordsTheRoutedAssignmentAndItsDeadline(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	g.Router = fake.ApproverRouterFunc(func(_ context.Context, request domain.ApprovalRequest, prefer domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		if request.Source != domain.ApprovalSourcePolicy || request.RiskTier != domain.RiskCritical || prefer.ID != "" {
			t.Fatalf("routed %+v, prefer %+v", request, prefer)
		}
		return domain.ApprovalAssignment{Approver: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"}, Authority: routedAuthority(1), Deadline: 4 * time.Hour}, nil
	})
	notified := 0
	g.Notifier = fake.NotifierFunc(func(context.Context, domain.Notification) error { notified++; return nil })
	decision, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), policyAsk)
	if err != nil || decision != domain.ApprovalPending || store.stored.Approver.ID != "77" || store.stored.Route != routedAuthority(1) ||
		!store.stored.DeadlineAt.Equal(gateNow.Add(4*time.Hour)) || notified != 1 {
		t.Fatalf("decision = %q, error = %v, stored = %+v, notified %d", decision, err, store.stored, notified)
	}
}

func TestGateNeverRoutesOrNotifiesAReplay(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	routed, notified := 0, 0
	g.Notifier = fake.NotifierFunc(func(context.Context, domain.Notification) error { notified++; return nil })
	g.Router = fake.ApproverRouterFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		routed++
		return domain.ApprovalAssignment{Approver: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"}}, nil
	})
	if _, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), autonomyAsk); err != nil {
		t.Fatal(err)
	}
	// The resolver is down now; a replay, even one asked by another source, must still read the request.
	g.Router = fake.ApproverRouterFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		t.Fatal("a replay must not route")
		return domain.ApprovalAssignment{}, errors.New("resolver down")
	})
	for _, ask := range []domain.ApprovalAsk{autonomyAsk, {Source: domain.ApprovalSourceGuardrail, RuleID: "pay.approval"}} {
		if decision, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), ask); err != nil || decision != domain.ApprovalPending {
			t.Fatalf("replay = %q, %v", decision, err)
		}
	}
	if routed != 1 || notified != 1 || store.opened != 1 {
		t.Fatalf("routed %d, notified %d, opened %d", routed, notified, store.opened)
	}
}

func TestGateOpensAVacantRoutedRequestWithoutNotifying(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	notified := 0
	g.Notifier = fake.NotifierFunc(func(context.Context, domain.Notification) error { notified++; return nil })
	g.Router = fake.ApproverRouterFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		return domain.ApprovalAssignment{Authority: routedAuthority(0), Deadline: 30 * time.Minute}, nil
	})
	decision, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), autonomyAsk)
	if err != nil || decision != domain.ApprovalPending || store.stored.Approver.ID != "" || store.stored.Route.RouteID != "payments" || notified != 0 {
		t.Fatalf("vacant = %q, %v, %+v, notified %d", decision, err, store.stored, notified)
	}
}

func TestGateRefusesAnUnroutedRequestWithNoApprover(t *testing.T) {
	store := &stubStore{}
	g := gate(store)
	g.Router = fake.ApproverRouterFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		return domain.ApprovalAssignment{}, nil
	})
	if decision, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), autonomyAsk); !errors.Is(err, domain.ErrForbidden) || decision != domain.ApprovalDenied || store.opened != 0 {
		t.Fatalf("unrouted = %q, %v", decision, err)
	}
	g.Router = fake.ApproverRouterFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		return domain.ApprovalAssignment{}, domain.ErrForbidden
	})
	if _, err := g.Decide(context.Background(), toolCall(`{"amount":1}`), autonomyAsk); !errors.Is(err, domain.ErrForbidden) || store.opened != 0 {
		t.Fatalf("router refusal = %v", err)
	}
}
