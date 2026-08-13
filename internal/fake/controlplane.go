package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// AgentCompilerFunc adapts a function to contract.AgentCompiler.
type AgentCompilerFunc func(context.Context, domain.AgentDefinition) (domain.ExecutionGraph, error)

// Compile invokes the configured function.
func (function AgentCompilerFunc) Compile(ctx context.Context, definition domain.AgentDefinition) (domain.ExecutionGraph, error) {
	return function(ctx, definition)
}

// AgentPublicationStoreFunc adapts a function to contract.AgentPublicationStore.
type AgentPublicationStoreFunc func(context.Context, int64, domain.AgentDefinition, domain.ExecutionGraph) error

// Publish invokes the configured function.
func (function AgentPublicationStoreFunc) Publish(ctx context.Context, tenantID int64, definition domain.AgentDefinition, graph domain.ExecutionGraph) error {
	return function(ctx, tenantID, definition, graph)
}

var _ contract.AgentCompiler = AgentCompilerFunc(nil)
var _ contract.AgentPublicationStore = AgentPublicationStoreFunc(nil)
