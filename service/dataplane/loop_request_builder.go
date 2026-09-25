package dataplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/skill"
)

// ReleaseLoopRequestBuilder opens a tool loop with the pinned release's compiled
// prompt and the turn input as the task. Task is the hook for product context; the
// index of the release's skills follows it.
type ReleaseLoopRequestBuilder struct {
	Definitions contract.AgentDefinitionReader
	Renderer    contract.PromptRenderer
	// Skills is required only by a release that binds skills.
	Skills contract.SkillRegistry
	// LanguageCode selects the compiled prompt; empty takes the release's only language.
	LanguageCode    string
	MaxOutputTokens int64
	// Task turns the step input into the rendered task and may choose a language;
	// nil renders the turn input as the task in LanguageCode.
	Task func(ctx context.Context, input domain.StepInput) (task domain.AgentTask, languageCode string, err error)
}

func (builder *ReleaseLoopRequestBuilder) Build(ctx context.Context, input domain.StepInput, _ domain.ToolLoopConfig) (domain.ModelRequest, error) {
	if builder.Definitions == nil || builder.Renderer == nil || builder.MaxOutputTokens <= 0 {
		return domain.ModelRequest{}, fmt.Errorf("%w: loop request builder needs a definition reader, a renderer, and positive max output tokens", domain.ErrValidation)
	}
	principal := input.Principal
	definition, err := builder.Definitions.Get(ctx, principal.TenantID, principal.ID, principal.Release)
	if err != nil {
		return domain.ModelRequest{}, err
	}
	task, languageCode := domain.AgentTask{Task: string(input.Input)}, builder.LanguageCode
	if builder.Task != nil {
		var chosen string
		if task, chosen, err = builder.Task(ctx, input); err != nil {
			return domain.ModelRequest{}, err
		}
		if chosen != "" {
			languageCode = chosen
		}
	}
	if strings.TrimSpace(task.Task) == "" {
		return domain.ModelRequest{}, fmt.Errorf("%w: the turn has no input to act on", domain.ErrValidation)
	}
	if task.Context, err = builder.withSkillIndex(ctx, principal.TenantID, definition, task.Context); err != nil {
		return domain.ModelRequest{}, err
	}
	prompt, err := compiledLanguage(definition, languageCode)
	if err != nil {
		return domain.ModelRequest{}, err
	}
	return domain.ModelRequest{
		TenantContext:   domain.TenantContext{TenantID: principal.TenantID, ScopeID: principal.ScopeID},
		RequestID:       input.RequestID,
		ConversationID:  input.Snapshot.ConversationID,
		Prompt:          []byte(builder.Renderer.Render(definition.AgentID, prompt.Sections, task)),
		MaxOutputTokens: builder.MaxOutputTokens,
		Model:           pinnedTextModel(definition),
		AffinityKey:     input.Snapshot.ConversationID,
		Output:          task.Output,
	}, nil
}

func (builder *ReleaseLoopRequestBuilder) withSkillIndex(ctx context.Context, tenantID int64, definition domain.AgentDefinition, taskContext string) (string, error) {
	if len(definition.Skills) == 0 {
		return taskContext, nil
	}
	if builder.Skills == nil {
		return "", fmt.Errorf("%w: release %s@%s binds skills but no skill registry is composed", domain.ErrNotReady, definition.AgentID, definition.Version)
	}
	skills, err := builder.Skills.List(ctx, tenantID, definition.AgentID, definition.Version)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(taskContext + "\n\n" + skill.Index(skills)), nil
}

func pinnedTextModel(definition domain.AgentDefinition) domain.ModelReference {
	if definition.Models.Text == nil {
		return domain.ModelReference{}
	}
	return *definition.Models.Text
}

func compiledLanguage(definition domain.AgentDefinition, languageCode string) (domain.CompiledPrompt, error) {
	if languageCode == "" && len(definition.Languages) == 1 {
		return definition.Languages[0], nil
	}
	for _, language := range definition.Languages {
		if language.LanguageCode == languageCode {
			return language, nil
		}
	}
	return domain.CompiledPrompt{}, fmt.Errorf("%w: release %s@%s has no compiled language %q", domain.ErrNoPrompts, definition.AgentID, definition.Version, languageCode)
}

// LoopBudgetEstimator quotes a turn at the loop's ceilings, because a loop's
// spend is bounded by its limits, not by one model call.
type LoopBudgetEstimator struct {
	Limits domain.ToolLoopLimits
	// Currency denominates the quote; required when the limits carry a cost ceiling.
	Currency string
}

func (estimator LoopBudgetEstimator) Estimate(_ context.Context, _ domain.TurnRequest) (domain.Usage, error) {
	if estimator.Limits.MaxTokens <= 0 {
		return domain.Usage{}, fmt.Errorf("%w: loop budget estimator needs a positive token ceiling", domain.ErrValidation)
	}
	if estimator.Limits.MaxCostMinorUnits > 0 && strings.TrimSpace(estimator.Currency) == "" {
		return domain.Usage{}, fmt.Errorf("%w: a cost ceiling requires a currency", domain.ErrValidation)
	}
	return domain.Usage{
		InputTokens: estimator.Limits.MaxTokens, ToolCalls: estimator.Limits.MaxToolCalls,
		CostMinorUnits: estimator.Limits.MaxCostMinorUnits, Currency: estimator.Currency,
	}, nil
}

var (
	_ contract.ToolLoopRequestBuilder = (*ReleaseLoopRequestBuilder)(nil)
	_ contract.TurnBudgetEstimator    = LoopBudgetEstimator{}
)
