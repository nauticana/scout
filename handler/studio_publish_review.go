package handler

import (
	"context"
	"net/http"
	"net/url"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeaturePublishReview is the Studio feature StudioPublishReviewHandler serves.
const StudioFeaturePublishReview = "publish_review"

// StudioPublishReviewHandler serves two-person publish reviews.
type StudioPublishReviewHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioPublishReviewBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioPublishReviewHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeaturePublishReview, Version: 1, Actions: []string{
		string(domain.StudioView), string(domain.StudioEdit), string(domain.StudioRestore), string(domain.StudioApprovePublish)}}
}

// Routes returns the review routes; the review path answers GET (show) and POST (request).
func (h *StudioPublishReviewHandler) Routes() map[string]http.HandlerFunc {
	show, request := query(&h.AbstractHandler, h.review), command(&h.AbstractHandler, h.request)
	return map[string]http.HandlerFunc{
		api.StudioPublishReviewPath: func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				show(w, r)
				return
			}
			request(w, r)
		},
		api.StudioPublishReviewApprovePath: command(&h.AbstractHandler, h.approve),
		api.StudioPublishReviewRejectPath:  command(&h.AbstractHandler, h.reject),
	}
}

func (h *StudioPublishReviewHandler) request(ctx context.Context, actor domain.StudioActor, request api.PublishReviewRequest) (any, error) {
	review, err := h.Backend.Request(ctx, actor, domain.PublishReviewRequest{AgentID: request.AgentName, Note: request.Note,
		SodExceptionID: request.SodExceptionID, RestoreVersion: request.RestoreVersion})
	if err != nil {
		return nil, err
	}
	return apiReview(review), nil
}

func (h *StudioPublishReviewHandler) review(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	state, err := h.Backend.Review(ctx, actor, query.Get("agent_name"), domain.PublishReviewKind(query.Get("kind")))
	if err != nil {
		return nil, err
	}
	result := api.PublishReviewState{AgentName: state.AgentID, Kind: string(state.Kind), Required: state.Required,
		ActiveVersion: state.ActiveVersion, CurrentDigest: state.CurrentDigest, Stale: state.Stale, OpenComments: apiComments(state.OpenComments)}
	if state.Review != nil {
		review := apiReview(*state.Review)
		result.Review = &review
	}
	return result, nil
}

func (h *StudioPublishReviewHandler) approve(ctx context.Context, actor domain.StudioActor, request api.PublishReviewDecision) (any, error) {
	review, err := h.Backend.Approve(ctx, actor, reviewDecision(request))
	if err != nil {
		return nil, err
	}
	return apiReview(review), nil
}

func (h *StudioPublishReviewHandler) reject(ctx context.Context, actor domain.StudioActor, request api.PublishReviewDecision) (any, error) {
	review, err := h.Backend.Reject(ctx, actor, reviewDecision(request))
	if err != nil {
		return nil, err
	}
	return apiReview(review), nil
}

func reviewDecision(request api.PublishReviewDecision) domain.PublishReviewDecision {
	return domain.PublishReviewDecision{AgentID: request.AgentName, ReviewID: request.ReviewID, Note: request.Note}
}

func apiReview(review domain.PublishReview) api.PublishReview {
	result := api.PublishReview{
		ReviewID: review.ReviewID, AgentName: review.AgentID, DefinitionDigest: review.DefinitionDigest,
		DraftRevision: review.DraftRevision, PromptProfileRevision: review.PromptProfileRevision, BaseVersion: review.BaseVersion,
		Status: string(review.Status), RequestedBy: review.RequestedBy, RequestNote: review.RequestNote, RequestedAt: review.RequestedAt,
		DecidedBy: review.DecidedBy, DecisionNote: review.DecisionNote, DecidedAt: review.DecidedAt,
		SodExceptionID: review.SodExceptionID, PublishedVersion: review.PublishedVersion, RestoreVersion: review.RestoreVersion,
		Diff: api.PublishReviewDiff{
			Editors: append([]int64{}, review.Diff.Editors...),
			Causes:  nonNil(review.Diff.Causes), ChangedLanguages: nonNil(review.Diff.ChangedLanguages),
			PromptSections: make([]api.PromptSectionChange, 0, len(review.Diff.PromptSections)),
			Settings:       make([]api.SettingChange, 0, len(review.Diff.Settings)),
		},
		Evaluations: make([]api.PublishReviewEvaluation, 0, len(review.Evaluations)),
	}
	for _, change := range review.Diff.PromptSections {
		result.Diff.PromptSections = append(result.Diff.PromptSections, api.PromptSectionChange{LanguageCode: change.LanguageCode,
			PromptSectionID: change.PromptSectionID, Caption: change.Caption, Change: string(change.Change)})
	}
	for _, change := range review.Diff.Settings {
		result.Diff.Settings = append(result.Diff.Settings, api.SettingChange{ResourceKind: string(change.ResourceKind),
			ResourceID: change.ResourceID, Change: string(change.Change), Before: change.Before, After: change.After})
	}
	for _, evaluation := range review.Evaluations {
		result.Evaluations = append(result.Evaluations, api.PublishReviewEvaluation{Name: evaluation.Name, Passed: evaluation.Passed,
			Summary: evaluation.Summary, Reference: evaluation.Reference})
	}
	return result
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
