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

// StudioFeaturePilot is the Studio feature StudioPilotHandler serves.
const StudioFeaturePilot = "pilot"

// StudioPilotHandler serves the people and positions an agent's supervised pilots list.
type StudioPilotHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioPilotBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioPilotHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeaturePilot, Version: 1, Actions: []string{string(domain.StudioView), string(domain.StudioPublish)}}
}

// Routes returns the pilot routes.
func (h *StudioPilotHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioPilotPath:       query(&h.AbstractHandler, h.list),
		api.StudioPilotAddPath:    command(&h.AbstractHandler, h.add),
		api.StudioPilotRemovePath: command(&h.AbstractHandler, h.remove),
	}
}

func (h *StudioPilotHandler) list(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	members, err := h.Backend.List(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	items := make([]api.PilotMember, 0, len(members))
	for _, member := range members {
		items = append(items, apiPilotMember(member))
	}
	return items, nil
}

func (h *StudioPilotHandler) add(ctx context.Context, actor domain.StudioActor, request api.PilotChangeRequest) (any, error) {
	return pilotResult(h.Backend.Add(ctx, actor, domainPilotChange(request)))
}

func (h *StudioPilotHandler) remove(ctx context.Context, actor domain.StudioActor, request api.PilotChangeRequest) (any, error) {
	return pilotResult(h.Backend.Remove(ctx, actor, domainPilotChange(request)))
}

func domainPilotChange(request api.PilotChangeRequest) domain.PilotChange {
	return domain.PilotChange{
		AgentID: request.AgentName, Version: request.Version, Reason: request.Reason,
		Subject: domain.PilotSubject{Kind: domain.PilotSubjectKind(request.SubjectKind), ID: request.SubjectID},
	}
}

func pilotResult(member domain.PilotMember, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return apiPilotMember(member), nil
}

func apiPilotMember(member domain.PilotMember) api.PilotMember {
	item := api.PilotMember{
		AgentName: member.AgentID, Version: member.Version, SubjectKind: string(member.Subject.Kind), SubjectID: member.Subject.ID,
		Reason: member.Reason, AddedByKind: string(member.AddedBy.Kind), AddedByID: member.AddedBy.ID, ValidFrom: member.ValidFrom,
	}
	if !member.ValidTo.IsZero() {
		validTo := member.ValidTo
		item.ValidTo = &validTo
	}
	return item
}
