package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// AgentVersionRepository stores immutable tenant agent definitions.
type AgentVersionRepository interface {
	// Publish persists a new immutable agent definition version.
	Publish(ctx context.Context, tenantID int64, definition domain.AgentDefinition) error
	// Get returns one immutable agent definition version.
	Get(ctx context.Context, tenantID int64, agentID, version string) (domain.AgentDefinition, error)
	// List returns the published versions for an agent.
	List(ctx context.Context, tenantID int64, agentID string) ([]domain.AgentDefinition, error)
}

// AgentPublicationStore atomically persists an immutable definition and compiled graph.
type AgentPublicationStore interface {
	// Publish stores the definition and graph in one transaction.
	Publish(ctx context.Context, tenantID int64, definition domain.AgentDefinition, graph domain.ExecutionGraph) error
}

// AgentCompiler converts an agent definition into an executable graph.
type AgentCompiler interface {
	// Compile validates a definition and produces an immutable execution graph.
	Compile(ctx context.Context, definition domain.AgentDefinition) (domain.ExecutionGraph, error)
}

// ExecutionGraphRepository stores compiled execution graphs.
type ExecutionGraphRepository interface {
	// Put persists a compiled graph by tenant, agent, and version.
	Put(ctx context.Context, tenantID int64, graph domain.ExecutionGraph) error
	// Get returns a compiled graph by tenant, agent, and version.
	Get(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, error)
}

// ExecutionGraphCache provides low-latency access to compiled graphs.
type ExecutionGraphCache interface {
	// Get returns a cached graph and whether it was found.
	Get(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, bool, error)
	// Put caches a graph under its immutable version.
	Put(ctx context.Context, tenantID int64, graph domain.ExecutionGraph) error
	// Invalidate removes cached graphs matching an immutable version.
	Invalidate(ctx context.Context, tenantID int64, agentID, version string) error
}

// ToolRegistry manages immutable tenant tool contracts.
type ToolRegistry interface {
	// Register publishes an immutable tenant-scoped tool contract.
	Register(ctx context.Context, tenantID int64, tool domain.ToolDefinition) error
	// Get returns a tenant-scoped tool contract by immutable version.
	Get(ctx context.Context, tenantID int64, toolID, version string) (domain.ToolDefinition, error)
	// List returns the tool contracts available to an agent.
	List(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.ToolDefinition, error)
}

// GuardrailConfigRepository stores versioned tenant guardrail policies.
type GuardrailConfigRepository interface {
	// Publish persists a new immutable guardrail configuration.
	Publish(ctx context.Context, tenantID int64, agentID string, config domain.GuardrailConfig) error
	// Get returns the guardrails pinned to an agent version.
	Get(ctx context.Context, tenantID int64, agentID, agentVersion string) (domain.GuardrailConfig, error)
}

// TenantPolicyRepository provides tenant-specific runtime limits.
type TenantPolicyRepository interface {
	// GetRuntimePolicy returns the current tenant execution limits.
	GetRuntimePolicy(ctx context.Context, tenantID int64) (domain.TenantRuntimePolicy, error)
}

// AgentVersionTrafficManager controls tenant agent canaries and rollback.
type AgentVersionTrafficManager interface {
	// ResolveVersion selects an agent version using the tenant's traffic policy.
	ResolveVersion(ctx context.Context, tenantID int64, agentID, conversationID string) (string, error)
	// SetCanary assigns a percentage of tenant traffic to a canary version.
	SetCanary(ctx context.Context, tenantID int64, agentID, version string, percentage int) error
	// Promote makes a tested version the tenant's default.
	Promote(ctx context.Context, tenantID int64, agentID, version string) error
	// Rollback restores the tenant's previous stable version.
	Rollback(ctx context.Context, tenantID int64, agentID string) error
}

// AgentPublisher coordinates publication of immutable agent versions.
type AgentPublisher interface {
	// Publish validates, compiles, persists, and makes a version eligible for traffic.
	Publish(ctx context.Context, tenantID int64, definition domain.AgentDefinition) error
}
