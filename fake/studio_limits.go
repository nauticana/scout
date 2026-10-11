package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioLimitsBackendFunc adapts a function to contract.StudioLimitsBackend.
type StudioLimitsBackendFunc func(context.Context, domain.StudioActor, string) (domain.AgentLimitsReport, error)

// Limits invokes the configured function.
func (function StudioLimitsBackendFunc) Limits(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentLimitsReport, error) {
	return function(ctx, actor, agentID)
}

var _ contract.StudioLimitsBackend = StudioLimitsBackendFunc(nil)
