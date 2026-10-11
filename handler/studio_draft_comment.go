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

// StudioFeatureComments is the Studio feature StudioDraftCommentHandler serves.
const StudioFeatureComments = "comments"

// StudioDraftCommentHandler serves comments on agent drafts.
type StudioDraftCommentHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioDraftCommentBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioDraftCommentHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureComments, Version: 1, Actions: []string{
		string(domain.StudioView), string(domain.StudioEdit), string(domain.StudioApprovePublish)}}
}

// Routes returns the comment routes.
func (h *StudioDraftCommentHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioCommentsPath:       query(&h.AbstractHandler, h.list),
		api.StudioCommentPath:        command(&h.AbstractHandler, h.comment),
		api.StudioCommentResolvePath: command(&h.AbstractHandler, h.resolve),
	}
}

func (h *StudioDraftCommentHandler) list(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	comments, err := h.Backend.Comments(ctx, actor, query.Get("agent_name"), domain.DraftCommentStatus(query.Get("status")))
	if err != nil {
		return nil, err
	}
	return apiComments(comments), nil
}

func (h *StudioDraftCommentHandler) comment(ctx context.Context, actor domain.StudioActor, request api.DraftComment) (any, error) {
	comment, err := h.Backend.Comment(ctx, actor, domain.DraftComment{
		AgentID: request.AgentName, DraftRevision: request.DraftRevision, Anchor: domain.DraftCommentAnchor(request.Anchor),
		PromptSectionID: request.PromptSectionID, LanguageCode: request.LanguageCode,
		ResourceKind: domain.ResourceKind(request.ResourceKind), ResourceID: request.ResourceID, Body: request.Body,
	})
	if err != nil {
		return nil, err
	}
	return apiComment(comment), nil
}

func (h *StudioDraftCommentHandler) resolve(ctx context.Context, actor domain.StudioActor, request api.DraftCommentResolveRequest) (any, error) {
	comment, err := h.Backend.Resolve(ctx, actor, request.AgentName, request.CommentID)
	if err != nil {
		return nil, err
	}
	return apiComment(comment), nil
}

func apiComments(comments []domain.DraftComment) []api.DraftComment {
	items := make([]api.DraftComment, 0, len(comments))
	for _, comment := range comments {
		items = append(items, apiComment(comment))
	}
	return items
}

func apiComment(comment domain.DraftComment) api.DraftComment {
	created := comment.CreatedAt
	item := api.DraftComment{
		CommentID: comment.CommentID, AgentName: comment.AgentID, DraftRevision: comment.DraftRevision, Anchor: string(comment.Anchor),
		PromptSectionID: comment.PromptSectionID, LanguageCode: comment.LanguageCode, ResourceKind: string(comment.ResourceKind),
		ResourceID: comment.ResourceID, Body: comment.Body, Status: string(comment.Status),
		AuthorKind: string(comment.Author.Kind), AuthorID: comment.Author.ID, CreatedAt: &created, ResolvedAt: comment.ResolvedAt,
	}
	if comment.ResolvedBy != nil {
		item.ResolvedByKind, item.ResolvedByID = string(comment.ResolvedBy.Kind), comment.ResolvedBy.ID
	}
	return item
}
