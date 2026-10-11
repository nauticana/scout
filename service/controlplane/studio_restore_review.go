package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

const maxRestoreVersion = 80

func validateRestoreVersion(version string) error {
	if version != strings.TrimSpace(version) || len(version) > maxRestoreVersion {
		return validationError("restore_version", "must be a published version of at most 80 characters without surrounding spaces")
	}
	return nil
}

// restoreCandidate is the review of restoring version over the release active now, and the
// candidate its evaluations judge: the version's frozen definition and settings. The diff
// changes the active release into the restored one; a restore has no editors, so its only
// author is its requester.
func (r *StudioPublishReviews) restoreCandidate(ctx context.Context, actor domain.StudioActor, agentID, version string) (domain.PublishReview, domain.PublishCandidate, error) {
	definition, found, err := publishedDefinition(ctx, r.qs, actor.TenantID, agentID, version)
	if err != nil {
		return domain.PublishReview{}, domain.PublishCandidate{}, err
	}
	if !found {
		return domain.PublishReview{}, domain.PublishCandidate{}, fmt.Errorf("%w: agent %q has no version %s", domain.ErrNotFound, agentID, version)
	}
	if len(definition.DefinitionDigest) != 64 {
		return domain.PublishReview{}, domain.PublishCandidate{}, fmt.Errorf("%w: version %s has no definition digest to review", domain.ErrConflict, version)
	}
	settings, err := r.releases().Get(ctx, actor.TenantID, agentID, version)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.PublishReview{}, domain.PublishCandidate{}, fmt.Errorf("%w: version %s has no frozen settings and cannot be restored", domain.ErrConflict, version)
	}
	if err != nil {
		return domain.PublishReview{}, domain.PublishCandidate{}, fmt.Errorf("load restored settings: %w", err)
	}
	base, active, activeSettings, err := r.activeRelease(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.PublishReview{}, domain.PublishCandidate{}, err
	}
	review := domain.PublishReview{
		AgentID: agentID, DefinitionDigest: definition.DefinitionDigest, DraftRevision: definition.DraftRevision,
		PromptProfileRevision: definition.PromptProfileRevision, BaseVersion: base, RestoreVersion: version,
		Diff: domain.PublishReviewDiff{ActiveVersion: base, PromptSections: scope.PromptSectionChanges(active.Languages, definition.Languages),
			Settings: scope.SettingChanges(activeSettings, settings.Resources)},
	}
	return review, domain.PublishCandidate{Actor: actor, Definition: definition, Settings: settings, RestoredFrom: version}, nil
}

// restoreUnchanged refuses a restore review whose version is gone or no longer has the reviewed
// digest, or whose active release moved since it was requested.
func (r *StudioPublishReviews) restoreUnchanged(ctx context.Context, qs keelport.QueryService, tenantID int64, review domain.PublishReview) error {
	definition, found, err := publishedDefinition(ctx, qs, tenantID, review.AgentID, review.RestoreVersion)
	if err != nil {
		return err
	}
	if !found || definition.DefinitionDigest != review.DefinitionDigest {
		return fmt.Errorf("%w: version %s no longer has the definition review %d approves", domain.ErrConflict, review.RestoreVersion, review.ReviewID)
	}
	return r.activeUnchanged(ctx, qs, qReviewActive, tenantID, review)
}

// activeUnchanged refuses a restore review once another release became active: its diff no
// longer describes what restoring changes.
func (r *StudioPublishReviews) activeUnchanged(ctx context.Context, qs keelport.QueryService, query string, tenantID int64, review domain.PublishReview) error {
	active, err := activeVersion(ctx, qs, query, tenantID, review.AgentID)
	if err != nil {
		return err
	}
	if active != review.BaseVersion {
		return fmt.Errorf("%w: version %s is active, not %s that review %d was requested over", domain.ErrConflict, active, review.BaseVersion, review.ReviewID)
	}
	return nil
}

// restoreState is the agent's latest restore review, stale once the active release moved.
func (r *StudioPublishReviews) restoreState(ctx context.Context, tenantID int64, state domain.PublishReviewState) (domain.PublishReviewState, error) {
	active, err := activeVersion(ctx, r.qs, qReviewActive, tenantID, state.AgentID)
	if err != nil {
		return domain.PublishReviewState{}, err
	}
	state.ActiveVersion = active
	latest, err := r.qs.Query(ctx, qReviewLatestRestore, tenantID, state.AgentID)
	if err != nil {
		return domain.PublishReviewState{}, fmt.Errorf("load restore review: %w", err)
	}
	if len(latest.Rows) > 0 {
		review, scanErr := scanReview(state.AgentID, latest.Rows[0])
		if scanErr != nil {
			return domain.PublishReviewState{}, scanErr
		}
		state.Review = &review
		state.Stale = review.BaseVersion != active
	}
	return state, nil
}

func activeVersion(ctx context.Context, qs keelport.QueryService, query string, tenantID int64, agentID string) (string, error) {
	res, err := qs.Query(ctx, query, tenantID, agentID)
	if err != nil {
		return "", fmt.Errorf("load active release: %w", err)
	}
	if len(res.Rows) == 0 {
		return "", nil
	}
	return common.AsString(res.Rows[0][0]), nil
}

func publishedDefinition(ctx context.Context, qs keelport.QueryService, tenantID int64, agentID, version string) (domain.AgentDefinition, bool, error) {
	res, err := qs.Query(ctx, qReviewVersion, tenantID, agentID, version)
	if err != nil {
		return domain.AgentDefinition{}, false, fmt.Errorf("load version %s: %w", version, err)
	}
	if len(res.Rows) == 0 {
		return domain.AgentDefinition{}, false, nil
	}
	definition, err := decodeDefinition(res.Rows[0][0])
	return definition, err == nil, err
}
