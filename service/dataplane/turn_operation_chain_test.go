package dataplane

import (
	"context"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/observability"
)

// §8.10: hold → takeover → act → handback, twice, then the agent completes: VerifyTurnChain and
// VerifyTurnControl both accept the chain, and the operator records of the two epochs are keyed
// apart by their fences.
func TestOperatedTurnChainVerifies(t *testing.T) {
	ctx := context.Background()
	fixture := newOperationFixture(t)
	control := fixture.control
	control.status["request-1"], control.fence["request-1"] = domain.TurnQueued, 2
	hold := domain.TurnHoldRequest{AgentID: "writer-a", RequestID: "request-1", Mode: domain.HoldModeCheckpoint, Reason: "inspect"}
	for round, fence := range []int64{3, 7} {
		if round == 1 {
			// The worker acquires the handed-back turn, is asked to hold, and holds at a checkpoint.
			control.status["request-1"], control.fence["request-1"] = domain.TurnRunning, 6
			if _, err := fixture.service.Hold(ctx, usageActor, hold); err != nil {
				t.Fatal(err)
			}
			control.status["request-1"], control.fence["request-1"] = domain.TurnHeld, fence
			if err := auditTurnControl(ctx, fixture.audit, usageActor, "writer-a", "request-1", observability.AuditCategoryTurnHeld, fence, "held", time.Now()); err != nil {
				t.Fatal(err)
			}
		} else if _, err := fixture.service.Hold(ctx, usageActor, hold); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.TakeOver(ctx, usageActor, domain.TurnTakeoverRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence}); err != nil {
			t.Fatalf("takeover %d: %v", round, err)
		}
		if _, err := fixture.service.Act(ctx, usageActor, domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence + 1, Kind: domain.OperatorNote, Text: "note"}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.HandBack(ctx, usageActor, domain.TurnHandbackRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence + 1}); err != nil {
			t.Fatal(err)
		}
	}
	completed := domain.WithDecisionScope(ctx, "turn/", "request-1")
	if err := fixture.audit.Record(completed, domain.DecisionRecord{TenantID: 7, RequestID: "request-1", Category: observability.AuditCategoryTurnCompleted,
		Action: "turn", Resource: "writer-a", Payload: []byte(`{"fence":10}`)}); err != nil {
		t.Fatal(err)
	}
	records := append([]domain.DecisionRecord{{TenantID: 7, RequestID: "request-1", Category: observability.AuditCategoryTurnAdmitted, DecisionKey: "admitted"}},
		fixture.audit.Request(7, "request-1")...)
	if err := observability.VerifyTurnChain(records); err != nil {
		t.Fatalf("turn chain = %v (%v)", err, categories(records))
	}
	if err := observability.VerifyTurnControl(records, nil); err != nil {
		t.Fatalf("turn control = %v (%v)", err, categories(records))
	}
	var actions []domain.DecisionRecord
	for _, record := range records {
		if record.Category == observability.AuditCategoryTurnOperatorAction {
			actions = append(actions, record)
		}
	}
	if len(actions) != 2 || actions[0].DecisionKey == actions[1].DecisionKey {
		t.Fatalf("operator records = %+v", actions)
	}
}
