package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// StudioLimitsBackend reports the execution limits and budget frozen into an agent's
// stable release, with the agent's current use of them.
type StudioLimitsBackend interface {
	Limits(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentLimitsReport, error)
}
