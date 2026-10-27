package dataplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/observability"
)

// separationFake is a tenant's operation separation: whether attesting needs a second person, and
// whether a person may attest.
type separationFake struct {
	second      bool
	requiredErr error
	permitErr   error
	asked       []domain.CallOutcome
}

func (s *separationFake) Required(_ context.Context, _ int64, _ string, call domain.CallOutcome, _ time.Time) (bool, error) {
	s.asked = append(s.asked, call)
	return s.second, s.requiredErr
}

func (s *separationFake) Permit(context.Context, int64, string, domain.CallOutcome, domain.PrincipalRef, time.Time) error {
	return s.permitErr
}

type evidenceVerifierFake struct{ err error }

func (v evidenceVerifierFake) VerifyObject(context.Context, int64, domain.ObjectRef) error {
	return v.err
}

// heldCallFixture is turn request-1 of writer-a held at fence 3 with call b unknown, dispatched under fence 2.
func heldCallFixture(t *testing.T, actions ...domain.OperationAction) *turnControlFixture {
	t.Helper()
	fixture := newTurnControlFixture(actions...)
	if _, err := fixture.fences.Acquire(7, "request-1"); err != nil {
		t.Fatal(err)
	}
	owner, err := fixture.fences.Acquire(7, "request-1")
	if err != nil {
		t.Fatal(err)
	}
	key := domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11}
	decision := domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryModel, Iteration: 1,
		Model: &domain.ModelResult{ToolCalls: []domain.ModelToolCall{toolCall("b", "shop_write", `{"sku":"1"}`)}}}
	if _, err := fixture.journal.Append(context.Background(), owner, key, decision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.outcomes.Intend(context.Background(), owner, domain.CallIntent{Key: writeKey, IdempotencyKey: "loop:b",
		Tool: domain.ToolReference{ToolID: "shop.write", Version: "2"}, Window: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.outcomes.MarkUnknown(context.Background(), owner, writeKey); err != nil {
		t.Fatal(err)
	}
	fixture.fences.Hold(7, "request-1")
	fixture.query.rows[qOperateRelease] = [][]any{{"v3", ""}}
	return fixture
}

func attestRequest(outcome domain.CallOutcomeStatus, note string) domain.TurnAttestRequest {
	return domain.TurnAttestRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3,
		ExecutionStepID: 11, CallID: "b", Outcome: outcome, Note: note}
}

func (fixture *turnControlFixture) passDeadline() {
	fixture.outcomes.Now = func() time.Time { return time.Now().Add(time.Hour) }
}

// §8.6 and decision 7: attesting needs RECONCILE, waits for the call's deadline, fails closed
// without a separation policy or when it cannot be evaluated, and a high-risk call settles only
// on a second authorized person; each person's statement is audited, and the settlement once.
func TestStudioTurnControlAttestsUnderSeparation(t *testing.T) {
	ctx := context.Background()
	operator := heldCallFixture(t, domain.OperationView, domain.OperationOperate)
	operator.service.Separation = &separationFake{}
	operator.passDeadline()
	if _, err := operator.service.AttestOutcome(ctx, usageActor, attestRequest(domain.CallCompleted, "shipped")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("attesting without RECONCILE = %v", err)
	}

	fixture := heldCallFixture(t, domain.OperationView, domain.OperationReconcile)
	if _, err := fixture.service.AttestOutcome(ctx, usageActor, attestRequest(domain.CallCompleted, "shipped")); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("attesting without a separation policy = %v, want fail closed", err)
	}
	separation := &separationFake{requiredErr: errors.New("charter documents unavailable")}
	fixture.service.Separation = separation
	if _, err := fixture.service.AttestOutcome(ctx, usageActor, attestRequest(domain.CallCompleted, "shipped")); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("an unevaluable separation = %v, want fail closed", err)
	}
	separation.requiredErr, separation.second = nil, true
	if _, err := fixture.service.AttestOutcome(ctx, usageActor, attestRequest(domain.CallCompleted, "shipped")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("attesting before the deadline = %v", err)
	}
	fixture.passDeadline()
	separation.permitErr = errors.New("author of release v3")
	if _, err := fixture.service.AttestOutcome(ctx, usageActor, attestRequest(domain.CallCompleted, "shipped")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a separated person = %v", err)
	}
	separation.permitErr = nil
	stale := attestRequest(domain.CallCompleted, "shipped")
	stale.Fence = 2
	if _, err := fixture.service.AttestOutcome(ctx, usageActor, stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stale fence = %v", err)
	}
	first, err := fixture.service.AttestOutcome(ctx, usageActor, attestRequest(domain.CallCompleted, "shipped per ERP"))
	if err != nil || !first.AwaitsSecond || first.Outcome.Status != domain.CallUnknown {
		t.Fatalf("first person = %+v, %v", first, err)
	}
	replay, err := fixture.service.AttestOutcome(ctx, usageActor, attestRequest(domain.CallCompleted, "shipped per ERP"))
	if err != nil || !replay.AwaitsSecond || !replay.AlreadyStated {
		t.Fatalf("the same person again = %+v, %v", replay, err)
	}
	if _, err := fixture.service.AttestOutcome(ctx, usageActor, attestRequest(domain.CallFailed, "shipped per ERP")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a conflicting replay = %v", err)
	}
	second := usageActor
	second.Principal.ID = "43"
	settled, err := fixture.service.AttestOutcome(ctx, second, attestRequest(domain.CallCompleted, "confirmed by warehouse"))
	if err != nil || settled.AwaitsSecond || settled.Outcome.Status != domain.CallCompleted || settled.Outcome.Resolution != domain.ResolvedByAttestation {
		t.Fatalf("second person = %+v, %v", settled, err)
	}
	var categories []string
	for _, record := range fixture.audit.Request(7, "request-1") {
		categories = append(categories, record.Category)
	}
	want := []string{observability.AuditCategoryCallOutcomeAttested, observability.AuditCategoryCallOutcomeAttested, observability.AuditCategoryCallOutcomeResolved}
	if !slices.Equal(categories, want) {
		t.Fatalf("audits = %v, want %v", categories, want)
	}
	if len(separation.asked) == 0 || separation.asked[0].Tool.ToolID != "shop.write" {
		t.Fatalf("the separation was asked about %+v", separation.asked)
	}
}

// Attestation evidence must verify as the tenant's own object; without a verifier it is refused.
func TestStudioTurnControlVerifiesAttestationEvidence(t *testing.T) {
	ctx := context.Background()
	fixture := heldCallFixture(t, domain.OperationView, domain.OperationReconcile)
	fixture.service.Separation = &separationFake{}
	fixture.passDeadline()
	request := attestRequest(domain.CallFailed, "never arrived")
	request.Evidence = domain.ObjectRef{URI: "scout://evidence/7/x", Digest: strings.Repeat("b", 64)}
	if _, err := fixture.service.AttestOutcome(ctx, usageActor, request); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("evidence without a verifier = %v", err)
	}
	fixture.service.Evidence = evidenceVerifierFake{err: fmt.Errorf("%w: object of tenant 8", domain.ErrForbidden)}
	if _, err := fixture.service.AttestOutcome(ctx, usageActor, request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign evidence = %v", err)
	}
	fixture.service.Evidence = evidenceVerifierFake{}
	result, err := fixture.service.AttestOutcome(ctx, usageActor, request)
	if err != nil || result.Outcome.Status != domain.CallFailed || result.Outcome.Evidence != request.Evidence {
		t.Fatalf("attestation with evidence = %+v, %v", result, err)
	}
}

// A lookup asks the reconciler about the call as it was sent and settles it when definite, as the
// person who asked; an answer still unknown leaves the call open.
func TestStudioTurnControlLooksAnUnknownCallUp(t *testing.T) {
	ctx := context.Background()
	viewer := heldCallFixture(t, domain.OperationView)
	if _, err := viewer.service.LookupOutcome(ctx, usageActor, attestRequest(domain.CallCompleted, "").TurnOutcomeRequest); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a lookup without OPERATE = %v", err)
	}
	fixture := heldCallFixture(t, domain.OperationView, domain.OperationOperate)
	request := attestRequest(domain.CallCompleted, "").TurnOutcomeRequest
	if _, err := fixture.service.LookupOutcome(ctx, usageActor, request); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a lookup without a reconciler = %v", err)
	}
	reconciler := &reconcilerFake{report: domain.CallReport{Status: domain.CallUnknown}}
	fixture.service.Reconciler = reconciler
	still, err := fixture.service.LookupOutcome(ctx, usageActor, request)
	if err != nil || still.Status != domain.CallUnknown {
		t.Fatalf("an unknown answer = %+v, %v", still, err)
	}
	sent := reconciler.calls[0]
	if sent.ToolID != "shop.write" || string(sent.Arguments) != `{"sku":"1"}` || sent.IdempotencyKey != "loop:b" || sent.Principal.Release != "v3" ||
		sent.Principal.TenantID != 7 || sent.ConversationID != "conversation-1" {
		t.Fatalf("the lookup asked about %+v", sent)
	}
	reconciler.report = domain.CallReport{Status: domain.CallCompleted, Resolution: domain.ResolvedByEffect, Result: domain.ToolResult{Output: []byte(`{"sku":"1"}`)}}
	resolved, err := fixture.service.LookupOutcome(ctx, usageActor, request)
	if err != nil || resolved.Status != domain.CallCompleted || resolved.Resolution != domain.ResolvedByEffect || resolved.Resolver != usageActor.Principal {
		t.Fatalf("a definite answer = %+v, %v", resolved, err)
	}
	again, err := fixture.service.LookupOutcome(ctx, usageActor, request)
	if err != nil || again.Status != domain.CallCompleted || len(reconciler.calls) != 2 {
		t.Fatalf("a settled call is returned without asking again: %+v, %v, %d asks", again, err, len(reconciler.calls))
	}
	turn, err := fixture.service.Turn(ctx, usageActor, "writer-a", "request-1")
	if err != nil || len(turn.Outcomes) != 1 || turn.Outcomes[0].Status != domain.CallCompleted {
		t.Fatalf("the turn read carries its outcomes: %+v, %v", turn.Outcomes, err)
	}
	other := heldCallFixture(t, domain.OperationView, domain.OperationOperate)
	other.query.rows[qOperateTurn] = nil
	if _, err := other.service.LookupOutcome(ctx, usageActor, request); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another agent's turn = %v", err)
	}
}

// Decision 18 at release: a held turn's reservation is settled only with its end, so a settled one
// refuses the release instead of letting the turn run unheld.
func TestStudioTurnControlRefusesToReleaseOntoASettledReservation(t *testing.T) {
	fixture := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	fixture.budget.ReserveFunc = func(context.Context, domain.BudgetRequest) (domain.BudgetReservation, error) {
		return domain.BudgetReservation{}, fmt.Errorf("%w: request-1", domain.ErrBudgetSettled)
	}
	_, err := fixture.service.Release(context.Background(), usageActor, domain.TurnReleaseRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3})
	if !errors.Is(err, domain.ErrBudgetSettled) || len(fixture.control.released) != 0 {
		t.Fatalf("release onto a settled reservation = %v, released %v", err, fixture.control.released)
	}
}
