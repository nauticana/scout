package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// GuardrailEnforcer contains configurable guardrail callbacks; nil funcs pass values through.
type GuardrailEnforcer struct {
	BeforeModelFunc     func(context.Context, domain.GuardrailConfig, domain.ModelRequest) (domain.ModelRequest, error)
	AfterModelChunkFunc func(context.Context, domain.GuardrailConfig, domain.ModelChunk) (domain.ModelChunk, error)
	BeforeToolFunc      func(context.Context, domain.GuardrailConfig, domain.ToolCall) (domain.ToolCall, error)
	AfterToolFunc       func(context.Context, domain.GuardrailConfig, domain.ToolResult) (domain.ToolResult, error)
}

func (enforcer *GuardrailEnforcer) BeforeModel(ctx context.Context, config domain.GuardrailConfig, request domain.ModelRequest) (domain.ModelRequest, error) {
	if enforcer.BeforeModelFunc == nil {
		return request, nil
	}
	return enforcer.BeforeModelFunc(ctx, config, request)
}

func (enforcer *GuardrailEnforcer) AfterModelChunk(ctx context.Context, config domain.GuardrailConfig, chunk domain.ModelChunk) (domain.ModelChunk, error) {
	if enforcer.AfterModelChunkFunc == nil {
		return chunk, nil
	}
	return enforcer.AfterModelChunkFunc(ctx, config, chunk)
}

func (enforcer *GuardrailEnforcer) BeforeTool(ctx context.Context, config domain.GuardrailConfig, call domain.ToolCall) (domain.ToolCall, error) {
	if enforcer.BeforeToolFunc == nil {
		return call, nil
	}
	return enforcer.BeforeToolFunc(ctx, config, call)
}

func (enforcer *GuardrailEnforcer) AfterTool(ctx context.Context, config domain.GuardrailConfig, result domain.ToolResult) (domain.ToolResult, error) {
	if enforcer.AfterToolFunc == nil {
		return result, nil
	}
	return enforcer.AfterToolFunc(ctx, config, result)
}

var _ contract.GuardrailEnforcer = (*GuardrailEnforcer)(nil)
