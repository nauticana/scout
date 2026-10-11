package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// StudioHealthBackend shows an agent's production health over its alert window and pages its alerts.
type StudioHealthBackend interface {
	Health(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentHealthReport, error)
	Alerts(ctx context.Context, actor domain.StudioActor, filter domain.AgentAlertFilter) (domain.AgentAlertPage, error)
}

// AgentAlertNotifier delivers a newly opened agent alert to the people or systems the product
// routes it to. A delivery failure leaves the alert open and is not retried.
type AgentAlertNotifier interface {
	AlertOpened(ctx context.Context, alert domain.AgentAlert) error
}
