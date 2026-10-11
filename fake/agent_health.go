package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioHealthBackend contains configurable health and alert reads.
type StudioHealthBackend struct {
	HealthFunc func(context.Context, domain.StudioActor, string) (domain.AgentHealthReport, error)
	AlertsFunc func(context.Context, domain.StudioActor, domain.AgentAlertFilter) (domain.AgentAlertPage, error)
}

// Health invokes HealthFunc.
func (b *StudioHealthBackend) Health(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentHealthReport, error) {
	return b.HealthFunc(ctx, actor, agentID)
}

// Alerts invokes AlertsFunc.
func (b *StudioHealthBackend) Alerts(ctx context.Context, actor domain.StudioActor, filter domain.AgentAlertFilter) (domain.AgentAlertPage, error) {
	return b.AlertsFunc(ctx, actor, filter)
}

// AgentAlertNotifierFunc adapts a function to contract.AgentAlertNotifier.
type AgentAlertNotifierFunc func(context.Context, domain.AgentAlert) error

// AlertOpened invokes the configured function.
func (function AgentAlertNotifierFunc) AlertOpened(ctx context.Context, alert domain.AgentAlert) error {
	return function(ctx, alert)
}

var (
	_ contract.StudioHealthBackend = (*StudioHealthBackend)(nil)
	_ contract.AgentAlertNotifier  = AgentAlertNotifierFunc(nil)
)
