package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	keelcommon "github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ProviderAgent binds one compiled agent prompt and model reference to provider
// adapters. It is complete for token and media execution; product-specific
// wrappers may embed it to add pricing or usage accounting.
type ProviderAgent struct {
	agentID         string
	reference       domain.ModelReference
	sections        []domain.CompiledPromptSection
	maxOutputTokens int64
	renderer        contract.PromptRenderer
	provider        contract.ModelProvider
	media           contract.MediaProvider
	tenant          domain.TenantContext
	requestID       string
	conversationID  string
}

var _ contract.AgentExecutor = (*ProviderAgent)(nil)

// NewProviderAgent validates and freezes one executable provider binding.
func NewProviderAgent(agentID string, reference domain.ModelReference, sections []domain.CompiledPromptSection, maxOutputTokens int64, renderer contract.PromptRenderer, provider contract.ModelProvider, media contract.MediaProvider) (*ProviderAgent, error) {
	return newProviderAgent(agentID, reference, sections, maxOutputTokens, renderer, provider, media, domain.TenantContext{}, "", "")
}

// NewTenantProviderAgent binds tenant-scoped invocation identity to an agent.
func NewTenantProviderAgent(agentID string, reference domain.ModelReference, sections []domain.CompiledPromptSection, maxOutputTokens int64, renderer contract.PromptRenderer, provider contract.ModelProvider, media contract.MediaProvider, tenant domain.TenantContext, requestID, conversationID string) (*ProviderAgent, error) {
	if tenant.TenantID <= 0 {
		return nil, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	return newProviderAgent(agentID, reference, sections, maxOutputTokens, renderer, provider, media, tenant, requestID, conversationID)
}

func newProviderAgent(agentID string, reference domain.ModelReference, sections []domain.CompiledPromptSection, maxOutputTokens int64, renderer contract.PromptRenderer, provider contract.ModelProvider, media contract.MediaProvider, tenant domain.TenantContext, requestID, conversationID string) (*ProviderAgent, error) {
	agentID = strings.TrimSpace(agentID)
	reference.ProviderID = strings.TrimSpace(reference.ProviderID)
	reference.ModelID = strings.TrimSpace(reference.ModelID)
	requestID = strings.TrimSpace(requestID)
	conversationID = strings.TrimSpace(conversationID)
	if agentID == "" || reference.ProviderID == "" || reference.ModelID == "" {
		return nil, fmt.Errorf("%w: agent, provider, and model are required", domain.ErrValidation)
	}
	if provider == nil && media == nil {
		return nil, fmt.Errorf("%w: provider adapters are required", domain.ErrValidation)
	}
	if provider != nil && maxOutputTokens <= 0 {
		return nil, fmt.Errorf("%w: positive max output tokens are required", domain.ErrValidation)
	}
	if provider != nil && renderer == nil {
		return nil, fmt.Errorf("%w: prompt renderer is required", domain.ErrValidation)
	}
	return &ProviderAgent{
		agentID:         agentID,
		reference:       reference,
		sections:        append([]domain.CompiledPromptSection(nil), sections...),
		maxOutputTokens: maxOutputTokens,
		renderer:        renderer,
		provider:        provider,
		media:           media,
		tenant:          tenant,
		requestID:       requestID,
		conversationID:  conversationID,
	}, nil
}

// Generate renders and executes one text task.
func (agent *ProviderAgent) Generate(ctx context.Context, task domain.AgentTask) (domain.ModelResult, error) {
	if agent == nil || agent.provider == nil || agent.renderer == nil {
		return domain.ModelResult{}, fmt.Errorf("%w: text generation is not configured", domain.ErrNotReady)
	}
	requestID, err := agent.modelRequestID(ctx)
	if err != nil {
		return domain.ModelResult{}, err
	}
	prompt := agent.renderer.Render(agent.agentID, agent.sections, task)
	return agent.provider.Generate(ctx, domain.ModelSelection{
		Provider: agent.reference.ProviderID,
		Model:    agent.reference.ModelID,
	}, domain.ModelRequest{
		TenantContext:   agent.tenant,
		RequestID:       requestID,
		ConversationID:  agent.conversationID,
		Prompt:          []byte(prompt),
		MaxOutputTokens: agent.maxOutputTokens,
	})
}

func (agent *ProviderAgent) modelRequestID(ctx context.Context) (string, error) {
	if requestID := strings.TrimSpace(keelcommon.RequestIDFromContext(ctx)); requestID != "" {
		return requestID, nil
	}
	if agent.requestID != "" {
		return agent.requestID, nil
	}
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate model request id: %w", err)
	}
	return "agent-" + hex.EncodeToString(value), nil
}

// GenerateImage executes one image request, overriding any prompt already on
// the request with the invocation prompt.
func (agent *ProviderAgent) GenerateImage(ctx context.Context, prompt string, request domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	if agent == nil || agent.media == nil {
		return nil, fmt.Errorf("%w: image generation is not supported", domain.ErrNotReady)
	}
	requestID, err := agent.modelRequestID(ctx)
	if err != nil {
		return nil, err
	}
	request.TenantContext = agent.tenant
	request.RequestID = requestID
	request.ConversationID = agent.conversationID
	request.Prompt = prompt
	return agent.media.GenerateImage(ctx, agent.reference.ModelID, request)
}

// GenerateVideo executes one video request, overriding any prompt already on
// the request with the invocation prompt.
func (agent *ProviderAgent) GenerateVideo(ctx context.Context, prompt string, request domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	if agent == nil || agent.media == nil {
		return nil, fmt.Errorf("%w: video generation is not supported", domain.ErrNotReady)
	}
	requestID, err := agent.modelRequestID(ctx)
	if err != nil {
		return nil, err
	}
	request.TenantContext = agent.tenant
	request.RequestID = requestID
	request.ConversationID = agent.conversationID
	request.Prompt = prompt
	return agent.media.GenerateVideo(ctx, agent.reference.ModelID, request)
}

// AgentID returns the immutable definition's agent id.
func (agent *ProviderAgent) AgentID() string { return agent.agentID }

// ModelReference returns the selected provider model.
func (agent *ProviderAgent) ModelReference() domain.ModelReference { return agent.reference }

// PromptSections returns a copy of the compiled prompt sections.
func (agent *ProviderAgent) PromptSections() []domain.CompiledPromptSection {
	return append([]domain.CompiledPromptSection(nil), agent.sections...)
}
