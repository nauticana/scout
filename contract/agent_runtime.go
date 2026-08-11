package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// PromptRenderer turns a published agent's compiled sections and one task into
// the payload sent to a model provider. It is the step between an immutable
// definition and inference, so its output format is part of published
// behavior: changing it changes what already-released agents produce.
type PromptRenderer interface {
	Render(agentID string, sections []domain.CompiledPromptSection, task domain.AgentTask) string
}

// MediaProvider adapts a provider that generates images or video. Providers
// that only serve text implement ModelProvider alone.
type MediaProvider interface {
	GenerateImage(ctx context.Context, model string, request domain.ImageRequest) ([]domain.GeneratedMedia, error)
	GenerateVideo(ctx context.Context, model string, request domain.VideoRequest) ([]domain.GeneratedMedia, error)
}

// AgentProviderFactory supplies the configured provider adapters for one
// published model reference. Products may construct adapters on demand from a
// keystore, while long-lived processes may return pre-registered adapters.
type AgentProviderFactory interface {
	Build(ctx context.Context, reference domain.ModelReference) (ModelProvider, MediaProvider, error)
}

// AgentExecutor binds one compiled prompt and model reference to provider
// adapters. Product runtimes can embed it and add concerns such as pricing or
// quota accounting without reimplementing prompt and media execution.
type AgentExecutor interface {
	Generate(ctx context.Context, task domain.AgentTask) (domain.ModelResult, error)
	GenerateImage(ctx context.Context, prompt string, request domain.ImageRequest) ([]domain.GeneratedMedia, error)
	GenerateVideo(ctx context.Context, prompt string, request domain.VideoRequest) ([]domain.GeneratedMedia, error)
	AgentID() string
	ModelReference() domain.ModelReference
	PromptSections() []domain.CompiledPromptSection
}

// AgentRuntime is the executable text/image/video view of one published
// release and language.
type AgentRuntime interface {
	Release() domain.AgentReleaseReference
	Language() domain.CompiledPrompt
	Text() AgentExecutor
	Image() AgentExecutor
	Video() AgentExecutor
}

// AgentRuntimeResolver resolves a live alias and binds its immutable release
// to configured provider adapters.
type AgentRuntimeResolver interface {
	Resolve(ctx context.Context, tenantID int64, aliasID, languageCode, conversationID string) (AgentRuntime, error)
}

// AgentRunRecorder records one successful execution against the exact
// immutable release that ran.
type AgentRunRecorder interface {
	Record(ctx context.Context, tenantID int64, release domain.AgentReleaseReference, taskKind string) error
}

// ModelPricer prices usage for one model reference, returning minor units in
// the catalog currency. controlplane.ModelCatalog implements it.
type ModelPricer interface {
	Cost(ctx context.Context, reference domain.ModelReference, usage domain.ModelUsage) (int64, string, error)
}

// PricedAgent is an executor that prices its own usage, so callers can quote
// and bill a task without resolving the model catalog themselves.
type PricedAgent interface {
	AgentExecutor
	// GenerateText runs the task and returns the output text with its token
	// counts, for callers that do not need the full result.
	GenerateText(ctx context.Context, task domain.AgentTask) (string, int64, int64, error)
	// Cost prices usage on this agent's own model.
	Cost(ctx context.Context, usage domain.ModelUsage) (int64, error)
}

// AgentRunPurger drops agent run activity past the retention horizon Scout
// owns. Deletes are bounded so a scheduled caller drains a backlog over
// several ticks.
type AgentRunPurger interface {
	Purge(ctx context.Context, limit int) (int64, error)
}

// AgentOperationalEventRecorder persists a tenant-scoped operational failure
// that may occur before a specific agent exists.
type AgentOperationalEventRecorder interface {
	RecordOperationalEvent(ctx context.Context, tenantID int64, event, detail string) error
}
