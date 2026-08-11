package runtime

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ProviderCredentialCheck reports whether a provider's credential is missing.
// Returning true withholds every agent on that provider.
type ProviderCredentialCheck func(ctx context.Context, providerID string) bool

// ReadinessResolver narrows the control-plane readiness of deployed agents with
// the two checks that live outside Agent Studio: whether the published model is
// still offered by the catalog, and whether its provider credential exists.
// Products layer only their own concerns — task catalogs, quota — on the result.
type ReadinessResolver struct {
	Catalog contract.StudioModelCatalog
	// CredentialMissing is optional; without it credentials are not checked.
	CredentialMissing ProviderCredentialCheck
}

// AgentState is one alias's readiness after narrowing.
type AgentState struct {
	Readiness domain.AgentReadiness
	Reason    string
	Version   string
}

// Resolve narrows every deployed alias. The catalog and credential checks are
// each performed once per tenant and per provider, not once per agent.
func (resolver *ReadinessResolver) Resolve(ctx context.Context, tenantID int64, deployed map[string]domain.DeployedAgent) (map[string]AgentState, error) {
	if resolver == nil || resolver.Catalog == nil {
		return nil, fmt.Errorf("readiness resolver: a model catalog is required")
	}
	models, err := resolver.Catalog.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	active := make(map[domain.ModelReference]bool, len(models))
	for _, model := range models {
		active[model.Reference] = model.Active
	}

	missing := map[string]bool{}
	states := make(map[string]AgentState, len(deployed))
	for aliasID, agent := range deployed {
		states[aliasID] = resolver.narrow(ctx, agent, active, missing)
	}
	return states, nil
}

func (resolver *ReadinessResolver) narrow(ctx context.Context, agent domain.DeployedAgent, active map[domain.ModelReference]bool, missing map[string]bool) AgentState {
	readiness, reason := agent.Readiness()
	state := AgentState{Readiness: readiness, Reason: reason, Version: agent.Version}
	if readiness != domain.AgentReady {
		return state
	}
	textModel := *agent.Definition.Models.Text
	if !active[textModel] {
		state.Readiness, state.Reason = domain.AgentMissingModel, "text model is no longer available"
		return state
	}
	if resolver.CredentialMissing != nil {
		if _, checked := missing[textModel.ProviderID]; !checked {
			missing[textModel.ProviderID] = resolver.CredentialMissing(ctx, textModel.ProviderID)
		}
		if missing[textModel.ProviderID] {
			state.Readiness, state.Reason = domain.AgentError, "provider credential is not configured"
			return state
		}
	}
	return state
}
