package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// StudioGuardrailBackend shows and replaces an agent's enforced guardrail rules and pages
// the safety events recorded against it.
type StudioGuardrailBackend interface {
	Guardrails(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentGuardrails, error)
	SaveGuardrails(ctx context.Context, actor domain.StudioActor, change domain.AgentGuardrailChange) (domain.AgentGuardrails, error)
	SafetyEvents(ctx context.Context, actor domain.StudioActor, filter domain.SafetyEventFilter) ([]domain.SafetyEvent, error)
}

// SafetyEventQuery pages the safety events recorded against one agent of a tenant, newest
// first. Events carry only what the sink stored: never the inspected content.
type SafetyEventQuery interface {
	SafetyEvents(ctx context.Context, tenantID int64, filter domain.SafetyEventFilter) ([]domain.SafetyEvent, error)
}
