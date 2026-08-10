package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// GuardrailEnforcer applies pinned tenant policy at every runtime boundary.
type GuardrailEnforcer interface {
	// BeforeModel validates and transforms model input.
	BeforeModel(ctx context.Context, config domain.GuardrailConfig, request domain.ModelRequest) (domain.ModelRequest, error)
	// AfterModelChunk validates and transforms streamed model output.
	AfterModelChunk(ctx context.Context, config domain.GuardrailConfig, chunk domain.ModelChunk) (domain.ModelChunk, error)
	// BeforeTool validates and transforms tool arguments.
	BeforeTool(ctx context.Context, config domain.GuardrailConfig, call domain.ToolCall) (domain.ToolCall, error)
	// AfterTool validates and transforms tool output.
	AfterTool(ctx context.Context, config domain.GuardrailConfig, result domain.ToolResult) (domain.ToolResult, error)
}
