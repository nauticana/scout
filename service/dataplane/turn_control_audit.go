package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// auditTurnControl records one turn-control transition a person made, keyed by the fence it
// left the turn at so a retried request records it once; a nil sink records nothing.
func auditTurnControl(ctx context.Context, sink contract.AuditSink, actor domain.StudioActor, agentID, requestID, category string, fence int64, reason string, now time.Time) error {
	if sink == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"request_id": requestID, "agent_id": agentID, "fence": fence, "reason": reason})
	if err != nil {
		return fmt.Errorf("encode %s audit: %w", category, err)
	}
	scoped := domain.WithDecisionScope(context.WithoutCancel(ctx), "turn/", requestID, "/fence/", fence)
	if err := sink.Record(scoped, domain.DecisionRecord{
		TenantID: actor.TenantID, Principal: actor.Principal, Category: category, Action: "turn", Resource: agentID,
		Outcome: domain.DecisionAllow, Reason: reason, RequestID: requestID, Payload: payload, OccurredAt: now,
	}); err != nil {
		return fmt.Errorf("record %s: %w", category, err)
	}
	return nil
}
