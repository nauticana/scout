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

// StudioFeatureTemplates is the Studio feature StudioTemplatesHandler serves.
const StudioFeatureTemplates = "templates"

// StudioTemplatesHandler lists, saves, and shares agent templates and reports what an agent derives
// from. Creating an agent from a template is the core create route with template_id.
type StudioTemplatesHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioTemplatesBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioTemplatesHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureTemplates, Version: 1, Actions: []string{
		string(domain.StudioView), string(domain.StudioCreate), string(domain.StudioEdit),
		string(domain.StudioTemplate), string(domain.StudioTemplatePublish),
	}}
}

// Routes returns GET templates?agent_type=, POST template, POST template/publish, and GET template/status?agent_name=.
func (h *StudioTemplatesHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioTemplatesPath:       query(&h.AbstractHandler, h.list),
		api.StudioTemplatePath:        command(&h.AbstractHandler, h.save),
		api.StudioTemplatePublishPath: command(&h.AbstractHandler, h.publish),
		api.StudioTemplateStatusPath:  query(&h.AbstractHandler, h.status),
	}
}

func (h *StudioTemplatesHandler) list(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	templates, err := h.Backend.ListTemplates(ctx, actor, query.Get("agent_type"))
	if err != nil {
		return nil, err
	}
	items := make([]api.AgentTemplate, 0, len(templates))
	for _, template := range templates {
		items = append(items, apiTemplate(template))
	}
	return items, nil
}

func (h *StudioTemplatesHandler) save(ctx context.Context, actor domain.StudioActor, request api.AgentTemplateSaveRequest) (any, error) {
	template, err := h.Backend.SaveTemplate(ctx, actor, domain.AgentTemplateSaveRequest{
		AgentID: request.AgentName, TemplateID: request.TemplateID, IdempotencyKey: request.IdempotencyKey,
		DisplayName: request.DisplayName, Description: request.Description, ChangeSummary: request.ChangeSummary,
		Slots: domainReferences(request.Slots),
	})
	if err != nil {
		return nil, err
	}
	return apiTemplate(template), nil
}

func (h *StudioTemplatesHandler) publish(ctx context.Context, actor domain.StudioActor, request api.AgentTemplatePublication) (any, error) {
	template, err := h.Backend.PublishTemplate(ctx, actor, domain.AgentTemplatePublication{
		TemplateID: request.TemplateID, Audience: domain.TemplateAudience{All: request.All, Tenants: request.Tenants}, Withdraw: request.Withdraw,
	})
	if err != nil {
		return nil, err
	}
	return apiTemplate(template), nil
}

func (h *StudioTemplatesHandler) status(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	status, err := h.Backend.TemplateStatus(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	return api.AgentTemplateStatus{
		AgentName: status.AgentID, DerivedFrom: apiDerivedFrom(status.DerivedFrom), TemplateID: status.TemplateID,
		TemplateVersion: status.Version, LatestVersion: status.LatestVersion, Newer: status.Newer, Available: status.Available,
	}, nil
}

// apiTemplate shows the source agent and audience only for the caller's own template.
func apiTemplate(template domain.AgentTemplate) api.AgentTemplate {
	latest := template.Latest
	item := api.AgentTemplate{
		TemplateID: latest.TemplateID, Version: latest.Version, Owner: string(latest.Owner), AgentType: latest.AgentTypeID,
		TypeVersion: latest.TypeVersion, DisplayName: latest.DisplayName, Description: latest.Description,
		ChangeSummary: latest.ChangeSummary, Digest: latest.Digest, CreatedAt: latest.CreatedAt,
		Bindings: make([]api.TemplateBinding, 0, len(latest.Payload.Bindings)),
	}
	for kind, names := range latest.Payload.Slots {
		if item.Slots == nil {
			item.Slots = map[string][]string{}
		}
		item.Slots[string(kind)] = append([]string{}, names...)
	}
	for _, binding := range latest.Payload.Bindings {
		item.Bindings = append(item.Bindings, api.TemplateBinding{
			ResourceKind: string(binding.ResourceKind), ResourceID: binding.ResourceID, Caption: binding.Caption,
			Language: binding.Language, MergeMode: string(binding.MergeMode), Sealed: binding.Sealed, Value: binding.Value,
		})
	}
	if latest.Owner == domain.TemplateOwn {
		item.SourceAgent = latest.SourceAgentID
		if template.Audience != nil {
			item.Audience = &api.TemplateAudience{All: template.Audience.All, Tenants: append([]int64{}, template.Audience.Tenants...)}
		}
	}
	return item
}

func apiDerivedFrom(derived *domain.DerivedFrom) *api.DerivedFrom {
	if derived == nil {
		return nil
	}
	return &api.DerivedFrom{
		Source:  api.CharterObjectRef{Kind: derived.Source.Kind, ID: derived.Source.ID, Namespace: derived.Source.Namespace, External: derived.Source.External},
		Version: derived.Version, Digest: derived.Digest,
	}
}
