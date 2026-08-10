package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/nauticana/keel/common"
	keelhandler "github.com/nauticana/keel/handler"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

// StudioHandler maps authenticated studio-v1 HTTP calls to one backend method.
type StudioHandler struct {
	keelhandler.AbstractHandler
	DB            keelport.DatabaseRepository
	Backend       contract.AgentStudioHTTPBackend
	MapTestResult func(domain.AgentTestResult) api.AgentTestResult
	MapModel      func(domain.StudioModel) api.StudioModel
}

// Routes returns authorized Agent Studio HTTP handlers.
func (h *StudioHandler) Routes() map[string]http.HandlerFunc {
	guard := func(action string, inner http.HandlerFunc) http.HandlerFunc {
		return keelhandler.WrapTableAction(h.DB, h.UserService, "AGENT_STUDIO", action, "agent_studio", inner)
	}
	return map[string]http.HandlerFunc{
		api.StudioAgentsPath:     guard("VIEW", h.JSON(http.MethodGet, h.listAgents)),
		api.StudioAgentPath:      guard("VIEW", h.getDraft),
		api.StudioDraftPath:      guard("EDIT", h.JSON(http.MethodPost, h.saveDraft)),
		api.StudioEnabledPath:    guard("EDIT", h.JSON(http.MethodPost, h.setEnabled)),
		api.StudioTestPath:       guard("TEST", h.JSON(http.MethodPost, h.testDraft)),
		api.StudioPublishPath:    guard("PUBLISH", h.JSON(http.MethodPost, h.publish)),
		api.StudioRestorePath:    guard("RESTORE", h.JSON(http.MethodPost, h.restore)),
		api.StudioResetPath:      guard("EDIT", h.JSON(http.MethodPost, h.reset)),
		api.StudioSetDefaultPath: guard("PUBLISH", h.JSON(http.MethodPost, h.setDefault)),
		api.StudioHistoryPath:    guard("VIEW", h.history),
		api.StudioAuditPath:      guard("VIEW", h.auditLog),
		api.StudioSectionsPath:   guard("VIEW", h.releaseSections),
		api.StudioModelsPath:     guard("VIEW", h.JSON(http.MethodGet, h.models)),
	}
}

func (h *StudioHandler) listAgents(ctx context.Context, session *keelmodel.UserSession, _ json.RawMessage) (any, error) {
	tenantID, err := sessionTenant(session)
	if err != nil {
		return nil, err
	}
	items, err := h.Backend.ListAgents(ctx, tenantID)
	if err != nil {
		return mapStudioError(nil, err)
	}
	result := make([]api.AgentSummary, 0, len(items))
	for _, item := range items {
		result = append(result, apiSummary(item))
	}
	return result, nil
}

func (h *StudioHandler) saveDraft(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	var request api.AgentDraft
	if err = decode(body, &request); err != nil {
		return nil, err
	}
	if request.AgentName == "" {
		return nil, badRequest("agent_name required")
	}
	result, err := h.Backend.SaveDraft(ctx, actor, domainDraft(request))
	if err != nil {
		return mapStudioError(nil, err)
	}
	return apiDraft(result), nil
}

func (h *StudioHandler) setEnabled(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	var request api.AgentSetEnabledRequest
	if err = decode(body, &request); err != nil {
		return nil, err
	}
	if request.AgentName == "" {
		return nil, badRequest("agent_name required")
	}
	result, err := h.Backend.SetEnabled(ctx, actor, domain.AgentSetEnabledRequest{
		AgentID: request.AgentName, Enabled: request.Enabled, ExpectedDraftRevision: request.ExpectedAgentRevision,
	})
	if err != nil {
		return mapStudioError(nil, err)
	}
	return api.AgentEnabledState{Enabled: result.Enabled, ExpectedAgentRevision: result.DraftRevision}, nil
}

func (h *StudioHandler) testDraft(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	var request api.AgentTestRequest
	if err = decode(body, &request); err != nil {
		return nil, err
	}
	if request.AgentName == "" {
		return nil, badRequest("agent_name required")
	}
	result, err := h.Backend.TestDraft(ctx, actor, domain.AgentTestRequest{
		AgentID: request.AgentName, LanguageCode: request.LanguageCode, Task: request.Task, InputData: request.InputData,
	})
	if err != nil {
		return mapStudioError(nil, err)
	}
	mapped := api.AgentTestResult{
		AgentName: result.AgentID, LanguageCode: result.LanguageCode, Model: result.Model.ModelID,
		Digest: result.Digest, Output: result.Output, LatencyMs: result.LatencyMs,
		InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
		Credits: result.Usage.CostMinorUnits, Sections: result.Sections,
	}
	if h.MapTestResult != nil {
		mapped = h.MapTestResult(result)
	}
	return mapped, nil
}

func (h *StudioHandler) publish(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	var request api.AgentPublishRequest
	if err = decode(body, &request); err != nil {
		return nil, err
	}
	if request.AgentName == "" {
		return nil, badRequest("agent_name required")
	}
	result, err := h.Backend.Publish(ctx, actor, domain.AgentPublishRequest{
		AgentID: request.AgentName, ChangeSummary: request.ChangeSummary,
		ExpectedDraftRevision:         request.ExpectedAgentRevision,
		ExpectedPromptProfileRevision: request.ExpectedTypeDefaultsRevision,
	})
	if err != nil {
		return mapStudioError(nil, err)
	}
	return apiRelease(result)
}

func (h *StudioHandler) restore(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	var request api.AgentRestoreRequest
	if err = decode(body, &request); err != nil {
		return nil, err
	}
	if request.AgentName == "" || request.Version <= 0 {
		return nil, badRequest("agent_name and version required")
	}
	result, err := h.Backend.Restore(ctx, actor, domain.AgentRestoreRequest{AgentID: request.AgentName, Version: strconv.FormatInt(request.Version, 10)})
	if err != nil {
		return mapStudioError(nil, err)
	}
	return apiRelease(result)
}

func (h *StudioHandler) reset(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	var request api.AgentResetRequest
	if err = decode(body, &request); err != nil {
		return nil, err
	}
	if request.AgentName == "" || request.Scope == "" {
		return nil, badRequest("agent_name and scope required")
	}
	result, err := h.Backend.Reset(ctx, actor, domain.AgentResetRequest{
		AgentID: request.AgentName, Scope: domain.PromptResetScope(request.Scope), PromptSectionID: request.PromptHeaderID,
		LanguageCode: request.LanguageCode, ExpectedDraftRevision: request.ExpectedAgentRevision,
		ExpectedPromptProfileRevision: request.ExpectedTypeDefaultsRevision,
	})
	if err != nil {
		return mapStudioError(nil, err)
	}
	return apiDraft(result), nil
}

func (h *StudioHandler) setDefault(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	var request api.AgentSetDefaultRequest
	if err = decode(body, &request); err != nil {
		return nil, err
	}
	if request.AgentName == "" {
		return nil, badRequest("agent_name required")
	}
	result, err := h.Backend.SetDefault(ctx, actor, domain.AgentSetDefaultRequest{
		AgentID: request.AgentName, ExpectedAliasRevision: request.ExpectedTypeDefaultsRevision,
	})
	if err != nil {
		return mapStudioError(nil, err)
	}
	items := make([]api.AgentSummary, 0, len(result))
	for _, item := range result {
		items = append(items, apiSummary(item))
	}
	return items, nil
}

func (h *StudioHandler) models(ctx context.Context, session *keelmodel.UserSession, _ json.RawMessage) (any, error) {
	tenantID, err := sessionTenant(session)
	if err != nil {
		return nil, err
	}
	result, err := h.Backend.Models(ctx, tenantID)
	if err != nil {
		return mapStudioError(nil, err)
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

func (h *StudioHandler) getDraft(w http.ResponseWriter, r *http.Request) {
	h.agentQuery(w, r, func(ctx context.Context, tenantID int64, agentID string) (any, error) {
		result, err := h.Backend.GetDraft(ctx, tenantID, agentID)
		if err != nil {
			return nil, err
		}
		return apiDraft(result), nil
	})
}

func (h *StudioHandler) history(w http.ResponseWriter, r *http.Request) {
	h.agentQuery(w, r, func(ctx context.Context, tenantID int64, agentID string) (any, error) {
		result, err := h.Backend.History(ctx, tenantID, agentID)
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
	})
}

func (h *StudioHandler) auditLog(w http.ResponseWriter, r *http.Request) {
	h.agentQuery(w, r, func(ctx context.Context, tenantID int64, agentID string) (any, error) {
		result, err := h.Backend.AuditLog(ctx, tenantID, agentID)
		if err != nil {
			return nil, err
		}
		items := make([]api.AgentAuditEvent, 0, len(result))
		for _, event := range result {
			item := api.AgentAuditEvent{Event: event.Event, Detail: event.Detail, EventTime: event.OccurredAt}
			if event.ActorID != nil {
				item.UserID = *event.ActorID
			}
			items = append(items, item)
		}
		return items, nil
	})
}

func (h *StudioHandler) releaseSections(w http.ResponseWriter, r *http.Request) {
	h.agentQuery(w, r, func(ctx context.Context, tenantID int64, agentID string) (any, error) {
		version := r.URL.Query().Get("version")
		parsed, parseErr := strconv.ParseInt(version, 10, 64)
		if parseErr != nil || parsed <= 0 {
			return nil, keelhandler.NewAPIError(http.StatusBadRequest, "numeric version required")
		}
		result, err := h.Backend.ReleaseSections(ctx, tenantID, agentID, version)
		if err != nil {
			return nil, err
		}
		items := make([]api.AgentReleaseSection, 0, len(result))
		for _, section := range result {
			items = append(items, api.AgentReleaseSection{
				LanguageCode: section.LanguageCode, PromptHeaderID: section.PromptSectionID,
				Caption: section.Caption, Description: section.Description, Instruction: section.Instruction,
				Output: section.Output, Sequence: section.Sequence,
			})
		}
		return items, nil
	})
}

func (h *StudioHandler) agentQuery(w http.ResponseWriter, r *http.Request, fn func(context.Context, int64, string) (any, error)) {
	if !h.RequireMethod(w, r, http.MethodGet) {
		return
	}
	session, ok := h.RequireSession(w, r)
	if !ok {
		return
	}
	tenantID, err := sessionTenant(session)
	if err != nil {
		h.write(w, nil, err)
		return
	}
	agentID := r.URL.Query().Get("agent_name")
	if agentID == "" {
		h.WriteError(w, http.StatusBadRequest, "Bad Request", "agent_name required")
		return
	}
	result, err := fn(r.Context(), tenantID, agentID)
	h.write(w, result, err)
}

func (h *StudioHandler) write(w http.ResponseWriter, result any, err error) {
	result, err = mapStudioError(result, err)
	if err == nil {
		common.WriteJSON(w, http.StatusOK, result)
		return
	}
	var apiErr *keelhandler.APIError
	if errors.As(err, &apiErr) {
		h.WriteError(w, apiErr.Status, http.StatusText(apiErr.Status), apiErr.Msg)
		return
	}
	h.WriteError(w, http.StatusInternalServerError, "Internal Server Error", err.Error())
}

func mapStudioError(result any, err error) (any, error) {
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
	case errors.Is(err, domain.ErrUnauthorized):
		return nil, keelhandler.NewAPIError(http.StatusUnauthorized, err.Error())
	case errors.Is(err, domain.ErrForbidden):
		return nil, keelhandler.NewAPIError(http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return nil, keelhandler.NewAPIError(http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrRevisionConflict):
		return nil, keelhandler.NewAPIError(http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrRateLimited), errors.Is(err, domain.ErrBudgetExceeded):
		return nil, keelhandler.NewAPIError(http.StatusTooManyRequests, err.Error())
	case errors.Is(err, domain.ErrNotReady), errors.Is(err, domain.ErrCircuitOpen):
		return nil, keelhandler.NewAPIError(http.StatusServiceUnavailable, err.Error())
	default:
		return result, err
	}
}

func domainDraft(value api.AgentDraft) domain.AgentDraft {
	draft := domain.AgentDraft{
		AgentID: value.AgentName, AgentKind: value.AgentType, DisplayName: value.DisplayName,
		Active: value.Enabled, Enabled: value.Enabled, Default: value.Default,
		ApprovalPolicy: domain.AgentApprovalPolicy{RequireApproval: value.ApprovalPolicy.RequireApproval},
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
				Baseline:  domain.PromptValue{Instruction: section.BusinessText, Output: section.BusinessOutput},
				Effective: domain.PromptValue{Instruction: section.EffectiveText, Output: section.EffectiveOutput},
			}
			if section.DefaultText != nil {
				mapped.TenantDefault = &domain.PromptValue{Instruction: *section.DefaultText, Output: dereference(section.DefaultOutput)}
			}
			if section.OverrideText != nil {
				mapped.AgentOverride = &domain.PromptOverride{
					PromptValue: domain.PromptValue{Instruction: *section.OverrideText, Output: dereference(section.OverrideOutput)},
					Overwrite:   section.Overwrite,
				}
			}
			item.Sections = append(item.Sections, mapped)
		}
		draft.Languages = append(draft.Languages, item)
	}
	return draft
}

func apiDraft(value domain.AgentDraft) api.AgentDraft {
	draft := api.AgentDraft{
		AgentType: value.AgentKind, AgentName: value.AgentID, DisplayName: value.DisplayName,
		Enabled: value.Enabled && value.Active, Default: value.Default,
		ApprovalPolicy:               api.AgentApprovalPolicy{RequireApproval: value.ApprovalPolicy.RequireApproval},
		Models:                       api.AgentModelSelection{TextModel: referenceID(value.Models.Text), ImageModel: referenceID(value.Models.Image), VideoModel: referenceID(value.Models.Video)},
		ExpectedAgentRevision:        value.ExpectedDraftRevision,
		ExpectedTypeDefaultsRevision: value.ExpectedPromptProfileRevision,
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
				BusinessText: section.Baseline.Instruction, BusinessOutput: section.Baseline.Output,
				EffectiveText: section.Effective.Instruction, EffectiveOutput: section.Effective.Output,
			}
			if section.TenantDefault != nil {
				mapped.DefaultText = pointer(section.TenantDefault.Instruction)
				mapped.DefaultOutput = optionalPointer(section.TenantDefault.Output)
			}
			if section.AgentOverride != nil {
				mapped.OverrideText = pointer(section.AgentOverride.Instruction)
				mapped.OverrideOutput = optionalPointer(section.AgentOverride.Output)
				mapped.Overwrite = section.AgentOverride.Overwrite
			}
			item.PromptSections = append(item.PromptSections, mapped)
		}
		draft.Languages = append(draft.Languages, item)
	}
	return draft
}

func apiSummary(value domain.AgentSummary) api.AgentSummary {
	version, _ := strconv.ParseInt(value.PublishedVersion, 10, 64)
	return api.AgentSummary{
		AgentType: value.AgentKind, AgentName: value.AgentID, DisplayName: value.DisplayName,
		Purpose: value.Purpose, Enabled: value.Active && value.Enabled, Default: value.Default,
		Readiness: string(value.Readiness), ReadinessReason: value.ReadinessReason,
		TypeDefaultsRevision: value.PromptProfileRevision, AgentRevision: value.DraftRevision,
		PublishedVersion: version, PublishedAt: value.PublishedAt, LastRunAt: value.LastRunAt,
	}
}

func apiRelease(value domain.AgentRelease) (api.AgentRelease, error) {
	version, err := strconv.ParseInt(value.Version, 10, 64)
	if err != nil {
		return api.AgentRelease{}, keelhandler.NewAPIError(http.StatusInternalServerError, "release version is not studio-v1 compatible")
	}
	publishedBy := int64(0)
	if value.PublishedBy != nil {
		publishedBy = *value.PublishedBy
	}
	return api.AgentRelease{
		AgentName: value.AgentID, AgentType: value.AgentKind, Version: version, Enabled: value.Enabled,
		Models:          api.AgentModelSelection{TextModel: referenceID(value.Models.Text), ImageModel: referenceID(value.Models.Image), VideoModel: referenceID(value.Models.Video)},
		RequireApproval: value.ApprovalPolicy.RequireApproval, DefinitionDigest: value.DefinitionDigest,
		ChangeSummary: value.ChangeSummary, PublishedBy: publishedBy, PublishedAt: value.PublishedAt,
		Active: value.Active, Languages: value.Languages,
	}, nil
}

func sessionTenant(session *keelmodel.UserSession) (int64, error) {
	if session == nil || session.PartnerId <= 0 {
		return 0, keelhandler.NewAPIError(http.StatusForbidden, "partner context required")
	}
	return session.PartnerId, nil
}

func sessionActor(session *keelmodel.UserSession) (domain.StudioActor, error) {
	tenantID, err := sessionTenant(session)
	if err != nil {
		return domain.StudioActor{}, err
	}
	return domain.StudioActor{TenantID: tenantID, ActorID: int64(session.Id)}, nil
}

func decode(body json.RawMessage, target any) error {
	if len(body) == 0 || json.Unmarshal(body, target) != nil {
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

func pointer(value string) *string { return &value }

func optionalPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
