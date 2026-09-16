package charter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nauticana/charter/sdk/binding"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/validate"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// toolOutput is the JSON shape a Scout tool returns to a Charter capability.
type toolOutput struct {
	Outcome           string          `json:"outcome"`
	BusinessError     string          `json:"business_error,omitempty"`
	Outputs           json.RawMessage `json:"outputs,omitempty"`
	ExternalReference string          `json:"external_reference,omitempty"`
}

// MappedBinder resolves the Scout tool a binding realizes from an explicit map and the caller from the request;
// an unmapped binding provably runs nothing.
type MappedBinder struct {
	Tools   map[corpus.DocumentKey]domain.ToolReference
	Callers CallContext
}

var _ ToolBinder = (*MappedBinder)(nil)

func (b *MappedBinder) Bind(ctx context.Context, cb model.CapabilityBinding, req binding.Request) (domain.ToolCall, error) {
	if b == nil || b.Callers == nil {
		return domain.ToolCall{}, fmt.Errorf("%w: caller resolver is required", binding.ErrNotExecuted)
	}
	tool, ok := b.Tools[corpus.KeyOf(cb.Namespace, model.Ref{Namespace: cb.Namespace, ID: cb.ID})]
	if !ok {
		return domain.ToolCall{}, fmt.Errorf("%w: no Scout tool is mapped for binding %s", binding.ErrNotExecuted, cb.ID)
	}
	tenant, principal, err := b.Callers.Caller(ctx)
	if err != nil {
		return domain.ToolCall{}, fmt.Errorf("%w: %w", binding.ErrNotExecuted, err)
	}
	arguments, err := json.Marshal(req.Inputs)
	if err != nil {
		return domain.ToolCall{}, fmt.Errorf("%w: %w", binding.ErrNotExecuted, err)
	}
	return domain.ToolCall{TenantContext: tenant, Principal: principal, RequestID: req.IdempotencyKey, ToolID: tool.ToolID, ToolVersion: tool.Version, Arguments: arguments}, nil
}

// GatewayEndpoint is the production vendor endpoint: Scout's governed tool gateway.
type GatewayEndpoint struct {
	Gateway contract.GovernedToolGateway
}

var _ validate.VendorEndpoint = (*GatewayEndpoint)(nil)

func (e *GatewayEndpoint) Call(ctx context.Context, payload any) (any, error) {
	if e == nil || e.Gateway == nil {
		return nil, fmt.Errorf("%w: governed gateway is required", binding.ErrNotExecuted)
	}
	call, ok := payload.(domain.ToolCall)
	if !ok {
		return nil, fmt.Errorf("%w: payload is %T, not a tool call", binding.ErrNotExecuted, payload)
	}
	result, err := e.Gateway.Invoke(ctx, call)
	if err != nil {
		return nil, notExecuted(err)
	}
	return result, nil
}

// notExecuted marks refusals that provably ran nothing; every other gateway error leaves the outcome unknown.
func notExecuted(err error) error {
	for _, refused := range []error{domain.ErrValidation, domain.ErrUnauthorized, domain.ErrForbidden, domain.ErrRateLimited, domain.ErrCircuitOpen, domain.ErrNotReady, domain.ErrNotFound} {
		if errors.Is(err, refused) {
			return fmt.Errorf("%w: %w", binding.ErrNotExecuted, err)
		}
	}
	return err
}

// ToolVendor is the vendor mapping for every Scout tool: bind the call, run it at the endpoint, read the tool's
// declared outcome back. The endpoint is the gateway in production and the scripted system under conformance.
type ToolVendor struct {
	Binding  model.CapabilityBinding
	Binder   ToolBinder
	Endpoint validate.VendorEndpoint
}

var _ binding.VendorMapping = (*ToolVendor)(nil)

func (v *ToolVendor) MapRequest(ctx context.Context, req binding.Request) (any, error) {
	return v.Binder.Bind(ctx, v.Binding, req)
}

func (v *ToolVendor) Call(ctx context.Context, payload any) (any, error) {
	return v.Endpoint.Call(ctx, payload)
}

func (v *ToolVendor) MapResponse(_ context.Context, result any) (binding.Response, error) {
	var raw []byte
	switch r := result.(type) {
	case domain.ToolResult:
		raw = r.Output
	case []byte:
		raw = r
	case string:
		raw = []byte(r)
	default:
		encoded, err := json.Marshal(r)
		if err != nil {
			return binding.Response{}, err
		}
		raw = encoded
	}
	var out toolOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return binding.Response{}, fmt.Errorf("tool output is not a declared outcome: %w", err)
	}
	response := binding.Response{Outcome: out.Outcome, BusinessError: out.BusinessError, ExternalReference: out.ExternalReference}
	if len(out.Outputs) > 0 {
		response.Outputs = out.Outputs
	}
	return response, nil
}

// MapError never invents a business error: Scout tools declare theirs in the output body.
func (v *ToolVendor) MapError(error) (string, bool) { return "", false }

// Transport realizes whichever capability binding a request names, so one executor serves every Scout tool.
type Transport struct {
	Namespace string
	Bindings  binding.Provider
	Binder    ToolBinder
	Endpoint  validate.VendorEndpoint
}

var _ binding.Executor = (*Transport)(nil)

func (t *Transport) Execute(ctx context.Context, req binding.Request) (binding.Response, error) {
	if t == nil || t.Bindings == nil || t.Binder == nil || t.Endpoint == nil {
		return binding.Response{}, fmt.Errorf("%w: transport is not fully composed", binding.ErrNotExecuted)
	}
	cb, err := binding.Lookup{Provider: t.Bindings}.CapabilityBindingFor(ctx, t.Namespace, req.Capability, nil)
	if err != nil {
		return binding.Response{}, fmt.Errorf("%w: %w", binding.ErrNotExecuted, err)
	}
	return (&binding.AbstractBinding{Document: cb, Vendor: &ToolVendor{Binding: cb, Binder: t.Binder, Endpoint: t.Endpoint}}).Execute(ctx, req)
}

// Adapter is the system-adapter subject: it realizes a named binding over a vendor endpoint the harness scripts.
type Adapter struct {
	Binder ToolBinder
}

var _ validate.AdapterSubject = (*Adapter)(nil)

func (a *Adapter) Realize(ctx context.Context, documents corpus.Source, owner string, ref model.Ref, vendor validate.VendorEndpoint) (binding.Executor, error) {
	if a == nil || a.Binder == nil || documents == nil || vendor == nil {
		return nil, fmt.Errorf("%w: adapter is not fully composed", binding.ErrNotExecuted)
	}
	cb, err := binding.NewBaseProvider(documents).CapabilityBinding(ctx, owner, ref)
	if err != nil {
		return nil, err
	}
	return &binding.AbstractBinding{Document: cb, Vendor: &ToolVendor{Binding: cb, Binder: a.Binder, Endpoint: vendor}}, nil
}
