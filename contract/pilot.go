package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// PilotResolver finds the supervised pilot release an agent serves to one person.
type PilotResolver interface {
	// Resolve returns the membership in force at `at` that serves person, who must be a
	// human: their own membership first, else one listing a position they hold. False
	// leaves the person to the cohort and canary rules.
	Resolve(ctx context.Context, tenantID int64, agentID string, person domain.PrincipalRef, at time.Time) (domain.PilotMember, bool, error)
}

// PositionResolver is the product port naming the positions a person holds at `at`, as the
// opaque references Studio lists in pilots. Positions are the product's vocabulary.
type PositionResolver interface {
	Positions(ctx context.Context, tenantID int64, person domain.PrincipalRef, at time.Time) ([]string, error)
}

// StudioPilotBackend lists and changes the people and positions an agent's pilot releases serve.
type StudioPilotBackend interface {
	List(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.PilotMember, error)
	Add(ctx context.Context, actor domain.StudioActor, change domain.PilotChange) (domain.PilotMember, error)
	Remove(ctx context.Context, actor domain.StudioActor, change domain.PilotChange) (domain.PilotMember, error)
}
