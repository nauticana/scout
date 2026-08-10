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
