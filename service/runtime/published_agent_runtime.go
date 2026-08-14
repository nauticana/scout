package runtime

import (
	"context"
	"fmt"
	"strings"

	keelcommon "github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DefaultAgentLanguage is used when a runtime does not configure another
// locale fallback.
const DefaultAgentLanguage = "en-US"

// PublishedAgentRuntime resolves a live alias and binds its immutable release
// to configured provider adapters.
type PublishedAgentRuntime struct {
	Definitions      contract.PublishedAgentResolver
	Providers        contract.AgentProviderFactory
	Renderer         contract.PromptRenderer
	MaxOutputTokens  int64
	FallbackLanguage string
	// Pricer, when set, makes ResolvePriced available to callers that bill.
	Pricer contract.ModelPricer
}

var _ contract.AgentRuntimeResolver = (*PublishedAgentRuntime)(nil)

// Resolve builds the text and optional media executors for one active alias.
// Definition.Enabled is deliberately not consulted: PublishedAgentResolver
// owns the live agent_profile switch, while the release value is historical.
func (runtime *PublishedAgentRuntime) Resolve(ctx context.Context, tenantID int64, aliasID, languageCode, conversationID string) (contract.AgentRuntime, error) {
	if runtime == nil || runtime.Definitions == nil || runtime.Providers == nil {
		return nil, fmt.Errorf("published agent runtime: definition resolver and provider factory are required")
	}
	if runtime.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("published agent runtime: positive max output tokens are required")
	}
	renderer := runtime.Renderer
	if renderer == nil {
		renderer = PromptRenderer{}
	}
	definition, err := runtime.Definitions.Resolve(ctx, tenantID, aliasID, "", conversationID)
	if err != nil {
		return nil, err
	}
	fallback := strings.TrimSpace(runtime.FallbackLanguage)
	if fallback == "" {
		fallback = DefaultAgentLanguage
	}
	language, err := SelectLanguage(definition, languageCode, fallback)
	if err != nil {
		return nil, err
	}
	if definition.Models.Text == nil {
		return nil, fmt.Errorf("%w: agent %s release %s has no text model", domain.ErrNotReady, definition.AgentID, definition.Version)
	}

	resolved := &ResolvedAgents{
		release: domain.AgentReleaseReference{
			AgentID: definition.AgentID,
			Version: definition.Version,
			Digest:  definition.DefinitionDigest,
		},
		language: cloneCompiledPrompt(language),
	}
	resolved.text, err = runtime.bind(ctx, tenantID, conversationID, definition.AgentID, *definition.Models.Text, language.Sections, renderer, false)
	if err != nil {
		return nil, fmt.Errorf("bind text model: %w", err)
	}
	if definition.Models.Image != nil {
		resolved.image, err = runtime.bind(ctx, tenantID, conversationID, definition.AgentID, *definition.Models.Image, language.Sections, renderer, true)
		if err != nil {
			return nil, fmt.Errorf("bind image model: %w", err)
		}
	}
	if definition.Models.Video != nil {
		resolved.video, err = runtime.bind(ctx, tenantID, conversationID, definition.AgentID, *definition.Models.Video, language.Sections, renderer, true)
		if err != nil {
			return nil, fmt.Errorf("bind video model: %w", err)
		}
	}
	return resolved, nil
}

func (runtime *PublishedAgentRuntime) bind(ctx context.Context, tenantID int64, conversationID, agentID string, reference domain.ModelReference, sections []domain.CompiledPromptSection, renderer contract.PromptRenderer, requireMedia bool) (contract.AgentExecutor, error) {
	provider, media, err := runtime.Providers.Build(ctx, reference)
	if err != nil {
		return nil, fmt.Errorf("provider %s model %s: %w", reference.ProviderID, reference.ModelID, err)
	}
	if provider == nil {
		return nil, fmt.Errorf("%w: provider %s has no text adapter", domain.ErrNotReady, reference.ProviderID)
	}
	if requireMedia && media == nil {
		return nil, fmt.Errorf("%w: provider %s has no media adapter", domain.ErrNotReady, reference.ProviderID)
	}
	return NewTenantProviderAgent(agentID, reference, sections, runtime.MaxOutputTokens, renderer, provider, media,
		domain.TenantContext{TenantID: tenantID}, keelcommon.RequestIDFromContext(ctx), conversationID)
}

// SelectLanguage picks an exact compiled language and otherwise uses the
// requested fallback language.
func SelectLanguage(definition domain.AgentDefinition, languageCode, fallbackLanguage string) (domain.CompiledPrompt, error) {
	languageCode = strings.TrimSpace(languageCode)
	fallbackLanguage = strings.TrimSpace(fallbackLanguage)
	if languageCode == "" {
		languageCode = fallbackLanguage
	}
	var fallback *domain.CompiledPrompt
	for i := range definition.Languages {
		language := &definition.Languages[i]
		if language.LanguageCode == languageCode {
			return cloneCompiledPrompt(*language), nil
		}
		if fallbackLanguage != "" && language.LanguageCode == fallbackLanguage {
			fallback = language
		}
	}
	if fallback != nil {
		return cloneCompiledPrompt(*fallback), nil
	}
	return domain.CompiledPrompt{}, fmt.Errorf("agent %s language %s: %w", definition.AgentID, languageCode, domain.ErrNoPrompts)
}

// ResolvedAgents is the executable view returned by PublishedAgentRuntime.
type ResolvedAgents struct {
	release  domain.AgentReleaseReference
	language domain.CompiledPrompt
	text     contract.AgentExecutor
	image    contract.AgentExecutor
	video    contract.AgentExecutor
}

var _ contract.AgentRuntime = (*ResolvedAgents)(nil)

func (agents *ResolvedAgents) Release() domain.AgentReleaseReference { return agents.release }

// Language returns a copy of the selected compiled prompt.
func (agents *ResolvedAgents) Language() domain.CompiledPrompt {
	return cloneCompiledPrompt(agents.language)
}

// Text returns the required text executor.
func (agents *ResolvedAgents) Text() contract.AgentExecutor { return agents.text }

// Image returns the optional image executor.
func (agents *ResolvedAgents) Image() contract.AgentExecutor { return agents.image }

// Video returns the optional video executor.
func (agents *ResolvedAgents) Video() contract.AgentExecutor { return agents.video }

func cloneCompiledPrompt(prompt domain.CompiledPrompt) domain.CompiledPrompt {
	prompt.Sections = append([]domain.CompiledPromptSection(nil), prompt.Sections...)
	return prompt
}

// PricedAgents is one alias resolved into executors that price their own usage.
type PricedAgents struct {
	Release domain.AgentReleaseReference
	Text    contract.PricedAgent
	Image   contract.PricedAgent
	Video   contract.PricedAgent
}

// ResolvePriced resolves an alias and wraps every present modality in a
// PricedAgent, so a billing caller does not repeat the decoration per modality.
// Absent media modalities stay nil.
func (runtime *PublishedAgentRuntime) ResolvePriced(ctx context.Context, tenantID int64, aliasID, languageCode, conversationID string) (PricedAgents, error) {
	if runtime == nil || runtime.Pricer == nil {
		return PricedAgents{}, fmt.Errorf("published agent runtime: pricer is required to resolve priced agents")
	}
	resolved, err := runtime.Resolve(ctx, tenantID, aliasID, languageCode, conversationID)
	if err != nil {
		return PricedAgents{}, err
	}
	return PricedAgents{
		Release: resolved.Release(),
		Text:    runtime.price(resolved.Text()),
		Image:   runtime.price(resolved.Image()),
		Video:   runtime.price(resolved.Video()),
	}, nil
}

func (runtime *PublishedAgentRuntime) price(executor contract.AgentExecutor) contract.PricedAgent {
	if executor == nil {
		return nil
	}
	return &PricedAgent{AgentExecutor: executor, Pricer: runtime.Pricer}
}
