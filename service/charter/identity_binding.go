package charter

import (
	"context"
	"errors"
	"fmt"

	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

// IdentityBinding is the release writer that keeps one principal across both
// libraries: a Scout agent id must name a Charter AgentIdentity, and the published
// version must be a definitionVersion one of that identity's definitions declares.
// Namespace maps a tenant to the Charter namespace that owns its agents.
type IdentityBinding struct {
	Agents    agent.Provider
	Namespace func(tenantID int64) string
}

var _ controlplane.ReleaseWriter = (*IdentityBinding)(nil)

// WriteRelease refuses the publish when either side is missing; it writes nothing.
func (b *IdentityBinding) WriteRelease(ctx context.Context, _ keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	if b.Agents == nil || b.Namespace == nil {
		return fmt.Errorf("%w: charter identity binding needs agents and a namespace", domain.ErrNotReady)
	}
	namespace := b.Namespace(tenantID)
	if namespace == "" {
		return fmt.Errorf("%w: tenant %d has no charter namespace", domain.ErrNotReady, tenantID)
	}
	identity, err := b.Agents.Identity(ctx, namespace, model.Ref{Namespace: namespace, ID: definition.AgentID})
	if errors.Is(err, corpus.ErrNotFound) {
		return fmt.Errorf("%w: agent %s has no charter identity in %s", domain.ErrValidation, definition.AgentID, namespace)
	}
	if err != nil {
		return fmt.Errorf("resolve charter identity of agent %s: %w", definition.AgentID, err)
	}
	definitions, err := b.Agents.Definitions(ctx)
	if err != nil {
		return fmt.Errorf("list charter agent definitions: %w", err)
	}
	for _, declared := range definitions {
		owner := declared.AgentIdentityID.Namespace
		if owner == "" {
			owner = declared.Namespace
		}
		if declared.AgentIdentityID.ID == identity.ID && owner == identity.Namespace && declared.DefinitionVersion == definition.Version {
			return nil
		}
	}
	return fmt.Errorf("%w: no charter definition of %s declares version %s", domain.ErrValidation, definition.AgentID, definition.Version)
}
