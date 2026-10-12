package dataplane

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/observability"
)

// operatedCode is the turn_status code of a turn a person operates, spelled as a literal so these
// regressions also compile against code that predates it.
const operatedCode = domain.TurnStatus("O")

// A delivery that finds the turn operated by a person is a held no-op, never a conflict that
// would be retried or dead-lettered.
func TestAnOperatedTurnRefusesAcquisitionAsHeld(t *testing.T) {
	if err := refusedAcquisition("request-1", operatedCode, true); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("acquiring an operated turn = %v, want ErrTurnHeld", err)
	}
}

// Decision 11: a verdict on an operated turn is kept for whoever ends the operation; the turn is
// neither failed nor resumed.
func TestAnOperatedTurnIsNeitherFailedNorResumedByAVerdict(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{
		qRecordFind: {{int64(4), "conversation-1", string(operatedCode), strings.Repeat("a", 64), "", "", int64(5)}},
	}}
	store := &TableTurnRecordStore{DB: queueDBFake{query: query}, Objects: newObjectStateStore(&fake.ObjectStorage{})}
	if err := store.FailParked(context.Background(), 7, "request-1", "rejected"); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("failing an operated turn = %v, want ErrTurnHeld", err)
	}
	if err := store.Resume(context.Background(), 7, "request-1"); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("resuming an operated turn = %v, want ErrTurnHeld", err)
	}
}

// §6: cancelling a turn a person operates wakes it under a new fence and requeues it, so a worker
// ends it cancelled and releases its reservation.
func TestCancellingAnOperatedTurnWakesIt(t *testing.T) {
	table := newControlTable(operatedCode, 4)
	control := &TableTurnControl{DB: table}
	if err := control.Cancel(context.Background(), 7, "request-1", "user left"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(table.woken, qControlWake) || !slices.Contains(table.woken, qQueueRequeue) {
		t.Fatalf("an operated turn was not woken: %v", table.woken)
	}
	if !strings.Contains(turnControlQueries[qControlWake], "'"+string(operatedCode)+"'") {
		t.Fatalf("the wake does not name the operated status:\n%s", turnControlQueries[qControlWake])
	}
}

// A hold or preemption of a turn a person operates is refused as already held, not reported as
// a turn that does not exist.
func TestAHoldOfAnOperatedTurnIsRefusedAsHeld(t *testing.T) {
	control := &TableTurnControl{DB: newControlTable(operatedCode, 4)}
	if _, err := control.RequestHold(context.Background(), 7, "request-1", domain.HoldCheckpoint, "inspect"); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("hold of an operated turn = %v, want ErrTurnHeld", err)
	}
	if _, err := control.Preempt(context.Background(), 7, "request-1"); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("preemption of an operated turn = %v, want ErrTurnHeld", err)
	}
}

// operatedPause refuses the hold of the one turn a person took over and holds the others.
type operatedPause struct {
	contract.TurnControl
	held []string
}

func (control *operatedPause) RequestHold(_ context.Context, _ int64, requestID string, _ domain.TurnHoldCode, _ string) (domain.TurnHoldOutcome, error) {
	if requestID == "operated" {
		return domain.TurnHoldOutcome{}, domain.ErrTurnHeld
	}
	control.held = append(control.held, requestID)
	return domain.TurnHoldOutcome{Status: domain.TurnHeld, Fence: 2}, nil
}

// §6: pause suspend skips a turn a person took over between the listing and the hold.
func TestPauseSkipsATurnAPersonTookOver(t *testing.T) {
	query := &queueQueryFake{sequence: map[string][][][]any{qPauseHoldableTurns: {{{"operated"}, {"queued"}}, nil}}}
	control := &operatedPause{}
	pause := &StudioPause{DB: queueDBFake{query: query}, Studio: &enabledStudio{}, Control: control, Audit: &observability.MemoryAuditSink{},
		Canceller: cancellerFunc(nil)}
	result, err := pause.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseSuspend, Reason: "incident"})
	if err != nil || result.Held != 1 || !slices.Equal(control.held, []string{"queued"}) {
		t.Fatalf("pause = %+v, %v, held %v", result, err, control.held)
	}
}

// G5 and §6: an operated turn is live: it blocks later turns of its conversation, is listed,
// cancellable, and paused, and counts toward the unresolved-outcome alert.
func TestTheLiveStatusListsNameTheOperatedStatus(t *testing.T) {
	literal := "'" + string(operatedCode) + "'"
	for name, sql := range map[string]string{
		"conversation order": inTurnOrder("q"), "active turn": durableSessionQueries[qSessionActiveTurn],
		"live turns": operateQueries[qOperateLiveTurns], "cancel request": turnControlQueries[qControlCancelRequest],
		"pause cancel listing": pauseQueries[qPauseLiveTurns], "unresolved alert": healthQueries[qHealthUnresolved],
	} {
		if !strings.Contains(sql, literal) {
			t.Errorf("%s does not name the operated status:\n%s", name, sql)
		}
	}
}

// A subscriber whose stream the cache lost while a person operates the turn gets the held frame.
func TestAStoredReplyOfAnOperatedTurnIsTheHeldFrame(t *testing.T) {
	hub := &CacheReplyHub{Records: &fake.TurnRecordStore{FindFunc: func(context.Context, int64, string) (domain.TurnRecord, error) {
		return domain.TurnRecord{TurnNo: 4, Status: operatedCode, Fence: 5}, nil
	}}}
	reply, err := hub.StoredReply(context.Background(), 7, "request-1", 2)
	if err != nil || !reply.Final || reply.ErrorCode != "held" || reply.Fence != 5 || len(reply.Events) != 1 || reply.Events[0].Kind != domain.TurnEventHeld {
		t.Fatalf("stored reply = %+v, %v", reply, err)
	}
}

// A person's call's outcome record decodes, so the turn's outcomes list and the release gate read it.
func TestAnOperatorCallOutcomeDecodes(t *testing.T) {
	row := []any{"request-1", int64(11), "op:4:1", "op:key", "shop.write", "2", "O", "U", int64(4), nil, nil, false,
		"", "", "", int64(0), "", "", nil, "", "", ""}
	outcome, err := decodeOutcome(7, row)
	if err != nil || outcome.Origin != domain.CallOrigin("O") || outcome.Status != domain.CallUnknown {
		t.Fatalf("operator call = %+v, %v", outcome, err)
	}
}
