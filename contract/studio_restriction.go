package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// StudioRestrictionBackend shows the restriction layers in force for the actor's tenant and
// replaces the tenant's own layer as a whole. It authorizes the actor itself: AGENT_RESTRICTION
// VIEW or REPLACE_TENANT at the tenant, and an agent's overrides also need AGENT_STUDIO VIEW
// on that agent.
type StudioRestrictionBackend interface {
	Restrictions(ctx context.Context, actor domain.StudioActor, agentID string) (domain.RestrictionOverview, error)
	ReplaceTenant(ctx context.Context, actor domain.StudioActor, layer domain.RestrictionLayer, expectedDigest string) (domain.RestrictionLayer, error)
}
