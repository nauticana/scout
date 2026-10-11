package controlplane

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

var _ contract.DraftFreezer = (*StudioService)(nil)

// FreezeDraft validates and freezes the current saved draft as Publish would, without
// writing a release; the result's definition digest identifies it as an evaluation subject.
func (s *StudioService) FreezeDraft(ctx context.Context, actor domain.StudioActor, agentID string) (domain.FrozenDraft, error) {
	if err := s.Authorize(ctx, actor, domain.StudioTest, agentID); err != nil {
		return domain.FrozenDraft{}, err
	}
	draft, err := s.draft(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.FrozenDraft{}, err
	}
	definition, settings, err := s.frozenDraft(ctx, actor.TenantID, draft, nil)
	if err != nil {
		return domain.FrozenDraft{}, err
	}
	return domain.FrozenDraft{Definition: definition, Settings: settings}, nil
}

// frozenDraft is the one freeze sequence Publish, TestDraft, and FreezeDraft share, so a
// draft's digest is the digest its release will carry: validate the draft as enabled,
// compile its definition, and freeze it with its settings.
func (s *StudioService) frozenDraft(ctx context.Context, tenantID int64, draft domain.AgentDraft, compiledBy *int64) (domain.AgentDefinition, domain.EffectiveRelease, error) {
	publishable := draft
	publishable.Enabled = true
	if err := s.validateDraft(ctx, tenantID, publishable, true); err != nil {
		return domain.AgentDefinition{}, domain.EffectiveRelease{}, err
	}
	definition, err := s.definition(ctx, tenantID, draft)
	if err != nil {
		return domain.AgentDefinition{}, domain.EffectiveRelease{}, err
	}
	settings, err := s.freeze(ctx, tenantID, draft, &definition, compiledBy)
	if err != nil {
		return domain.AgentDefinition{}, domain.EffectiveRelease{}, err
	}
	return definition, settings, nil
}
