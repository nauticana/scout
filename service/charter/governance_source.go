package charter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/temporal"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// GovernanceSource reads an agent's enterprise-owned governance from a tenant's Charter documents. It resolves
// the agent's AgentIdentity and the definitions its namespace declares as IdentityBinding does, and the one
// definition in force now governs: its lifecycle is active or unset and its validity holds. IdentityBinding
// then requires that definition to declare the published version. It stores nothing.
type GovernanceSource struct {
	Documents corpus.Source
	// Namespace maps a tenant to the Charter namespace that owns its agents.
	Namespace func(tenantID int64) string
	// Link, when set, is where a person reads or changes the governing definition: an absolute http or https URL.
	Link func(definition model.AgentDefinition) string
	// Now is the clock that decides which definition is in force, read as a UTC calendar date; nil is the system clock.
	Now func() time.Time
}

var _ contract.GovernanceSource = (*GovernanceSource)(nil)

// Snapshot fails closed, never reporting that the enterprise owns nothing, when the tenant has no namespace,
// the agent no identity, or not exactly one of its definitions is in force.
func (s *GovernanceSource) Snapshot(ctx context.Context, tenantID int64, agentID string) (domain.GovernanceSnapshot, bool, error) {
	if s.Documents == nil {
		return domain.GovernanceSnapshot{}, false, fmt.Errorf("%w: charter governance source needs documents", domain.ErrNotReady)
	}
	_, declared, err := declaredDefinitions(ctx, agent.NewBaseProvider(s.Documents), s.Namespace, tenantID, agentID)
	if err != nil {
		return domain.GovernanceSnapshot{}, false, err
	}
	governing, err := s.governing(agentID, declared)
	if err != nil {
		return domain.GovernanceSnapshot{}, false, err
	}
	governance := governanceOf(governing)
	encoded, err := json.Marshal(governance)
	if err != nil {
		return domain.GovernanceSnapshot{}, false, fmt.Errorf("encode charter governance of agent %s: %w", agentID, err)
	}
	sum := sha256.Sum256(encoded)
	snapshot := domain.GovernanceSnapshot{
		Document:   domain.CharterObjectRef{Kind: string(model.KindAgentDefinition), ID: governing.ID},
		Version:    governing.DefinitionVersion,
		Digest:     "sha256:" + hex.EncodeToString(sum[:]),
		Governance: governance,
	}
	if s.Link != nil {
		snapshot.Link = s.Link(governing)
	}
	return snapshot, true, nil
}

func (s *GovernanceSource) governing(agentID string, declared []model.AgentDefinition) (model.AgentDefinition, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	// Charter validity is a calendar date; reading it in UTC makes every process agree at midnight.
	now = now.UTC()
	var inForce []model.AgentDefinition
	for _, definition := range declared {
		active := definition.LifecycleState == "" || definition.LifecycleState == model.LifecycleActive
		if active && temporal.EffectiveAt(definition.Validity, now) {
			inForce = append(inForce, definition)
		}
	}
	switch len(inForce) {
	case 0:
		return model.AgentDefinition{}, fmt.Errorf("%w: no charter definition of %s is in force", domain.ErrValidation, agentID)
	case 1:
		return inForce[0], nil
	}
	versions := make([]string, 0, len(inForce))
	for _, definition := range inForce {
		versions = append(versions, definition.ID+"@"+definition.DefinitionVersion)
	}
	slices.Sort(versions)
	return model.AgentDefinition{}, fmt.Errorf("%w: charter definitions %s of %s are in force at once", domain.ErrConflict, strings.Join(versions, ", "), agentID)
}

// governanceOf copies the governance fields of a Charter definition in Charter's names and shapes.
func governanceOf(definition model.AgentDefinition) domain.AgentGovernance {
	governance := domain.AgentGovernance{
		Purpose:              definition.Purpose,
		ResponsibilityIDs:    charterRefs(definition.ResponsibilityIDs),
		Triggers:             nonEmpty(definition.Triggers),
		RequiredInputs:       nonEmpty(definition.RequiredInputs),
		ExpectedOutcomes:     nonEmpty(definition.ExpectedOutcomes),
		CapabilityIDs:        charterRefs(definition.CapabilityIDs),
		Policies:             nonEmpty(definition.Policies),
		ConfidenceBoundaries: nonEmpty(definition.ConfidenceBoundaries),
		EscalationConditions: nonEmpty(definition.EscalationConditions),
	}
	if definition.Accountable != (model.ObjectRef{}) {
		governance.Accountable = &domain.CharterObjectRef{
			Kind: string(definition.Accountable.Kind), ID: definition.Accountable.ID,
			Namespace: definition.Accountable.Namespace, External: definition.Accountable.External,
		}
	}
	return governance
}

func charterRefs(refs []model.Ref) []domain.CharterRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]domain.CharterRef, len(refs))
	for index, ref := range refs {
		out[index] = domain.CharterRef{Namespace: ref.Namespace, ID: ref.ID}
	}
	return out
}

func nonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return slices.Clone(values)
}
