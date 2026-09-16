package charter

import (
	"context"
	"fmt"
	"time"

	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/identity"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/temporal"
)

// Description is a runtime with the exact identity and definition version it operates, resolved at one instant.
type Description struct {
	Runtime    model.AgentRuntime
	Identity   identity.Actor
	Definition model.AgentDefinition
}

// Directory answers effective-time questions about runtimes through Charter's own lifecycle, never a Scout copy of it.
type Directory struct {
	Agents     agent.Provider
	Identities identity.Resolver
}

// Describe fails closed when the identity is not active at the instant, or the definition belongs to another
// identity, declares a different version than the runtime operates, or is not effective then.
func (d *Directory) Describe(ctx context.Context, namespace string, runtime model.Ref, at time.Time) (Description, error) {
	if d.Agents == nil || d.Identities == nil {
		return Description{}, fmt.Errorf("charter directory: agents and identities are required")
	}
	rt, err := d.Agents.Runtime(ctx, namespace, runtime)
	if err != nil {
		return Description{}, err
	}
	actor, err := d.Identities.ActiveAt(ctx, namespace, model.ObjectRef{Kind: model.KindAgentIdentity, ID: rt.AgentIdentityID.ID, Namespace: rt.AgentIdentityID.Namespace}, at)
	if err != nil {
		return Description{}, err
	}
	def, err := d.Agents.Definition(ctx, namespace, rt.AgentDefinitionID)
	if err != nil {
		return Description{}, err
	}
	if def.AgentIdentityID.ID != rt.AgentIdentityID.ID {
		return Description{}, fmt.Errorf("charter directory: runtime %s operates definition %s of another identity", rt.ID, def.ID)
	}
	if def.DefinitionVersion != rt.DefinitionVersion {
		return Description{}, fmt.Errorf("charter directory: runtime %s operates version %s, definition %s declares %s", rt.ID, rt.DefinitionVersion, def.ID, def.DefinitionVersion)
	}
	if def.Validity != nil && !temporal.NewPeriod(*def.Validity).Contains(at) {
		return Description{}, fmt.Errorf("charter directory: definition %s is not effective at %s", def.ID, at.Format("2006-01-02"))
	}
	return Description{Runtime: rt, Identity: actor, Definition: def}, nil
}
