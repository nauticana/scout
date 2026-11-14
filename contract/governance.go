package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// GovernanceSource resolves the enterprise-owned governance of an agent: the current version of a
// snapshot the enterprise keeps, such as a Charter AgentDefinition, and a link to it. ok is false
// when the enterprise owns nothing of the agent. Studio never stores what a snapshot supplies.
type GovernanceSource interface {
	// Snapshot resolves the governance declaring version, the release a restore creates; an empty
	// version is what is in force now, which a draft and a publish freeze.
	Snapshot(ctx context.Context, tenantID int64, agentID, version string) (snapshot domain.GovernanceSnapshot, ok bool, err error)
}

// CharterObjects maps a tenant's Charter documents, in the tenant's namespace, to the object ids
// Scout's foreign keys reference, and back.
type CharterObjects interface {
	// ObjectIDs returns, in order, the ids of the stored documents of kind with the identifiers;
	// one not stored as that kind is ErrNotFound.
	ObjectIDs(ctx context.Context, tenantID int64, kind string, identifiers ...string) ([]int64, error)
	// Identifiers returns, in order, the identifiers of the documents the ids store.
	Identifiers(ctx context.Context, tenantID int64, ids ...int64) ([]string, error)
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
