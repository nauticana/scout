package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/url"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeaturePortability is the Studio feature StudioPortabilityHandler serves.
const StudioFeaturePortability = "portability"

// StudioPortabilityHandler exports an agent as a Charter AgentDefinition document and imports one.
type StudioPortabilityHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioPortabilityBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioPortabilityHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeaturePortability, Version: 1, Actions: []string{
		string(domain.StudioView), string(domain.StudioCreate), string(domain.StudioEdit),
	}}
}

// Routes returns GET export?agent_name= and POST import.
func (h *StudioPortabilityHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioExportPath: query(&h.AbstractHandler, h.export),
		api.StudioImportPath: command(&h.AbstractHandler, h.importAgent),
	}
}

func (h *StudioPortabilityHandler) export(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	return h.Backend.Export(ctx, actor, query.Get("agent_name"))
}

func (h *StudioPortabilityHandler) importAgent(ctx context.Context, actor domain.StudioActor, request api.AgentImportRequest) (any, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(request.Document), []byte("{")) {
		return nil, badRequest("document must be a JSON object")
	}
	result, err := h.Backend.Import(ctx, actor, domain.AgentImportRequest{
		Document: request.Document, AgentID: request.AgentName, DisplayName: request.DisplayName,
		IdempotencyKey: request.IdempotencyKey, ExpectedDraftRevision: request.ExpectedAgentRevision,
		References: domainReferences(request.References),
	})
	return apiSummary(result), err
}

// domainReferences keys a knowledge and entitlement mapping by resource kind; the backend refuses other kinds.
func domainReferences(references map[string]map[string]string) map[domain.ResourceKind]map[string]string {
	if len(references) == 0 {
		return nil
	}
	mapped := make(map[domain.ResourceKind]map[string]string, len(references))
	for kind, mapping := range references {
		mapped[domain.ResourceKind(kind)] = mapping
	}
	return mapped
}
