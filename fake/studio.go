package fake

import (
	"context"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type PromptSources struct {
	ResolveFunc   func(context.Context, int64, string, string) (domain.ResolvedPrompts, error)
	LanguagesFunc func(context.Context, int64, string) ([]string, error)
}

func (f PromptSources) Resolve(ctx context.Context, tenantID int64, agentID, language string) (domain.ResolvedPrompts, error) {
	return f.ResolveFunc(ctx, tenantID, agentID, language)
}

func (f PromptSources) Languages(ctx context.Context, tenantID int64, agentID string) ([]string, error) {
	return f.LanguagesFunc(ctx, tenantID, agentID)
}

type BaselineSelector struct {
	SelectFunc func(context.Context, int64, string, string) (domain.PromptBaselineSelection, error)
}

func (f BaselineSelector) Select(ctx context.Context, tenantID int64, agentID, agentTypeID string) (domain.PromptBaselineSelection, error) {
	return f.SelectFunc(ctx, tenantID, agentID, agentTypeID)
}

type DraftValidator struct {
	ValidateFunc func(context.Context, int64, domain.AgentDraft, domain.ValidationPhase) ([]domain.AgentFieldError, error)
}

func (f DraftValidator) Validate(ctx context.Context, tenantID int64, draft domain.AgentDraft, phase domain.ValidationPhase) ([]domain.AgentFieldError, error) {
	return f.ValidateFunc(ctx, tenantID, draft, phase)
}

type ActivityReporter struct {
	LastRunFunc func(context.Context, int64) (map[string]time.Time, error)
}

func (f ActivityReporter) LastRun(ctx context.Context, tenantID int64) (map[string]time.Time, error) {
	return f.LastRunFunc(ctx, tenantID)
}

type DraftTester struct {
	ExecuteFunc func(context.Context, domain.StudioActor, domain.AgentTestRequest, domain.AgentDefinition) (domain.AgentTestResult, error)
}

func (f DraftTester) Execute(ctx context.Context, actor domain.StudioActor, request domain.AgentTestRequest, definition domain.AgentDefinition) (domain.AgentTestResult, error) {
	return f.ExecuteFunc(ctx, actor, request, definition)
}

type KindCatalog struct {
	GetFunc  func(context.Context, string) (domain.AgentTypeDescriptor, error)
	ListFunc func(context.Context) ([]domain.AgentTypeDescriptor, error)
}

func (f KindCatalog) Get(ctx context.Context, kind string) (domain.AgentTypeDescriptor, error) {
	return f.GetFunc(ctx, kind)
}

func (f KindCatalog) List(ctx context.Context) ([]domain.AgentTypeDescriptor, error) {
	return f.ListFunc(ctx)
}

type ModelCatalog struct {
	ListFunc     func(context.Context, int64) ([]domain.StudioModel, error)
	ValidateFunc func(context.Context, int64, domain.AgentModelSelection) ([]domain.AgentFieldError, error)
}

func (f ModelCatalog) List(ctx context.Context, tenantID int64) ([]domain.StudioModel, error) {
	return f.ListFunc(ctx, tenantID)
}

func (f ModelCatalog) Validate(ctx context.Context, tenantID int64, selection domain.AgentModelSelection) ([]domain.AgentFieldError, error) {
	return f.ValidateFunc(ctx, tenantID, selection)
}

var (
	_ contract.PromptSourceRepository = PromptSources{}
	_ contract.PromptBaselineSelector = BaselineSelector{}
	_ contract.AgentDraftValidator    = DraftValidator{}
	_ contract.AgentActivityReporter  = ActivityReporter{}
	_ contract.AgentDraftTestExecutor = DraftTester{}
	_ contract.AgentTypeCatalog       = KindCatalog{}
	_ contract.StudioModelCatalog     = ModelCatalog{}
)
