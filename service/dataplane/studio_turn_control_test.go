package dataplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/observability"
)

// turnControlFake answers holds by a turn's status and releases by its held fence.
type turnControlFake struct {
	status   map[string]domain.TurnStatus
	fence    map[string]int64
	holds    []string
	released []string
	release  error
	// operation is the slice-3 half of the fake, in studio_turn_operation_test.go.
	operation turnOperationFake
}

var _ contract.TurnControl = (*turnControlFake)(nil)

func (control *turnControlFake) RequestHold(_ context.Context, _ int64, requestID string, code domain.TurnHoldCode, reason string) (domain.TurnHoldOutcome, error) {
	control.holds = append(control.holds, requestID+"/"+string(code)+"/"+reason)
	switch status := control.status[requestID]; status {
	case domain.TurnQueued, domain.TurnSuspended:
		control.status[requestID], control.fence[requestID] = domain.TurnHeld, control.fence[requestID]+1
		return domain.TurnHoldOutcome{Status: domain.TurnHeld, Fence: control.fence[requestID]}, nil
	case domain.TurnRunning, domain.TurnStreaming:
		return domain.TurnHoldOutcome{Status: status, Fence: control.fence[requestID]}, nil
	}
	return domain.TurnHoldOutcome{}, fmt.Errorf("%w: %s", domain.ErrNotFound, requestID)
}

func (control *turnControlFake) Preempt(_ context.Context, _ int64, requestID string) (domain.TurnHoldOutcome, error) {
	control.holds = append(control.holds, requestID+"/"+string(domain.HoldPreempted))
	switch control.status[requestID] {
	case domain.TurnQueued, domain.TurnSuspended, domain.TurnRunning, domain.TurnStreaming:
		control.status[requestID], control.fence[requestID] = domain.TurnHeld, control.fence[requestID]+1
		return domain.TurnHoldOutcome{Status: domain.TurnHeld, Fence: control.fence[requestID]}, nil
	case domain.TurnHeld:
		return domain.TurnHoldOutcome{Status: domain.TurnHeld, Fence: control.fence[requestID]}, nil
	}
	return domain.TurnHoldOutcome{}, fmt.Errorf("%w: %s", domain.ErrNotFound, requestID)
}

func (control *turnControlFake) Release(_ context.Context, _ int64, requestID string, expectedFence int64) (int64, error) {
	control.released = append(control.released, requestID)
	if control.release != nil {
		return 0, control.release
	}
	if control.status[requestID] != domain.TurnHeld || control.fence[requestID] != expectedFence {
		return 0, fmt.Errorf("%w: stale fence", domain.ErrConflict)
	}
	control.status[requestID], control.fence[requestID] = domain.TurnQueued, expectedFence+1
	return expectedFence + 1, nil
}

// §8.8: suspend holds queued and parked turns at once and asks running ones to hold, counting
// each, after admission closes; nothing is cancelled, and unpausing releases nothing.
func TestStudioPauseSuspendHoldsParkedTurnsAndAsksRunningOnesToHold(t *testing.T) {
	studio := &enabledStudio{}
	query := &queueQueryFake{sequence: map[string][][][]any{qPauseHoldableTurns: {{{"queued"}, {"parked"}, {"running"}, {"ended"}}, nil}}}
	control := &turnControlFake{
		status: map[string]domain.TurnStatus{"queued": domain.TurnQueued, "parked": domain.TurnSuspended, "running": domain.TurnRunning},
		fence:  map[string]int64{"queued": 0, "parked": 3, "running": 5},
	}
	audit := &observability.MemoryAuditSink{}
	pause := &StudioPause{DB: queueDBFake{query: query}, Studio: studio, Control: control, Audit: audit,
		Canceller: cancellerFunc(func(context.Context, int64, string, string) error {
			t.Fatal("suspend must not cancel")
			return nil
		})}
	result, err := pause.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseSuspend, Reason: "incident 12"})
	if err != nil || result.Held != 2 || result.HoldRequested != 1 || result.Cancelled != 0 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if studio.request.Enabled || slices.Contains(query.calls, qPauseLiveTurns) {
		t.Fatalf("admission must close first and only holdable turns are listed: %+v, %v", studio.request, query.calls)
	}
	if control.holds[0] != "queued/"+string(domain.HoldAgentPaused)+"/agent paused: incident 12" {
		t.Fatalf("holds = %v", control.holds)
	}
	var categories []string
	for _, requestID := range []string{"queued", "parked", "running"} {
		for _, record := range audit.Request(7, requestID) {
			categories = append(categories, record.Category)
		}
	}
	want := []string{observability.AuditCategoryTurnHeld, observability.AuditCategoryTurnHeld, observability.AuditCategoryTurnHoldRequested}
	if !slices.Equal(categories, want) || len(control.released) != 0 {
		t.Fatalf("audits = %v, released = %v", categories, control.released)
	}
	missing := &StudioPause{DB: queueDBFake{query: &queueQueryFake{}}, Studio: &enabledStudio{}, Canceller: cancellerFunc(nil)}
	if _, err := missing.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseSuspend, Reason: "x"}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("suspend without turn control = %v", err)
	}
}

// operateRoles grants the AGENT_OPERATION actions it lists on agent writer-a.
type operateRoles struct{ actions []domain.OperationAction }

func (roles operateRoles) Authorize(_ context.Context, _ domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
	allowed := object == domain.OperationAuthorizationObject && value == "agent:writer-a" && slices.Contains(roles.actions, domain.OperationAction(action))
	return domain.AuthorizationGrant{Allowed: allowed}, nil
}

type turnControlFixture struct {
	query    *queueQueryFake
	control  *turnControlFake
	fences   *MemoryTurnFences
	outcomes *MemoryCallOutcomeStore
	journal  *MemoryLoopJournal
	budget   *fake.TenantBudgetManager
	reserved []domain.BudgetReservation
	refunded []string
	audit    *observability.MemoryAuditSink
	service  *StudioTurnControl
}

func liveTurnRow(requestID string, status domain.TurnStatus, fence int64, holdCode string) []any {
	queued := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return []any{requestID, "conversation-1", int64(4), string(status), fence, holdCode, false, "", false, queued, nil, nil, "", "", nil}
}

func newTurnControlFixture(actions ...domain.OperationAction) *turnControlFixture {
	fixture := &turnControlFixture{
		query: &queueQueryFake{rows: map[string][][]any{
			qOperateTurn:     {liveTurnRow("request-1", domain.TurnHeld, 3, string(domain.HoldCheckpoint))},
			qOperateDispatch: {{"conversation-1", "writer-a", "", "route:request-1"}},
		}},
		control: &turnControlFake{status: map[string]domain.TurnStatus{"request-1": domain.TurnHeld}, fence: map[string]int64{"request-1": 3}},
		audit:   &observability.MemoryAuditSink{},
		fences:  &MemoryTurnFences{},
	}
	fixture.journal = &MemoryLoopJournal{Fences: fixture.fences}
	fixture.outcomes = &MemoryCallOutcomeStore{Fences: fixture.fences, Journal: fixture.journal}
	fixture.budget = &fake.TenantBudgetManager{
		ReserveFunc: func(_ context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
			reservation := domain.BudgetReservation{TenantID: request.TenantID, ReservationID: "reservation-2", RequestID: request.RequestID, Currency: "USD"}
			fixture.reserved = append(fixture.reserved, reservation)
			return reservation, nil
		},
		ReleaseFunc: func(_ context.Context, reservation domain.BudgetReservation) error {
			fixture.refunded = append(fixture.refunded, reservation.ReservationID)
			return nil
		},
	}
	fixture.service = &StudioTurnControl{
		DB: queueDBFake{query: fixture.query}, Authorizer: allowStudio{domain.StudioView},
		Principals: fake.PrincipalResolverFunc(func(_ context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
			return domain.Principal{Kind: ref.Kind, ID: ref.ID, TenantID: tenantID}, nil
		}),
		Roles: operateRoles{actions: actions}, Control: fixture.control, Estimator: &fake.TurnBudgetEstimator{}, Budget: fixture.budget,
		Outcomes: fixture.outcomes, Journal: fixture.journal, Audit: fixture.audit,
	}
	return fixture
}

func TestStudioTurnControlAuthorizesOnAgentOperation(t *testing.T) {
	ctx := context.Background()
	viewer := newTurnControlFixture(domain.OperationView)
	if _, err := viewer.service.Turn(ctx, usageActor, "writer-a", "request-1"); err != nil {
		t.Fatalf("VIEW reads a turn: %v", err)
	}
	if _, err := viewer.service.Hold(ctx, usageActor, domain.TurnHoldRequest{AgentID: "writer-a", RequestID: "request-1", Mode: domain.HoldModeCheckpoint, Reason: "x"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("hold without OPERATE = %v, want forbidden", err)
	}
	if _, err := viewer.service.Release(ctx, usageActor, domain.TurnReleaseRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("release without OPERATE = %v, want forbidden", err)
	}
	if len(viewer.control.holds) != 0 || len(viewer.control.released) != 0 || len(viewer.reserved) != 0 {
		t.Fatal("a refused operation changed a turn")
	}
	none := newTurnControlFixture()
	if _, err := none.service.LiveTurns(ctx, usageActor, domain.LiveTurnFilter{AgentID: "writer-a"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("no grant = %v, want forbidden", err)
	}
	// An agent the actor cannot view in Studio reads as not found, whatever its operation grants.
	hidden := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	hidden.service.Authorizer = allowStudio{domain.StudioEdit}
	if _, err := hidden.service.Turn(ctx, usageActor, "writer-a", "request-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unviewable agent = %v, want not found", err)
	}
	// A turn of another agent reads as not found.
	other := newTurnControlFixture(domain.OperationView)
	other.query.rows[qOperateTurn] = nil
	if _, err := other.service.Turn(ctx, usageActor, "writer-a", "request-9"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another agent's turn = %v, want not found", err)
	}
	if args := other.query.firstArgs(qOperateTurn); len(args) != 3 || args[0] != int64(7) || args[1] != "writer-a" {
		t.Fatalf("the turn is read within the actor's tenant and agent: %v", args)
	}
}

func TestStudioTurnControlHoldsAtACheckpointOnRequest(t *testing.T) {
	fixture := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	fixture.control.status["request-1"] = domain.TurnRunning
	ctx := context.Background()
	if _, err := fixture.service.Hold(ctx, usageActor, domain.TurnHoldRequest{AgentID: "writer-a", RequestID: "request-1", Mode: "now", Reason: "x"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unknown mode = %v, want validation", err)
	}
	if _, err := fixture.service.Hold(ctx, usageActor, domain.TurnHoldRequest{AgentID: "writer-a", RequestID: "request-1", Mode: domain.HoldModeCheckpoint}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a hold needs a reason, got %v", err)
	}
	outcome, err := fixture.service.Hold(ctx, usageActor, domain.TurnHoldRequest{AgentID: "writer-a", RequestID: "request-1", Mode: domain.HoldModeCheckpoint, Reason: "inspect"})
	if err != nil || outcome.Held() || outcome.Status != domain.TurnRunning {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	if fixture.control.holds[0] != "request-1/"+string(domain.HoldCheckpoint)+"/inspect" {
		t.Fatalf("holds = %v", fixture.control.holds)
	}
	if records := fixture.audit.Request(7, "request-1"); len(records) != 1 || records[0].Category != observability.AuditCategoryTurnHoldRequested {
		t.Fatalf("audit = %+v", records)
	}
}

// Decision 10: a release re-admits the turn's budget first; a refused re-admission leaves the
// turn held, and a refused release, such as a stale fence, releases only the hold it newly took.
func TestStudioTurnControlReleaseReadmitsFirst(t *testing.T) {
	ctx := context.Background()
	request := domain.TurnReleaseRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3}

	released := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	released.query.sequence = map[string][][][]any{qOperateTurn: {
		{liveTurnRow("request-1", domain.TurnHeld, 3, string(domain.HoldCheckpoint))}, {liveTurnRow("request-1", domain.TurnQueued, 4, "")},
	}}
	turn, err := released.service.Release(ctx, usageActor, request)
	if err != nil || turn.Status != domain.TurnQueued || turn.Fence != 4 || len(released.reserved) != 1 || len(released.refunded) != 0 {
		t.Fatalf("release = %+v, %v, reserved %v, refunded %v", turn, err, released.reserved, released.refunded)
	}
	if records := released.audit.Request(7, "request-1"); len(records) != 1 || records[0].Category != observability.AuditCategoryTurnReleased {
		t.Fatalf("audit = %+v", records)
	}

	refused := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	refused.budget.ReserveFunc = func(context.Context, domain.BudgetRequest) (domain.BudgetReservation, error) {
		return domain.BudgetReservation{}, fmt.Errorf("%w: window spent", domain.ErrBudgetExceeded)
	}
	if _, err := refused.service.Release(ctx, usageActor, request); !errors.Is(err, domain.ErrBudgetExceeded) || len(refused.control.released) != 0 {
		t.Fatalf("refused re-admission = %v, released %v", err, refused.control.released)
	}

	raced := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	raced.control.release = fmt.Errorf("%w: turn moved", domain.ErrConflict)
	if _, err := raced.service.Release(ctx, usageActor, request); !errors.Is(err, domain.ErrConflict) || !slices.Equal(raced.refunded, []string{"reservation-2"}) {
		t.Fatalf("raced release = %v, refunded %v", err, raced.refunded)
	}
	// A hold that was already live before the release is not this call's to give back.
	kept := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	kept.control.release = fmt.Errorf("%w: turn moved", domain.ErrConflict)
	kept.budget.ReserveFunc = func(_ context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
		return domain.BudgetReservation{TenantID: request.TenantID, ReservationID: "reservation-1", RequestID: request.RequestID, Replayed: true}, nil
	}
	if _, err := kept.service.Release(ctx, usageActor, request); !errors.Is(err, domain.ErrConflict) || len(kept.refunded) != 0 {
		t.Fatalf("replayed hold released: %v, %v", err, kept.refunded)
	}

	stale := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	request.Fence = 2
	if _, err := stale.service.Release(ctx, usageActor, request); !errors.Is(err, domain.ErrConflict) || len(stale.reserved) != 0 {
		t.Fatalf("stale fence = %v, reserved %v", err, stale.reserved)
	}
}

func TestStudioTurnControlPagesLiveTurnsNewestFirst(t *testing.T) {
	fixture := newTurnControlFixture(domain.OperationView)
	fixture.query.rows[qOperateLiveTurns] = [][]any{
		liveTurnRow("request-3", domain.TurnRunning, 1, ""), liveTurnRow("request-2", domain.TurnHeld, 2, "A"), liveTurnRow("request-1", domain.TurnQueued, 0, ""),
	}
	page, err := fixture.service.LiveTurns(context.Background(), usageActor, domain.LiveTurnFilter{AgentID: "writer-a", Limit: 2})
	if err != nil || len(page.Turns) != 2 || page.Turns[1].HoldCode != domain.HoldAgentPaused || page.NextBefore == "" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if args := fixture.query.firstArgs(qOperateLiveTurns); len(args) != 7 || args[6] != 3 {
		t.Fatalf("listing args = %v", args)
	}
	next, err := fixture.service.LiveTurns(context.Background(), usageActor, domain.LiveTurnFilter{AgentID: "writer-a", Before: page.NextBefore, Limit: 2})
	if err != nil || len(next.Turns) == 0 {
		t.Fatalf("next = %+v, %v", next, err)
	}
	if args := fixture.query.args[qOperateLiveTurns][1]; args[2] != page.NextBefore || args[5] != "request-2" {
		t.Fatalf("cursor args = %v", args)
	}
	for _, filter := range []domain.LiveTurnFilter{{AgentID: "writer-a", Limit: 201}, {AgentID: "writer-a", Limit: -1}, {AgentID: "writer-a", Before: "nonsense"}} {
		if _, err := fixture.service.LiveTurns(context.Background(), usageActor, filter); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%+v = %v, want validation", filter, err)
		}
	}
	fixture.service.Limits = domain.LiveTurnLimits{PageSize: 300, MaxPageSize: 200}
	if _, err := fixture.service.LiveTurns(context.Background(), usageActor, domain.LiveTurnFilter{AgentID: "writer-a"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a default page above the maximum must be refused, got %v", err)
	}
}
