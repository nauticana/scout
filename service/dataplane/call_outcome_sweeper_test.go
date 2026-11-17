package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/observability"
)

func sweeperOf(fixture *turnControlFixture, reconciler contract.ToolOutcomeReconciler) *CallOutcomeSweeper {
	service := fixture.service
	return &CallOutcomeSweeper{DB: service.DB, Outcomes: service.Outcomes, Journal: service.Journal, Reconciler: reconciler,
		Control: service.Control, Principals: service.Principals, Audit: service.Audit}
}

// The sweep asks about a due call as an operator's lookup does, settles a definite answer as the
// platform, leaves an unknown one open, and continues after the last call it asked about.
func TestCallOutcomeSweeperSettlesDueCallsAsThePlatform(t *testing.T) {
	ctx := context.Background()
	fixture := heldCallFixture(t)
	deadline := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	due := []any{int64(7), "request-1", int64(11), "b", int64(3), "writer-a", deadline}
	fixture.query.rows[qSweepDueCalls] = [][]any{due}

	if _, err := sweeperOf(fixture, nil).Sweep(ctx, 1); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a sweep without a reconciler = %v", err)
	}
	reconciler := &reconcilerFake{report: domain.CallReport{Status: domain.CallUnknown}}
	sweeper := sweeperOf(fixture, reconciler)
	if _, err := sweeper.Sweep(ctx, 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a sweep of no calls = %v", err)
	}
	if settled, err := sweeper.Sweep(ctx, 1); settled != 0 || err != nil || len(reconciler.calls) != 1 {
		t.Fatalf("an unknown answer settled %d, %v", settled, err)
	}
	if sent := reconciler.calls[0]; sent.ToolID != "shop.write" || string(sent.Arguments) != `{"sku":"1"}` || sent.Principal.Release != "v3" {
		t.Fatalf("the sweep asked about %+v", sent)
	}
	reconciler.report = domain.CallReport{Status: domain.CallCompleted, Resolution: domain.ResolvedByLookup, Result: domain.ToolResult{Output: []byte(`{}`)}}
	if settled, err := sweeper.Sweep(ctx, 1); settled != 1 || err != nil {
		t.Fatalf("a definite answer settled %d, %v", settled, err)
	}
	resolved, _, err := fixture.outcomes.Find(ctx, writeKey)
	if err != nil || resolved.Status != domain.CallCompleted || resolved.Resolver != domain.PlatformPrincipal {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	records := fixture.audit.Request(7, "request-1")
	if len(records) != 1 || records[0].Category != observability.AuditCategoryCallOutcomeResolved || records[0].Principal != domain.PlatformPrincipal {
		t.Fatalf("audit = %+v", records)
	}

	cursors := fixture.query.args[qSweepDueCalls]
	if first, next := cursors[0], cursors[1]; first[1] != int64(0) || next[0] != deadline || next[1] != int64(7) || next[2] != "request-1" || next[4] != "b" {
		t.Fatalf("a full page continues after its last call: %v then %v", first, next)
	}
	fixture.query.rows[qSweepDueCalls] = nil
	if _, err := sweeper.Sweep(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := sweeper.Sweep(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if last := fixture.query.args[qSweepDueCalls]; last[len(last)-1][1] != int64(0) {
		t.Fatalf("a short page starts the next sweep over: %v", last[len(last)-1])
	}
}

// A failed lookup is reported and the sweep goes on; a call settled or gone meanwhile is skipped,
// and a cancelled sweep asks about nothing more.
func TestCallOutcomeSweeperCollectsFailuresAndSkipsMovedCalls(t *testing.T) {
	ctx := context.Background()
	fixture := heldCallFixture(t)
	deadline := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fixture.query.rows[qSweepDueCalls] = [][]any{
		{int64(7), "request-1", int64(11), "gone", int64(3), "writer-a", deadline},
		{int64(7), "request-1", int64(11), "b", int64(3), "writer-a", deadline},
	}
	reconciler := &reconcilerFake{err: errors.New("provider down")}
	sweeper := sweeperOf(fixture, reconciler)
	if settled, err := sweeper.Sweep(ctx, 5); settled != 0 || err == nil || len(reconciler.calls) != 1 {
		t.Fatalf("a failed lookup settled %d after %d asks, %v", settled, len(reconciler.calls), err)
	}
	reconciler.err = nil
	reconciler.report = domain.CallReport{Status: domain.CallCompleted, Resolution: domain.ResolvedByLookup, Result: domain.ToolResult{Output: []byte(`{}`)}}
	fixture.query.rows[qSweepDueCalls][1][4] = int64(2)
	if settled, err := sweeper.Sweep(ctx, 5); settled != 0 || err != nil {
		t.Fatalf("a call whose turn moved to another fence settled %d, %v", settled, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := sweeper.Sweep(cancelled, 5); !errors.Is(err, context.Canceled) || len(reconciler.calls) != 2 {
		t.Fatalf("a cancelled sweep = %v after %d asks", err, len(reconciler.calls))
	}
}
