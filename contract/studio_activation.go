package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// StudioActivationBackend shows an agent's scheduled publishes and cancels a pending one.
type StudioActivationBackend interface {
	Activations(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.ReleaseActivation, error)
	Cancel(ctx context.Context, actor domain.StudioActor, request domain.ReleaseActivationCancel) (domain.ReleaseActivation, error)
}
