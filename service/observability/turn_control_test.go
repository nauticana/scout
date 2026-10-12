package observability

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nauticana/scout/domain"
)

func controlRecord(category string, fence int64) domain.DecisionRecord {
	return domain.DecisionRecord{TenantID: 7, RequestID: "request-1", Category: category,
		DecisionKey: fmt.Sprintf("%s/%d", category, fence), Payload: fmt.Appendf(nil, `{"fence":%d}`, fence)}
}

// held, taken over, two actions, handed back, then completed by the agent, with an earlier call
// settled at the held fence.
func goodControlChain() []domain.DecisionRecord {
	return []domain.DecisionRecord{
		{TenantID: 7, RequestID: "request-1", Category: AuditCategoryTurnAdmitted, DecisionKey: "a"},
		controlRecord(AuditCategoryTurnHeld, 2), controlRecord(AuditCategoryCallOutcomeResolved, 2),
		controlRecord(AuditCategoryTurnTakenOver, 3), controlRecord(AuditCategoryTurnOperatorAction, 3),
		{TenantID: 7, RequestID: "request-1", Category: AuditCategoryTurnOperatorAction, DecisionKey: "op2", Payload: []byte(`{"fence":3}`)},
		controlRecord(AuditCategoryTurnHandedBack, 4), controlRecord(AuditCategoryTurnCompleted, 5),
	}
}

func settledBefore() []domain.CallOutcome {
	return []domain.CallOutcome{{Key: domain.CallOutcomeKey{CallID: "c1"}, Status: domain.CallCompleted, DispatchFence: 1, ResolvedFence: 2}}
}

// §5 and §8.10: VerifyTurnControl accepts hold → takeover → act → handback → complete, and the
// lapse and finish paths, and VerifyTurnChain keeps the new categories intermediate.
func TestVerifyTurnControlAcceptsTheOperatedPaths(t *testing.T) {
	chain := goodControlChain()
	if err := VerifyTurnControl(chain, settledBefore()); err != nil {
		t.Fatalf("handback path = %v", err)
	}
	if err := VerifyTurnChain(chain); err != nil {
		t.Fatalf("turn chain = %v", err)
	}
	lapsed := []domain.DecisionRecord{
		controlRecord(AuditCategoryTurnHeld, 2), controlRecord(AuditCategoryTurnTakenOver, 3), controlRecord(AuditCategoryTurnHoldLapsed, 4),
		controlRecord(AuditCategoryTurnTakenOver, 5), controlRecord(AuditCategoryTurnOperatorAction, 5), controlRecord(AuditCategoryTurnCompleted, 5),
	}
	if err := VerifyTurnControl(lapsed, nil); err != nil {
		t.Fatalf("lapse, second takeover, finish = %v", err)
	}
	released := []domain.DecisionRecord{controlRecord(AuditCategoryTurnHeld, 2), controlRecord(AuditCategoryTurnReleased, 3), controlRecord(AuditCategoryTurnFailed, 4)}
	if err := VerifyTurnControl(released, nil); err != nil {
		t.Fatalf("release path = %v", err)
	}
	unknown := []domain.DecisionRecord{controlRecord(AuditCategoryTurnHeld, 2), controlRecord(AuditCategoryTurnTakenOver, 3),
		controlRecord(AuditCategoryTurnOperatorAction, 3), controlRecord(AuditCategoryTurnHeld, 4), controlRecord(AuditCategoryTurnCancelled, 5)}
	if err := VerifyTurnControl(unknown, []domain.CallOutcome{{Key: domain.CallOutcomeKey{CallID: "op:3:1"}, Status: domain.CallUnknown, DispatchFence: 3}}); err != nil {
		t.Fatalf("an operator call holding the turn again, then a cancellation = %v", err)
	}
}

// §5: each broken order is refused: a takeover without a hold, an action outside its takeover's
// fence, a handback or lapse of a turn nobody operates, a release of a turn a person operates, a
// fence that goes back, a transition that does not move the fence, a takeover while a call was
// open, a worker completing an operated turn, and a record without its fence.
func TestVerifyTurnControlRejectsEachBrokenOrder(t *testing.T) {
	held, taken := controlRecord(AuditCategoryTurnHeld, 2), controlRecord(AuditCategoryTurnTakenOver, 3)
	for name, broken := range map[string]struct {
		records  []domain.DecisionRecord
		outcomes []domain.CallOutcome
	}{
		"takeover without hold":     {records: []domain.DecisionRecord{controlRecord(AuditCategoryTurnTakenOver, 3)}},
		"action before takeover":    {records: []domain.DecisionRecord{held, controlRecord(AuditCategoryTurnOperatorAction, 2)}},
		"action at another fence":   {records: []domain.DecisionRecord{held, taken, controlRecord(AuditCategoryTurnOperatorAction, 4)}},
		"handback of a held turn":   {records: []domain.DecisionRecord{held, controlRecord(AuditCategoryTurnHandedBack, 3)}},
		"lapse of a live turn":      {records: []domain.DecisionRecord{controlRecord(AuditCategoryTurnHoldLapsed, 3)}},
		"release while operated":    {records: []domain.DecisionRecord{held, taken, controlRecord(AuditCategoryTurnReleased, 4)}},
		"fence goes back":           {records: []domain.DecisionRecord{held, taken, controlRecord(AuditCategoryTurnHandedBack, 2)}},
		"transition keeps fence":    {records: []domain.DecisionRecord{held, controlRecord(AuditCategoryTurnTakenOver, 2)}},
		"takeover over open call":   {records: []domain.DecisionRecord{held, taken}, outcomes: []domain.CallOutcome{{Key: domain.CallOutcomeKey{CallID: "c1"}, Status: domain.CallUnknown, DispatchFence: 1}}},
		"call settled after":        {records: []domain.DecisionRecord{held, taken}, outcomes: []domain.CallOutcome{{Key: domain.CallOutcomeKey{CallID: "c1"}, Status: domain.CallCompleted, DispatchFence: 1, ResolvedFence: 3}}},
		"worker completes operated": {records: []domain.DecisionRecord{held, taken, controlRecord(AuditCategoryTurnCompleted, 6)}},
		"completion while held":     {records: []domain.DecisionRecord{held, controlRecord(AuditCategoryTurnCompleted, 2)}},
		"suspension while held":     {records: []domain.DecisionRecord{held, controlRecord(AuditCategoryTurnSuspended, 3)}},
		"record without fence":      {records: []domain.DecisionRecord{{Category: AuditCategoryTurnHeld, Payload: []byte(`{}`)}}},
	} {
		if err := VerifyTurnControl(broken.records, broken.outcomes); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("%s = %v, want ErrConflict", name, err)
		}
	}
}
