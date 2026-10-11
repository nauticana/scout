package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// GovernanceSource resolves the enterprise-owned governance of an agent: the current version of a
// snapshot the enterprise keeps, such as a Charter AgentDefinition, and a link to it. ok is false
// when the enterprise owns nothing of the agent. Studio never stores what a snapshot supplies.
type GovernanceSource interface {
	Snapshot(ctx context.Context, tenantID int64, agentID string) (snapshot domain.GovernanceSnapshot, ok bool, err error)
}

// TriggerBindingVerifier proves that the product-owned schedule or topic a schedule or event
// trigger binds exists for the tenant and may start the agent; false refuses the trigger.
type TriggerBindingVerifier interface {
	Verify(ctx context.Context, tenantID int64, agentID string, trigger domain.AgentTrigger) (bool, error)
}

// StudioGovernanceBackend shows and edits the governance fields of an agent's Charter definition.
type StudioGovernanceBackend interface {
	Governance(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentGovernanceView, error)
	SaveGovernance(ctx context.Context, actor domain.StudioActor, change domain.AgentGovernanceChange) (domain.AgentGovernanceView, error)
}
