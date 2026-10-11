package charter

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

// IdentityBinding is the release writer that keeps one principal across both
// libraries: a Scout agent id must name a Charter AgentIdentity in the tenant's
// namespace, and the published version must be a definitionVersion one of that
// identity's definitions in the same namespace declares. A release that froze
// governance from a Charter AgentDefinition must have frozen that very definition.
// Namespace maps a tenant to the Charter namespace that owns its agents.
type IdentityBinding struct {
	Agents    agent.Provider
	Namespace func(tenantID int64) string
}

var _ controlplane.ReleaseWriter = (*IdentityBinding)(nil)

// WriteRelease refuses the publish when either side is missing; it writes nothing.
func (b *IdentityBinding) WriteRelease(ctx context.Context, _ keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	identity, declared, err := declaredDefinitions(ctx, b.Agents, b.Namespace, tenantID, definition.AgentID)
	if err != nil {
		return err
	}
	var bound []string
	for _, candidate := range declared {
		if candidate.DefinitionVersion == definition.Version {
			bound = append(bound, candidate.ID)
		}
	}
	if len(bound) == 0 {
		return fmt.Errorf("%w: no charter definition of %s declares version %s", domain.ErrValidation, definition.AgentID, definition.Version)
	}
	return frozenGovernanceBinds(definition, identity.Namespace, bound)
}

// frozenGovernanceBinds refuses a release whose governance section froze a Charter AgentDefinition other
// than one of the definitions that declare the release's version.
func frozenGovernanceBinds(definition domain.AgentDefinition, namespace string, bound []string) error {
	raw := definition.Sections[domain.GovernanceSection]
	if len(raw) == 0 {
		return nil
	}
	var frozen domain.AgentGovernance
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return fmt.Errorf("%w: governance section of agent %s: %v", domain.ErrValidation, definition.AgentID, err)
	}
	enterprise := frozen.Enterprise
	if enterprise == nil || enterprise.Document.Kind != string(model.KindAgentDefinition) {
		return nil
	}
	sameNamespace := enterprise.Document.Namespace == "" || enterprise.Document.Namespace == namespace
	for _, id := range bound {
		if sameNamespace && enterprise.Document.ID == id && enterprise.Version == definition.Version {
			return nil
		}
	}
	return fmt.Errorf("%w: release %s of %s froze governance of charter definition %s version %s, not the definition declaring %s",
		domain.ErrConflict, definition.Version, definition.AgentID, enterprise.Document.ID, enterprise.Version, definition.Version)
}
