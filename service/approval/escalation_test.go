package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type sweepFixture struct {
	table   *approvalTable
	store   *TableStore
	sweeper *Sweeper
	failed  []string
	audited []domain.DecisionRecord
}

func newSweepFixture(t *testing.T, policy func(domain.ApprovalRequest) (domain.EscalationStep, error)) *sweepFixture {
	t.Helper()
	f := &sweepFixture{}
	f.table, f.store = newApprovalStore(false)
	f.store.Now = func() time.Time { return gateNow }
	f.sweeper = &Sweeper{
		Store: f.store, Now: func() time.Time { return gateNow },
		Policy: fake.EscalationPolicyFunc(func(_ context.Context, request domain.ApprovalRequest) (domain.EscalationStep, error) {
			return policy(request)
		}),
		Records: &fake.TurnRecordStore{FailFunc: func(_ context.Context, tenantID int64, requestID, status, code string) error {
			f.failed = append(f.failed, requestID+"/"+status+"/"+code)
			return nil
		}},
		Audit: &fake.AuditSink{RecordFunc: func(_ context.Context, record domain.DecisionRecord) error {
			f.audited = append(f.audited, record)
			return nil
		}},
	}
	return f
}

func (f *sweepFixture) open(t *testing.T, request domain.ApprovalRequest) domain.ApprovalRequest {
	t.Helper()
	opened, err := f.store.Open(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func (f *sweepFixture) get(requestID string) domain.ApprovalRequest {
	request, _ := f.store.Get(context.Background(), domain.ApprovalKey{TenantID: 7, RequestID: requestID, ExecutionStepID: 3})
	return request
}

func dueAt(request domain.ApprovalRequest, deadline time.Time) domain.ApprovalRequest {
	request.DeadlineAt = deadline
	return request
}

func TestSweepSkipsALostSwapAndCollectsOtherFailures(t *testing.T) {
	boom := errors.New("directory down")
	var f *sweepFixture
	f = newSweepFixture(t, func(request domain.ApprovalRequest) (domain.EscalationStep, error) {
		switch request.RequestID {
		case "lost":
			// Another sweeper moves the request between the read and this swap.
			if err := f.store.Reassign(context.Background(), request, domain.EscalationStep{Backup: person("1").Principal}); err != nil {
				t.Fatal(err)
			}
			return domain.EscalationStep{Backup: person("2").Principal}, nil
		case "broken":
			return domain.EscalationStep{}, boom
		default:
			return domain.EscalationStep{Backup: person("3").Principal, Extension: time.Hour}, nil
		}
	})
	past := gateNow.Add(-time.Minute)
	for _, id := range []string{"lost", "broken", "moved"} {
		f.open(t, dueAt(owed(id, "alex"), past))
	}
	moved, err := f.sweeper.Sweep(context.Background(), 7)
	if moved != 1 || !errors.Is(err, boom) || errors.Is(err, domain.ErrConflict) {
		t.Fatalf("moved = %d, err = %v", moved, err)
	}
	if f.get("lost").Approver.ID != "1" || f.get("moved").Approver.ID != "3" {
		t.Fatalf("lost = %+v, moved = %+v", f.get("lost").Approver, f.get("moved").Approver)
	}
}

func TestSweepWaitsOrFillsVacancies(t *testing.T) {
	action := domain.EscalationWait
	f := newSweepFixture(t, func(request domain.ApprovalRequest) (domain.EscalationStep, error) {
		if request.Approver.ID != "" || request.DeadlineAt.Before(gateNow) {
			t.Fatalf("only the vacancy is swept: %+v", request)
		}
		if action == domain.EscalationWait {
			return domain.EscalationStep{Action: action}, nil
		}
		return domain.EscalationStep{Action: action, Backup: person("77").Principal, KeepDeadline: true, Route: routedAuthority(0), Reason: "vacancy filled"}, nil
	})
	vacant := f.open(t, dueAt(routedOwed("vacant", "", 0), gateNow.Add(time.Hour)))
	f.open(t, dueAt(routedOwed("assigned", "77", 0), gateNow.Add(time.Hour)))
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 0 || err != nil || len(f.table.escalations) != 0 {
		t.Fatalf("wait: moved %d, %v, writes %d", moved, err, len(f.table.escalations))
	}
	action = domain.EscalationReassign
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 1 || err != nil {
		t.Fatalf("fill: moved %d, %v", moved, err)
	}
	filled := f.get("vacant")
	if filled.Approver.ID != "77" || !filled.DeadlineAt.Equal(vacant.DeadlineAt) || filled.Status != domain.ApprovalStatusPending {
		t.Fatalf("filled = %+v", filled)
	}
	if record := f.audited[0]; record.Action != "escalated" || !strings.Contains(record.Reason, "route payments, role controller, step 0") || record.Outcome != domain.DecisionDeny {
		t.Fatalf("audit = %+v", record)
	}
}

func TestSweepHoldsWithNoDeadline(t *testing.T) {
	f := newSweepFixture(t, func(request domain.ApprovalRequest) (domain.EscalationStep, error) {
		return domain.EscalationStep{Action: domain.EscalationHold, Backup: request.Approver, Route: request.Route, Reason: "escalation exhausted"}, nil
	})
	f.open(t, dueAt(routedOwed("held", "77", 3), gateNow.Add(-time.Minute)))
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 1 || err != nil {
		t.Fatalf("moved %d, %v", moved, err)
	}
	held := f.get("held")
	if !held.DeadlineAt.IsZero() || held.Approver.ID != "77" || held.Status != domain.ApprovalStatusEscalated || f.audited[0].Action != "held" {
		t.Fatalf("held = %+v, audit = %+v", held, f.audited)
	}
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 0 || err != nil {
		t.Fatalf("a held request is not swept again: %d, %v", moved, err)
	}
}

func TestSweepExpiresUnderTheFenceAndFailsTheTurn(t *testing.T) {
	f := newSweepFixture(t, func(domain.ApprovalRequest) (domain.EscalationStep, error) {
		return domain.EscalationStep{Action: domain.EscalationExpire, Reason: "escalation exhausted"}, nil
	})
	f.open(t, dueAt(routedOwed("late", "77", 1), gateNow.Add(-time.Minute)))
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 1 || err != nil {
		t.Fatalf("moved %d, %v", moved, err)
	}
	if expired := f.get("late"); expired.Status != domain.ApprovalStatusExpired {
		t.Fatalf("expired = %+v", expired)
	}
	if fence := f.table.fences[0]; fence[0] != 1 || fence[1] != "human" || fence[2] != "77" {
		t.Fatalf("expiry fence = %v", fence)
	}
	if len(f.failed) != 1 || f.failed[0] != "late/failed/expired" {
		t.Fatalf("failed turns = %v", f.failed)
	}
}

func TestSweepNeedsTurnRecords(t *testing.T) {
	f := newSweepFixture(t, func(domain.ApprovalRequest) (domain.EscalationStep, error) { return domain.EscalationStep{}, nil })
	f.sweeper.Records = nil
	if _, err := f.sweeper.Sweep(context.Background(), 7); err == nil {
		t.Fatal("a sweeper without turn records would leave an expired turn parked")
	}
}

func TestALaterSweepFailsATurnTheExpiryLeftParked(t *testing.T) {
	f := newSweepFixture(t, func(domain.ApprovalRequest) (domain.EscalationStep, error) {
		return domain.EscalationStep{Action: domain.EscalationExpire, Reason: "escalation exhausted"}, nil
	})
	down := errors.New("turn store down")
	// The expiry commits and failing the turn does not, both at once and in the same sweep's retry.
	failures := 2
	f.sweeper.Records = &fake.TurnRecordStore{FailFunc: func(_ context.Context, _ int64, requestID, status, code string) error {
		if failures > 0 {
			failures--
			return down
		}
		f.failed = append(f.failed, requestID+"/"+status+"/"+code)
		return nil
	}}
	opened := f.open(t, dueAt(routedOwed("late", "77", 1), gateNow.Add(-time.Minute)))
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 1 || !errors.Is(err, down) {
		t.Fatalf("moved %d, %v", moved, err)
	}
	if f.get("late").Status != domain.ApprovalStatusExpired || len(f.failed) != 0 || f.table.settled[opened.ID] {
		t.Fatalf("request %+v, failed %v, settled %t", f.get("late"), f.failed, f.table.settled[opened.ID])
	}
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 0 || err != nil {
		t.Fatalf("reconcile: moved %d, %v", moved, err)
	}
	if len(f.failed) != 1 || f.failed[0] != "late/failed/expired" || !f.table.settled[opened.ID] {
		t.Fatalf("failed %v, settled %t", f.failed, f.table.settled[opened.ID])
	}
	if _, err := f.sweeper.Sweep(context.Background(), 7); err != nil || len(f.failed) != 1 {
		t.Fatalf("a settled turn is not failed again: %v, %v", err, f.failed)
	}
}

func TestASweepSettlesATurnThatEndedAnotherWay(t *testing.T) {
	f := newSweepFixture(t, func(domain.ApprovalRequest) (domain.EscalationStep, error) {
		return domain.EscalationStep{Action: domain.EscalationExpire, Reason: "deadline passed"}, nil
	})
	f.sweeper.Records = &fake.TurnRecordStore{
		FailFunc: func(context.Context, int64, string, string, string) error { return domain.ErrConflict },
		FindFunc: func(context.Context, int64, string) (int64, string, []byte, error) { return 1, "cancelled", nil, nil },
	}
	opened := f.open(t, dueAt(owed("gone", "77"), gateNow.Add(-time.Minute)))
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 1 || err != nil || !f.table.settled[opened.ID] {
		t.Fatalf("moved %d, %v, settled %t", moved, err, f.table.settled[opened.ID])
	}
}

func TestSweepTellsTheApproverAVacancyIsFilled(t *testing.T) {
	f := newSweepFixture(t, func(request domain.ApprovalRequest) (domain.EscalationStep, error) {
		return domain.EscalationStep{Action: domain.EscalationReassign, Backup: person("77").Principal, KeepDeadline: true, Route: routedAuthority(0), Reason: "vacancy filled"}, nil
	})
	var told []domain.Notification
	f.sweeper.Notifier = fake.NotifierFunc(func(_ context.Context, notification domain.Notification) error {
		told = append(told, notification)
		return nil
	})
	vacant := f.open(t, dueAt(routedOwed("vacant", "", 0), gateNow.Add(time.Hour)))
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 1 || err != nil {
		t.Fatalf("moved %d, %v", moved, err)
	}
	if len(told) != 1 || told[0].Recipient.ID != "77" || told[0].Reference != "vacant" || !told[0].DueAt.Equal(vacant.DeadlineAt) {
		t.Fatalf("told = %+v", told)
	}
	f.store.NotifyThroughOutbox = true
	if _, err := f.sweeper.Sweep(context.Background(), 7); err == nil {
		t.Fatal("a notifier and the outbox together would tell the approver twice")
	}
}

func TestSweepReportsAFailedNoticeWithoutUndoingTheMove(t *testing.T) {
	f := newSweepFixture(t, func(domain.ApprovalRequest) (domain.EscalationStep, error) {
		return domain.EscalationStep{Backup: person("3").Principal, Extension: time.Hour}, nil
	})
	down := errors.New("mail down")
	f.sweeper.Notifier = fake.NotifierFunc(func(context.Context, domain.Notification) error { return down })
	f.open(t, dueAt(owed("late", "alex"), gateNow.Add(-time.Minute)))
	if moved, err := f.sweeper.Sweep(context.Background(), 7); moved != 1 || !errors.Is(err, down) || f.get("late").Approver.ID != "3" {
		t.Fatalf("moved %d, %v, approver %q", moved, err, f.get("late").Approver.ID)
	}
}
