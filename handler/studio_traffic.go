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

// StudioFeatureTraffic is the Studio feature StudioTrafficHandler serves.
const StudioFeatureTraffic = "traffic"

// StudioTrafficHandler serves canary, promote, rollback, and which-release explanations.
type StudioTrafficHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioTrafficBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioTrafficHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureTraffic, Version: 1, Actions: []string{string(domain.StudioView), string(domain.StudioPublish)}}
}

// Routes returns the traffic routes.
func (h *StudioTrafficHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioCanaryPath:   command(&h.AbstractHandler, h.canary),
		api.StudioPromotePath:  command(&h.AbstractHandler, h.promote),
		api.StudioRollbackPath: command(&h.AbstractHandler, h.rollback),
		api.StudioExplainPath:  query(&h.AbstractHandler, h.explain),
	}
}

func (h *StudioTrafficHandler) canary(ctx context.Context, actor domain.StudioActor, request api.AgentTrafficRequest) (any, error) {
	return apiDeployment(h.Backend.Canary(ctx, actor, request.AgentName, request.Version, request.Percentage))
}

func (h *StudioTrafficHandler) promote(ctx context.Context, actor domain.StudioActor, request api.AgentTrafficRequest) (any, error) {
	return apiDeployment(h.Backend.Promote(ctx, actor, request.AgentName, request.Version))
}

func (h *StudioTrafficHandler) rollback(ctx context.Context, actor domain.StudioActor, request api.AgentTrafficRequest) (any, error) {
	return apiDeployment(h.Backend.Rollback(ctx, actor, request.AgentName))
}

func (h *StudioTrafficHandler) explain(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	explained, err := h.Backend.Explain(ctx, actor, query.Get("conversation_id"))
	if err != nil {
		return nil, err
	}
	return api.ConversationVersion{
		ConversationID: explained.ConversationID, AgentName: explained.AgentID, AgentType: explained.AliasID,
		Version: explained.Version, Source: string(explained.Source), PinReason: explained.PinReason,
		DefinitionDigest: explained.DefinitionDigest, StartedAt: explained.StartedAt,
	}, nil
}

func apiDeployment(deployment domain.AgentDeployment, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return api.AgentDeployment{
		AgentName: deployment.AgentID, StableVersion: deployment.StableVersion,
		CanaryVersion: deployment.CanaryVersion, CanaryPercentage: deployment.CanaryPercentage,
	}, nil
}
