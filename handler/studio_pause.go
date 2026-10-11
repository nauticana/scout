package handler

import (
	"context"
	"net/http"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeaturePause is the Studio feature StudioPauseHandler serves.
const StudioFeaturePause = "pause"

// StudioPauseHandler serves pausing an agent with a policy for its live turns.
type StudioPauseHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioPauseBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioPauseHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeaturePause, Version: 1, Actions: []string{string(domain.StudioEdit)}}
}

// Routes returns the pause route.
func (h *StudioPauseHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{api.StudioPausePath: command(&h.AbstractHandler, h.pause)}
}

func (h *StudioPauseHandler) pause(ctx context.Context, actor domain.StudioActor, request api.AgentPauseRequest) (any, error) {
	result, err := h.Backend.Pause(ctx, actor, domain.AgentPauseRequest{
		AgentID: request.AgentName, Policy: domain.PausePolicy(request.Policy), Reason: request.Reason,
		ExpectedDraftRevision: request.ExpectedAgentRevision,
	})
	if err != nil {
		return nil, err
	}
	return api.AgentPauseResult{ExpectedAgentRevision: result.DraftRevision, Cancelled: result.Cancelled}, nil
}
