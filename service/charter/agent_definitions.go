package charter

import (
	"context"
	"errors"
	"fmt"

	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
)

// documentLister is the raw listing a corpus-backed agent.Provider such as agent.BaseProvider offers.
type documentLister interface {
	List(ctx context.Context, kind model.Kind) ([]*model.Document, error)
}

// declaredDefinitions resolves the Charter AgentIdentity a Scout agent id names in the tenant's namespace and
// the AgentDefinitions that namespace declares for it. A definition in another namespace never counts, even
// when it names the identity explicitly, so one tenant's documents cannot speak for another tenant's agent.
func declaredDefinitions(ctx context.Context, agents agent.Provider, namespaceOf func(int64) string, tenantID int64, agentID string) (model.AgentIdentity, []model.AgentDefinition, error) {
	if agents == nil || namespaceOf == nil {
		return model.AgentIdentity{}, nil, fmt.Errorf("%w: charter agent resolution needs agents and a namespace", domain.ErrNotReady)
	}
	namespace := namespaceOf(tenantID)
	if namespace == "" {
		return model.AgentIdentity{}, nil, fmt.Errorf("%w: tenant %d has no charter namespace", domain.ErrNotReady, tenantID)
	}
	// The identity id is the one an exported definition names, so both directions agree.
	identity, err := agents.Identity(ctx, namespace, model.Ref{Namespace: namespace, ID: charterAgentID(agentID)})
	if errors.Is(err, corpus.ErrNotFound) || errors.Is(err, corpus.ErrKindMismatch) {
		return model.AgentIdentity{}, nil, fmt.Errorf("%w: agent %s has no charter identity in %s", domain.ErrValidation, agentID, namespace)
	}
	if err != nil {
		return model.AgentIdentity{}, nil, fmt.Errorf("resolve charter identity of agent %s: %w", agentID, err)
	}
	all, err := namespaceDefinitions(ctx, agents, namespace)
	if err != nil {
		return model.AgentIdentity{}, nil, err
	}
	identityKey := corpus.DocumentKey{Namespace: identity.Namespace, ID: identity.ID}
	var declared []model.AgentDefinition
	for _, definition := range all {
		if definition.Namespace == namespace && corpus.KeyOf(definition.Namespace, definition.AgentIdentityID) == identityKey {
			declared = append(declared, definition)
		}
	}
	return identity, declared, nil
}

// namespaceDefinitions decodes only the namespace's own definitions when the provider lists raw documents, so a
// malformed definition in another tenant's namespace cannot fail this tenant's resolution.
func namespaceDefinitions(ctx context.Context, agents agent.Provider, namespace string) ([]model.AgentDefinition, error) {
	lister, raw := agents.(documentLister)
	if !raw {
		all, err := agents.Definitions(ctx)
		if err != nil {
			return nil, fmt.Errorf("list charter agent definitions: %w", err)
		}
		return all, nil
	}
	docs, err := lister.List(ctx, model.KindAgentDefinition)
	if err != nil {
		return nil, fmt.Errorf("list charter agent definitions: %w", err)
	}
	var out []model.AgentDefinition
	for _, doc := range docs {
		if doc == nil || doc.Namespace != namespace {
			continue
		}
		definition, err := corpus.Decode[model.AgentDefinition](doc)
		if err != nil {
			return nil, fmt.Errorf("decode charter definition %s in %s: %w", doc.ID, namespace, err)
		}
		out = append(out, definition)
	}
	return out, nil
}
