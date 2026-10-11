package handler

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureActivation is the Studio feature StudioActivationHandler serves.
const StudioFeatureActivation = "activation"

// StudioActivationHandler serves the scheduled publishes of Studio.
type StudioActivationHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioActivationBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioActivationHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureActivation, Version: 1, Actions: []string{string(domain.StudioView), string(domain.StudioPublish)}}
}

// Routes returns the activation routes.
func (h *StudioActivationHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioActivationsPath:      query(&h.AbstractHandler, h.list),
		api.StudioActivationCancelPath: command(&h.AbstractHandler, h.cancel),
	}
}

func (h *StudioActivationHandler) list(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	activations, err := h.Backend.Activations(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	items := make([]api.ReleaseActivation, 0, len(activations))
	for _, activation := range activations {
		item, mapErr := apiActivation(activation)
		if mapErr != nil {
			return nil, mapErr
		}
		items = append(items, item)
	}
	return items, nil
}

func (h *StudioActivationHandler) cancel(ctx context.Context, actor domain.StudioActor, request api.ReleaseActivationCancel) (any, error) {
	if request.ActivationID <= 0 {
		return nil, badRequest("positive activation_id required")
	}
	activation, err := h.Backend.Cancel(ctx, actor, domain.ReleaseActivationCancel{
		AgentID: request.AgentName, ActivationID: request.ActivationID, Reason: request.Reason,
	})
	if err != nil {
		return nil, err
	}
	return apiActivation(activation)
}

func apiActivation(value domain.ReleaseActivation) (api.ReleaseActivation, error) {
	version, err := strconv.ParseInt(value.Version, 10, 64)
	if err != nil {
		return api.ReleaseActivation{}, keelhandler.NewAPIError(http.StatusInternalServerError, "release version is not studio-v2 compatible")
	}
	activation := api.ReleaseActivation{
		ID: value.ID, AgentName: value.AgentID, Version: version, DefinitionDigest: value.DefinitionDigest,
		CanaryPercentage: value.CanaryPercentage, EffectiveAt: value.EffectiveAt, State: string(value.State),
		ScheduledBy: activationActor(value.ScheduledBy), ScheduledAt: value.ScheduledAt, Attempts: value.Attempts,
		NextAttemptAt: value.NextAttemptAt, Reason: string(value.Reason), Detail: value.Detail, DecidedAt: value.DecidedAt,
	}
	if value.DecidedBy != nil {
		activation.DecidedBy = activationActor(*value.DecidedBy)
	}
	return activation, nil
}

func activationActor(ref domain.PrincipalRef) string {
	return string(ref.Kind) + ":" + ref.ID
}
