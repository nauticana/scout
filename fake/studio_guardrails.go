package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioGuardrailBackend contains configurable Studio guardrail callbacks.
type StudioGuardrailBackend struct {
	GuardrailsFunc     func(context.Context, domain.StudioActor, string) (domain.AgentGuardrails, error)
	SaveGuardrailsFunc func(context.Context, domain.StudioActor, domain.AgentGuardrailChange) (domain.AgentGuardrails, error)
	SafetyEventsFunc   func(context.Context, domain.StudioActor, domain.SafetyEventFilter) ([]domain.SafetyEvent, error)
}

// Guardrails invokes the configured function.
func (backend StudioGuardrailBackend) Guardrails(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentGuardrails, error) {
	return backend.GuardrailsFunc(ctx, actor, agentID)
}

// SaveGuardrails invokes the configured function.
func (backend StudioGuardrailBackend) SaveGuardrails(ctx context.Context, actor domain.StudioActor, change domain.AgentGuardrailChange) (domain.AgentGuardrails, error) {
	return backend.SaveGuardrailsFunc(ctx, actor, change)
}

// SafetyEvents invokes the configured function.
func (backend StudioGuardrailBackend) SafetyEvents(ctx context.Context, actor domain.StudioActor, filter domain.SafetyEventFilter) ([]domain.SafetyEvent, error) {
	return backend.SafetyEventsFunc(ctx, actor, filter)
}

// SafetyEventQueryFunc adapts a function to contract.SafetyEventQuery.
type SafetyEventQueryFunc func(context.Context, int64, domain.SafetyEventFilter) ([]domain.SafetyEvent, error)

// SafetyEvents invokes the configured function.
func (function SafetyEventQueryFunc) SafetyEvents(ctx context.Context, tenantID int64, filter domain.SafetyEventFilter) ([]domain.SafetyEvent, error) {
	return function(ctx, tenantID, filter)
}

var (
	_ contract.StudioGuardrailBackend = StudioGuardrailBackend{}
	_ contract.SafetyEventQuery       = SafetyEventQueryFunc(nil)
)
