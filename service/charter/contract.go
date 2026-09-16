package charter

import (
	"context"

	"github.com/nauticana/charter/sdk/binding"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
)

// These three interfaces carry Charter types, so they live here rather than in contract: a downstream that does
// not compose Charter never compiles its SDK.

// CallContext resolves who is calling a Charter capability from the request context.
type CallContext interface {
	Caller(ctx context.Context) (domain.TenantContext, domain.Principal, error)
}

// ToolBinder turns a Charter capability request into the Scout tool call the binding realizes.
type ToolBinder interface {
	Bind(ctx context.Context, binding model.CapabilityBinding, req binding.Request) (domain.ToolCall, error)
}

// TriggerDispatcher hands a delivered Charter trigger to the runtime for one definition that declares it.
type TriggerDispatcher interface {
	Dispatch(ctx context.Context, definition model.AgentDefinition, delivery binding.Delivery) error
}
