package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/nauticana/keel/common"
	keelhandler "github.com/nauticana/keel/handler"
	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

// StudioFeatureAgents is the core Studio feature this handler serves.
const StudioFeatureAgents = "agents"

// StudioHandler maps authenticated studio-v2 HTTP calls to one backend method. It
// only authenticates: the backend authorizes each call at the agent's scope.
type StudioHandler struct {
	keelhandler.AbstractHandler
	Backend       contract.AgentStudioHTTPBackend
	MapTestResult func(domain.AgentTestResult) api.AgentTestResult
	MapModel      func(domain.StudioModel) api.StudioModel
	// Features lists the optional Studio APIs the product mounted beside this handler.
	Features []api.StudioFeature
}

// Routes returns the Agent Studio HTTP handlers.
func (h *StudioHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioAgentsPath:     query(&h.AbstractHandler, h.listAgents),
		api.StudioAgentPath:      query(&h.AbstractHandler, h.getDraft),
		api.StudioHistoryPath:    query(&h.AbstractHandler, h.history),
		api.StudioAuditPath:      query(&h.AbstractHandler, h.auditLog),
		api.StudioSectionsPath:   query(&h.AbstractHandler, h.releaseSections),
		api.StudioModelsPath:     query(&h.AbstractHandler, h.models),
		api.StudioFeaturesPath:   query(&h.AbstractHandler, h.features),
		api.StudioSettingsPath:   query(&h.AbstractHandler, h.settings),
		api.StudioSettingPath:    command(&h.AbstractHandler, h.saveSetting),
		api.StudioCreatePath:     command(&h.AbstractHandler, h.create),
		api.StudioDraftPath:      command(&h.AbstractHandler, h.saveDraft),
		api.StudioEnabledPath:    command(&h.AbstractHandler, h.setEnabled),
		api.StudioRenamePath:     command(&h.AbstractHandler, h.rename),
		api.StudioArchivePath:    command(&h.AbstractHandler, h.archive),
		api.StudioUnarchivePath:  command(&h.AbstractHandler, h.unarchive),
		api.StudioTestPath:       command(&h.AbstractHandler, h.testDraft),
		api.StudioPublishPath:    command(&h.AbstractHandler, h.publish),
		api.StudioRestorePath:    command(&h.AbstractHandler, h.restore),
		api.StudioResetPath:      command(&h.AbstractHandler, h.reset),
		api.StudioSetDefaultPath: command(&h.AbstractHandler, h.setDefault),
	}
}

func (h *StudioHandler) features(context.Context, domain.StudioActor, url.Values) (any, error) {
	core := api.StudioFeature{ID: StudioFeatureAgents, Version: 2, Actions: []string{
		string(domain.StudioView), string(domain.StudioCreate), string(domain.StudioEdit), string(domain.StudioTest),
		string(domain.StudioPublish), string(domain.StudioRestore), string(domain.StudioArchive),
	}}
	return append([]api.StudioFeature{core}, h.Features...), nil
}

func (h *StudioHandler) listAgents(ctx context.Context, actor domain.StudioActor, _ url.Values) (any, error) {
	items, err := h.Backend.ListAgents(ctx, actor)
	return summaries(items), err
}

func (h *StudioHandler) getDraft(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	result, err := h.Backend.GetDraft(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	return apiDraft(result), nil
}

func (h *StudioHandler) settings(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	return apiSettings(h.Backend.Settings(ctx, actor, query.Get("agent_name")))
}

func (h *StudioHandler) saveSetting(ctx context.Context, actor domain.StudioActor, request api.AgentSettingChange) (any, error) {
	return apiSettings(h.Backend.SaveSetting(ctx, actor, domain.AgentSettingChange{
		AgentID: request.AgentName, ScopeID: request.ScopeID, ResourceKind: domain.ResourceKind(request.ResourceKind),
		ResourceID: request.ResourceID, MergeMode: domain.MergeMode(request.MergeMode), Sealed: request.Sealed,
		Value: request.Value, Remove: request.Remove, ExpectedDraftRevision: request.ExpectedAgentRevision,
		ExpectedPromptProfileRevision: request.ExpectedTypeDefaultsRevision,
	}))
}

func apiSettings(settings domain.AgentSettings, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	result := api.AgentSettings{
		AgentName: settings.AgentID, AgentScopeID: settings.Scopes.AgentScopeID, TypeScopeID: settings.Scopes.TypeScopeID,
		Problem: settings.Problem, ExpectedAgentRevision: settings.DraftRevision, ExpectedTypeDefaultsRevision: settings.PromptProfileRevision,
		Bindings: make([]api.AgentSetting, 0, len(settings.Bindings)), Effective: make([]api.AgentEffectiveSetting, 0, len(settings.Effective)),
	}
	for _, binding := range settings.Bindings {
		result.Bindings = append(result.Bindings, api.AgentSetting{
			ScopeID: binding.ScopeID, ScopeKind: binding.ScopeKind, Editable: binding.Editable,
			ResourceKind: string(binding.ResourceKind), ResourceID: binding.ResourceID, ResourceVersion: binding.ResourceVersion,
			MergeMode: string(binding.MergeMode), Sealed: binding.Sealed, Value: binding.Value,
		})
	}
	for _, effective := range settings.Effective {
		item := api.AgentEffectiveSetting{
			ResourceKind: string(effective.ResourceKind), ResourceID: effective.ResourceID, Value: effective.Value,
			Source: settingSource(effective.Winner),
		}
		for _, superseded := range effective.Superseded {
			item.Superseded = append(item.Superseded, settingSource(superseded))
		}
		result.Effective = append(result.Effective, item)
	}
	return result, nil
}

func settingSource(source domain.Provenance) api.AgentSettingSource {
	return api.AgentSettingSource{
		ScopeID: source.ScopeID, ScopeKind: source.ScopeKind, ResourceVersion: source.ResourceVersion,
		MergeMode: string(source.MergeMode), Sealed: source.Sealed,
	}
}

func (h *StudioHandler) history(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	result, err := h.Backend.History(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	items := make([]api.AgentRelease, 0, len(result))
	for _, release := range result {
		item, mapErr := apiRelease(release)
		if mapErr != nil {
			return nil, mapErr
		}
		items = append(items, item)
	}
	return items, nil
}

func (h *StudioHandler) auditLog(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	result, err := h.Backend.AuditLog(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	items := make([]api.AgentAuditEvent, 0, len(result))
	for _, event := range result {
		item := api.AgentAuditEvent{Event: event.Event, Detail: event.Detail, ActorKind: string(event.Actor.Kind),
			ActorID: event.Actor.ID, EventTime: event.OccurredAt}
		if event.Actor.Kind == domain.PrincipalHuman {
			item.UserID, _ = strconv.ParseInt(event.Actor.ID, 10, 64)
		}
		items = append(items, item)
	}
	return items, nil
}

func (h *StudioHandler) releaseSections(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	version := query.Get("version")
	if parsed, err := strconv.ParseInt(version, 10, 64); err != nil || parsed <= 0 {
		return nil, badRequest("numeric version required")
	}
	result, err := h.Backend.ReleaseSections(ctx, actor, query.Get("agent_name"), version)
	if err != nil {
		return nil, err
	}
	items := make([]api.AgentReleaseSection, 0, len(result))
	for _, section := range result {
		items = append(items, api.AgentReleaseSection{
			LanguageCode: section.LanguageCode, PromptHeaderID: section.PromptSectionID,
			Caption: section.Caption, Description: section.Description, Instruction: section.Instruction,
			Output: section.Output, Sequence: section.Sequence,
			Source: api.AgentPromptSource{
				ScopeID: section.Source.ScopeID, ScopeKind: section.Source.ScopeKind,
				MergeMode: string(section.Source.MergeMode), Sealed: section.Source.Sealed,
			},
		})
	}
	return items, nil
}

func (h *StudioHandler) models(ctx context.Context, actor domain.StudioActor, _ url.Values) (any, error) {
	result, err := h.Backend.Models(ctx, actor)
	if err != nil {
		return nil, err
	}
	items := make([]api.StudioModel, 0, len(result))
	for _, item := range result {
		mapped := api.StudioModel{
			ID: item.Reference.ModelID, Provider: item.Reference.ProviderID,
			ModelType: modelType(item.Capabilities), DisplayName: item.DisplayName,
		}
		if h.MapModel != nil {
			mapped = h.MapModel(item)
		}
		items = append(items, mapped)
	}
	return items, nil
}

func (h *StudioHandler) create(ctx context.Context, actor domain.StudioActor, request api.AgentCreateRequest) (any, error) {
	result, err := h.Backend.Create(ctx, actor, domain.AgentCreateRequest{
		AgentTypeID: request.AgentType, DisplayName: request.DisplayName, CopyFrom: request.CopyFrom,
		TemplateID: request.TemplateID, IdempotencyKey: request.IdempotencyKey,
		TemplateVersion: request.TemplateVersion, References: domainReferences(request.References),
	})
	return apiSummary(result), err
}

func (h *StudioHandler) saveDraft(ctx context.Context, actor domain.StudioActor, request api.AgentDraft) (any, error) {
	result, err := h.Backend.SaveDraft(ctx, actor, domainDraft(request))
	return apiDraft(result), err
}

func (h *StudioHandler) setEnabled(ctx context.Context, actor domain.StudioActor, request api.AgentSetEnabledRequest) (any, error) {
	result, err := h.Backend.SetEnabled(ctx, actor, domain.AgentSetEnabledRequest{
		AgentID: request.AgentName, Enabled: request.Enabled, Reason: request.Reason, ExpectedDraftRevision: request.ExpectedAgentRevision,
	})
	return api.AgentEnabledState{Enabled: result.Enabled, ExpectedAgentRevision: result.DraftRevision}, err
}

func (h *StudioHandler) rename(ctx context.Context, actor domain.StudioActor, request api.AgentRenameRequest) (any, error) {
	result, err := h.Backend.Rename(ctx, actor, domain.AgentRenameRequest{
		AgentID: request.AgentName, DisplayName: request.DisplayName, ExpectedDraftRevision: request.ExpectedAgentRevision,
	})
	return apiSummary(result), err
}

func (h *StudioHandler) archive(ctx context.Context, actor domain.StudioActor, request api.AgentArchiveRequest) (any, error) {
	result, err := h.Backend.Archive(ctx, actor, domain.AgentArchiveRequest{AgentID: request.AgentName, ExpectedDraftRevision: request.ExpectedAgentRevision})
	return apiSummary(result), err
}

func (h *StudioHandler) unarchive(ctx context.Context, actor domain.StudioActor, request api.AgentArchiveRequest) (any, error) {
	result, err := h.Backend.Unarchive(ctx, actor, domain.AgentArchiveRequest{AgentID: request.AgentName, ExpectedDraftRevision: request.ExpectedAgentRevision})
	return apiSummary(result), err
}

func (h *StudioHandler) testDraft(ctx context.Context, actor domain.StudioActor, request api.AgentTestRequest) (any, error) {
	result, err := h.Backend.TestDraft(ctx, actor, domain.AgentTestRequest{
		AgentID: request.AgentName, LanguageCode: request.LanguageCode, Task: request.Task, InputData: request.InputData,
	})
	if err != nil {
		return nil, err
	}
	if h.MapTestResult != nil {
		return h.MapTestResult(result), nil
	}
	return api.AgentTestResult{
		AgentName: result.AgentID, LanguageCode: result.LanguageCode, Model: result.Model.ModelID,
		Digest: result.Digest, Output: result.Output, LatencyMs: result.LatencyMs,
		InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
		Credits: result.Usage.CostMinorUnits, Sections: result.Sections,
	}, nil
}

func (h *StudioHandler) publish(ctx context.Context, actor domain.StudioActor, request api.AgentPublishRequest) (any, error) {
	publish := domain.AgentPublishRequest{
		AgentID: request.AgentName, ChangeSummary: request.ChangeSummary, CanaryPercentage: request.CanaryPercentage,
		ExpectedDraftRevision:         request.ExpectedAgentRevision,
		ExpectedPromptProfileRevision: request.ExpectedTypeDefaultsRevision,
	}
	if request.EffectiveAt != nil {
		publish.EffectiveAt = *request.EffectiveAt
	}
	result, err := h.Backend.Publish(ctx, actor, publish)
	if err != nil {
		return nil, err
	}
	return apiRelease(result)
}

func (h *StudioHandler) restore(ctx context.Context, actor domain.StudioActor, request api.AgentRestoreRequest) (any, error) {
	if request.Version <= 0 {
		return nil, badRequest("positive version required")
	}
	result, err := h.Backend.Restore(ctx, actor, domain.AgentRestoreRequest{AgentID: request.AgentName, Version: strconv.FormatInt(request.Version, 10)})
	if err != nil {
		return nil, err
	}
	return apiRelease(result)
}

func (h *StudioHandler) reset(ctx context.Context, actor domain.StudioActor, request api.AgentResetRequest) (any, error) {
	result, err := h.Backend.Reset(ctx, actor, domain.AgentResetRequest{
		AgentID: request.AgentName, Scope: domain.PromptResetScope(request.Scope), PromptSectionID: request.PromptHeaderID,
		LanguageCode: request.LanguageCode, ExpectedDraftRevision: request.ExpectedAgentRevision,
		ExpectedPromptProfileRevision: request.ExpectedTypeDefaultsRevision,
	})
	return apiDraft(result), err
}

func (h *StudioHandler) setDefault(ctx context.Context, actor domain.StudioActor, request api.AgentSetDefaultRequest) (any, error) {
	result, err := h.Backend.SetDefault(ctx, actor, domain.AgentSetDefaultRequest{
		AgentID: request.AgentName, ExpectedAliasRevision: request.ExpectedTypeDefaultsRevision,
	})
	return summaries(result), err
}

// command serves one authenticated JSON POST: the body decodes into R and the backend decides.
func command[R any](h *keelhandler.AbstractHandler, call func(context.Context, domain.StudioActor, R) (any, error)) http.HandlerFunc {
	return bounded(h, h.JSON(http.MethodPost, func(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
		actor, err := sessionActor(session)
		if err != nil {
			return nil, err
		}
		var request R
		if err = decode(body, &request); err != nil {
			return nil, err
		}
		return mapServiceError(call(ctx, actor, request))
	}))
}

// query serves one authenticated GET with its URL parameters.
func query(h *keelhandler.AbstractHandler, call func(context.Context, domain.StudioActor, url.Values) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.RequireMethod(w, r, http.MethodGet) {
			return
		}
		session, ok := h.RequireSession(w, r)
		if !ok {
			return
		}
		actor, err := sessionActor(session)
		var result any
		if err == nil {
			result, err = call(r.Context(), actor, r.URL.Query())
		}
		result, err = mapServiceError(result, err)
		if err == nil {
			common.WriteJSON(w, http.StatusOK, result)
			return
		}
		var apiErr *keelhandler.APIError
		if errors.As(err, &apiErr) {
			for key, values := range apiErr.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			h.WriteRequestError(r, w, apiErr.Status, http.StatusText(apiErr.Status), apiErr.Msg)
			return
		}
		h.WriteRequestError(r, w, http.StatusInternalServerError, "Internal Server Error", err.Error())
	}
}

// withErrorHeaders carries a typed error's advisory headers onto the API error
// that replaces it, so the status mapping does not discard them.
func withErrorHeaders(apiErr *keelhandler.APIError, err error) *keelhandler.APIError {
	var carrier keelhandler.HeaderCarrier
	if !errors.As(err, &carrier) {
		return apiErr
	}
	for key, values := range carrier.ErrorHeaders() {
		for _, value := range values {
			apiErr = apiErr.WithHeader(key, value)
		}
	}
	return apiErr
}

// mapServiceError maps a typed service error onto its HTTP status; an unclassified
// error stays a 500, whose detail keel replaces with a request id.
func mapServiceError(result any, err error) (any, error) {
	if err == nil {
		return result, nil
	}
	var validation *controlplane.StudioValidationError
	if errors.As(err, &validation) {
		fields := make([]api.AgentFieldError, 0, len(validation.Fields))
		for _, field := range validation.Fields {
			fields = append(fields, api.AgentFieldError{Field: field.Field, Message: field.Message})
		}
		body, marshalErr := json.Marshal(api.ValidationProblem{Message: domain.ErrValidation.Error(), Fields: fields})
		if marshalErr != nil {
			return nil, keelhandler.NewAPIError(http.StatusBadRequest, domain.ErrValidation.Error())
		}
		return nil, keelhandler.NewAPIError(http.StatusBadRequest, string(body))
	}
	switch {
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrNoPrompts):
		return nil, keelhandler.NewAPIError(http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrPrincipalUnknown):
		return nil, keelhandler.NewAPIError(http.StatusUnauthorized, err.Error())
	case errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrAuthorityExceeded):
		return nil, keelhandler.NewAPIError(http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return nil, keelhandler.NewAPIError(http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrRevisionConflict), errors.Is(err, domain.ErrSealed):
		return nil, keelhandler.NewAPIError(http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrRateLimited), errors.Is(err, domain.ErrBudgetExceeded):
		return nil, withErrorHeaders(keelhandler.NewAPIError(http.StatusTooManyRequests, err.Error()), err)
	case errors.Is(err, domain.ErrNotReady), errors.Is(err, domain.ErrCircuitOpen):
		return nil, withErrorHeaders(keelhandler.NewAPIError(http.StatusServiceUnavailable, err.Error()), err)
	case errors.Is(err, domain.ErrExecutionLimit):
		// The agent's own configured limit stopped the work; the detail may hold provider text.
		return nil, keelhandler.NewAPIError(http.StatusUnprocessableEntity, domain.ErrExecutionLimit.Error())
	default:
		return nil, err
	}
}

func domainDraft(value api.AgentDraft) domain.AgentDraft {
	draft := domain.AgentDraft{
		AgentID: value.AgentName, AgentTypeID: value.AgentType, DisplayName: value.DisplayName,
		Active: value.Enabled, Enabled: value.Enabled, Default: value.Default,
		Models: domain.AgentModelSelection{
			Text: legacyModel(value.Models.TextModel), Image: legacyModel(value.Models.ImageModel), Video: legacyModel(value.Models.VideoModel),
		},
		ExpectedDraftRevision:         value.ExpectedAgentRevision,
		ExpectedPromptProfileRevision: value.ExpectedTypeDefaultsRevision,
	}
	for _, language := range value.Languages {
		item := domain.AgentLanguageDraft{LanguageCode: language.LanguageCode}
		for _, section := range language.PromptSections {
			mapped := domain.AgentPromptSection{
				PromptSectionID: section.PromptHeaderID, Caption: section.Caption, Description: section.Description,
				Effective: domain.PromptValue{Instruction: section.EffectiveText, Output: section.EffectiveOutput},
			}
			for _, layer := range section.Layers {
				mapped.Layers = append(mapped.Layers, domain.PromptLayer{
					ScopeID: layer.ScopeID, ScopeKind: layer.ScopeKind, MergeMode: domain.MergeMode(layer.MergeMode),
					Sealed: layer.Sealed, Instruction: layer.Instruction, Output: layer.Output,
				})
			}
			item.Sections = append(item.Sections, mapped)
		}
		draft.Languages = append(draft.Languages, item)
	}
	return draft
}

func apiDraft(value domain.AgentDraft) api.AgentDraft {
	draft := api.AgentDraft{
		AgentType: value.AgentTypeID, AgentName: value.AgentID, DisplayName: value.DisplayName,
		Enabled: value.Enabled && value.Active, Default: value.Default,
		Models:                       api.AgentModelSelection{TextModel: referenceID(value.Models.Text), ImageModel: referenceID(value.Models.Image), VideoModel: referenceID(value.Models.Video)},
		ExpectedAgentRevision:        value.ExpectedDraftRevision,
		ExpectedTypeDefaultsRevision: value.ExpectedPromptProfileRevision,
		AgentScopeID:                 value.PromptScopes.AgentScopeID,
		TypeScopeID:                  value.PromptScopes.TypeScopeID,
	}
	if value.Drift != nil {
		version, _ := strconv.ParseInt(value.Drift.ActiveVersion, 10, 64)
		draft.Drift = &api.AgentDrift{ActiveVersion: version, ChangedLanguages: value.Drift.ChangedLanguages, Causes: value.Drift.Causes}
	}
	for _, language := range value.Languages {
		item := api.AgentLanguageDraft{LanguageCode: language.LanguageCode}
		for _, section := range language.Sections {
			mapped := api.AgentPromptSection{
				PromptHeaderID: section.PromptSectionID, Caption: section.Caption, Description: section.Description,
				EffectiveText: section.Effective.Instruction, EffectiveOutput: section.Effective.Output,
				Layers: make([]api.AgentPromptLayer, 0, len(section.Layers)),
			}
			for _, layer := range section.Layers {
				mapped.Layers = append(mapped.Layers, api.AgentPromptLayer{
					ScopeID: layer.ScopeID, ScopeKind: layer.ScopeKind, MergeMode: string(layer.MergeMode),
					Sealed: layer.Sealed, Editable: layer.Editable, Instruction: layer.Instruction, Output: layer.Output,
					BoundBy: layer.BoundBy,
				})
			}
			item.PromptSections = append(item.PromptSections, mapped)
		}
		draft.Languages = append(draft.Languages, item)
	}
	return draft
}

func summaries(items []domain.AgentSummary) []api.AgentSummary {
	result := make([]api.AgentSummary, 0, len(items))
	for _, item := range items {
		result = append(result, apiSummary(item))
	}
	return result
}

func apiSummary(value domain.AgentSummary) api.AgentSummary {
	version, _ := strconv.ParseInt(value.PublishedVersion, 10, 64)
	return api.AgentSummary{
		AgentType: value.AgentTypeID, AgentName: value.AgentID, DisplayName: value.DisplayName,
		Purpose: value.Purpose, Enabled: value.Active && value.Enabled, Default: value.Default, Archived: value.Archived,
		Readiness: string(value.Readiness), ReadinessReason: value.ReadinessReason,
		TypeDefaultsRevision: value.PromptProfileRevision, AgentRevision: value.DraftRevision,
		PublishedVersion: version, PublishedAt: value.PublishedAt, LastRunAt: value.LastRunAt,
	}
}

func apiRelease(value domain.AgentRelease) (api.AgentRelease, error) {
	version, err := strconv.ParseInt(value.Version, 10, 64)
	if err != nil {
		return api.AgentRelease{}, keelhandler.NewAPIError(http.StatusInternalServerError, "release version is not studio-v2 compatible")
	}
	publishedBy := int64(0)
	if value.PublishedBy != nil {
		publishedBy = *value.PublishedBy
	}
	release := api.AgentRelease{
		AgentName: value.AgentID, AgentType: value.AgentTypeID, Version: version, Enabled: value.Enabled,
		Models:           api.AgentModelSelection{TextModel: referenceID(value.Models.Text), ImageModel: referenceID(value.Models.Image), VideoModel: referenceID(value.Models.Video)},
		DefinitionDigest: value.DefinitionDigest,
		ChangeSummary:    value.ChangeSummary, PublishedBy: publishedBy, PublishedAt: value.PublishedAt,
		Active: value.Active, Languages: value.Languages,
	}
	if value.Activation != nil {
		activation, err := apiActivation(*value.Activation)
		if err != nil {
			return api.AgentRelease{}, err
		}
		release.Activation = &activation
	}
	return release, nil
}

func sessionActor(session *keelmodel.UserSession) (domain.StudioActor, error) {
	if session == nil || session.PartnerId <= 0 {
		return domain.StudioActor{}, keelhandler.NewAPIError(http.StatusForbidden, "partner context required")
	}
	if session.Id <= 0 {
		return domain.StudioActor{}, keelhandler.NewAPIError(http.StatusUnauthorized, "user session required")
	}
	return domain.StudioActor{TenantID: session.PartnerId, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: strconv.Itoa(session.Id)}}, nil
}

func decode(body json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if len(body) == 0 || decoder.Decode(target) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return keelhandler.NewAPIError(http.StatusBadRequest, "invalid JSON request body")
	}
	return nil
}

func badRequest(message string) error {
	return keelhandler.NewAPIError(http.StatusBadRequest, message)
}

func legacyModel(id string) *domain.ModelReference {
	if id == "" {
		return nil
	}
	return &domain.ModelReference{ModelID: id}
}

func referenceID(value *domain.ModelReference) string {
	if value == nil {
		return ""
	}
	return value.ModelID
}

func modelType(capabilities []string) string {
	for _, capability := range capabilities {
		switch capability {
		case "text":
			return "T"
		case "image":
			return "I"
		case "video":
			return "V"
		}
	}
	return ""
}
