package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// StudioEvaluationGateBackend shows, decides, and overrides the evaluation gate of an agent's current draft.
type StudioEvaluationGateBackend interface {
	Gate(ctx context.Context, actor domain.StudioActor, agentID string) (domain.EvaluationGateState, error)
	EvaluateGate(ctx context.Context, actor domain.StudioActor, agentID string) (domain.EvaluationGateState, error)
	OverrideGate(ctx context.Context, actor domain.StudioActor, override domain.EvaluationGateOverride) (domain.EvaluationGateState, error)
}
