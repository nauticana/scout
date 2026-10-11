package handler

import (
	"context"
	"net/http"
	"net/url"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureReview is the Studio feature StudioReviewHandler serves.
const StudioFeatureReview = "review"

// StudioReviewHandler serves the Review tab: an agent's sampled and low-rated turns,
// their grading and dismissal, and promotion of a graded turn into the golden set.
type StudioReviewHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioReviewBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioReviewHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureReview, Version: 1, Actions: []string{
		string(domain.StudioView), string(domain.StudioReview), string(domain.StudioEdit),
	}}
}

// Routes returns GET reviews, GET review/turn?item_id=, and POST review/grade, review/dismiss, and review/promote.
func (h *StudioReviewHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioReviewsPath:       query(&h.AbstractHandler, h.reviews),
		api.StudioReviewTurnPath:    query(&h.AbstractHandler, h.turn),
		api.StudioReviewGradePath:   command(&h.AbstractHandler, h.grade),
		api.StudioReviewDismissPath: command(&h.AbstractHandler, h.dismiss),
		api.StudioReviewPromotePath: command(&h.AbstractHandler, h.promote),
	}
}

func (h *StudioReviewHandler) reviews(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	before, beforeErr := optionalInt(query.Get("before"), 0)
	limit, limitErr := optionalInt(query.Get("limit"), 0)
	if beforeErr != nil || limitErr != nil {
		return nil, badRequest("before and limit must be integers")
	}
	filter := domain.ReviewFilter{
		AgentID: query.Get("agent_name"), Status: domain.ReviewStatus(query.Get("status")), Release: query.Get("release"),
		Before: before, Limit: int(limit),
	}
	if source := query.Get("source"); source != "" {
		filter.Sources = []domain.ReviewSource{domain.ReviewSource(source)}
	}
	page, err := h.Backend.Reviews(ctx, actor, filter)
	if err != nil {
		return nil, err
	}
	result := api.ReviewPage{Items: make([]api.ReviewItem, 0, len(page.Items)), NextBefore: page.NextBefore}
	for _, item := range page.Items {
		result.Items = append(result.Items, apiReviewItem(item))
	}
	return result, nil
}

func (h *StudioReviewHandler) turn(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	itemID, err := optionalInt(query.Get("item_id"), 0)
	if err != nil {
		return nil, badRequest("item_id must be an integer")
	}
	content, err := h.Backend.ReviewTurn(ctx, actor, itemID)
	if err != nil {
		return nil, err
	}
	return api.ReviewTurn{Item: apiReviewItem(content.Item), Input: string(content.Input), Response: string(content.Response)}, nil
}

func (h *StudioReviewHandler) grade(ctx context.Context, actor domain.StudioActor, request api.ReviewGradeRequest) (any, error) {
	item, err := h.Backend.Grade(ctx, actor, request.ItemID, domain.ReviewGrade{
		Score: request.Score, Verdict: domain.ReviewVerdict(request.Verdict), Notes: request.Notes, ExpectedBehavior: optionalBytes(request.ExpectedBehavior),
	})
	if err != nil {
		return nil, err
	}
	return apiReviewItem(item), nil
}

func (h *StudioReviewHandler) dismiss(ctx context.Context, actor domain.StudioActor, request api.ReviewDismissRequest) (any, error) {
	item, err := h.Backend.Dismiss(ctx, actor, request.ItemID, request.Notes)
	if err != nil {
		return nil, err
	}
	return apiReviewItem(item), nil
}

func (h *StudioReviewHandler) promote(ctx context.Context, actor domain.StudioActor, request api.ReviewPromoteRequest) (any, error) {
	item, set, err := h.Backend.Promote(ctx, actor, domain.ReviewPromotion{
		ItemID: request.ItemID, CaseName: request.CaseName, RiskTier: request.RiskTier, ExpectedRevision: request.ExpectedRevision,
	})
	if err != nil {
		return nil, err
	}
	return api.ReviewPromoted{Item: apiReviewItem(item), Set: toCaseSet(set)}, nil
}

func apiReviewItem(item domain.HumanReviewItem) api.ReviewItem {
	result := api.ReviewItem{
		ItemID: item.ItemID, AgentName: item.AgentID, Version: item.AgentVersion, Source: string(item.Source), Reason: item.Reason,
		RiskTier: item.RiskTier, Status: string(item.Status), EnqueuedAt: item.EnqueuedAt,
		ReviewerKind: string(item.Reviewer.Kind), ReviewerID: item.Reviewer.ID, ReviewedAt: optionalTime(item.ReviewedAt),
		GoldenSetID: item.PromotedSetID, CaseID: item.PromotedCaseID, PromotedAt: optionalTime(item.PromotedAt),
	}
	if item.Grade.Score > 0 {
		result.Grade = &api.ReviewGrade{
			Score: item.Grade.Score, Verdict: string(item.Grade.Verdict), Notes: item.Grade.Notes, ExpectedBehavior: string(item.Grade.ExpectedBehavior),
		}
	} else {
		result.Notes = item.Grade.Notes
	}
	return result
}

func optionalTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}
