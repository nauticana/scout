package charter

import (
	"context"

	"github.com/nauticana/charter/sdk/binding"
	"github.com/nauticana/charter/sdk/capability"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
)

// These interfaces carry Charter types, so they live here rather than in contract: a downstream that does
// not compose Charter never compiles its SDK.

// CallContext resolves who is calling a Charter capability from the request context.
type CallContext interface {
	Caller(ctx context.Context) (domain.TenantContext, domain.Principal, error)
}

// Capabilities names the Charter capability a tenant's tool realizes; a tool not found is not governed.
type Capabilities interface {
	Capability(ctx context.Context, tenantID int64, toolID string) (Capability, bool, error)
}

// BindingTools names the Scout tool a tenant's capability binding realizes; a binding not found runs nothing.
type BindingTools interface {
	Tool(ctx context.Context, tenantID int64, binding corpus.DocumentKey) (domain.ToolReference, bool, error)
}

// ToolBinder turns a Charter capability request into the Scout tool call the binding realizes.
type ToolBinder interface {
	Bind(ctx context.Context, binding model.CapabilityBinding, req binding.Request) (domain.ToolCall, error)
}

// TriggerDispatcher hands a delivered Charter trigger to the runtime for one definition that declares it.
type TriggerDispatcher interface {
	Dispatch(ctx context.Context, definition model.AgentDefinition, delivery binding.Delivery) error
}

// Invocations describes a governed tool call in Charter's terms: actor, assignment or responsibility, enterprise,
// resource scope, measures, subjects, and material inputs, which only the product can read from its arguments. The
// gateway then fixes the capability, inputs, idempotency key, and execution context. An error refuses the call.
type Invocations interface {
	Invocation(ctx context.Context, call domain.ToolCall, target model.Ref) (capability.Invocation, error)
}
