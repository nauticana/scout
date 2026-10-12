package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

// §2: every operation needs AGENT_STUDIO VIEW, which an unviewable agent fails as not found, and
// AGENT_OPERATION OPERATE; a refused call changes nothing.
func TestStudioTurnOperationsAuthorizeOnAgentOperation(t *testing.T) {
	ctx := context.Background()
	hidden := newOperationFixture(t)
	hidden.service.Authorizer = allowStudio{domain.StudioEdit}
	viewer := newOperationFixture(t)
	viewer.takeOver(t)
	viewer.service.Roles = operateRoles{actions: []domain.OperationAction{domain.OperationView}}
	for name, fixture := range map[string]struct {
		fixture *operationFixture
		want    error
	}{"unviewable agent": {hidden, domain.ErrNotFound}, "without OPERATE": {viewer, domain.ErrForbidden}} {
		service := fixture.fixture.service
		if _, err := service.TakeOver(ctx, usageActor, domain.TurnTakeoverRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3}); !errors.Is(err, fixture.want) {
			t.Fatalf("%s: takeover = %v", name, err)
		}
		if _, err := service.Act(ctx, usageActor, domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 4, Kind: domain.OperatorNote, Text: "x"}); !errors.Is(err, fixture.want) {
			t.Fatalf("%s: act = %v", name, err)
		}
		if _, err := service.HandBack(ctx, usageActor, domain.TurnHandbackRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 4}); !errors.Is(err, fixture.want) {
			t.Fatalf("%s: handback = %v", name, err)
		}
		if _, err := service.Finish(ctx, usageActor, domain.TurnFinishRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 4, Reply: []byte("x")}); !errors.Is(err, fixture.want) {
			t.Fatalf("%s: finish = %v", name, err)
		}
		if op := fixture.fixture.control.operation; len(op.entries) != 0 || len(op.handbacks) != 0 || len(op.finishes) != 0 {
			t.Fatalf("%s: a refused operation changed the turn: %+v", name, op)
		}
	}
}
