package fake

import (
	"context"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PilotResolverFunc adapts a function to contract.PilotResolver.
type PilotResolverFunc func(context.Context, int64, string, domain.PrincipalRef, time.Time) (domain.PilotMember, bool, error)

// Resolve invokes the configured function.
func (function PilotResolverFunc) Resolve(ctx context.Context, tenantID int64, agentID string, person domain.PrincipalRef, at time.Time) (domain.PilotMember, bool, error) {
	return function(ctx, tenantID, agentID, person, at)
}

// PositionResolverFunc adapts a function to contract.PositionResolver.
type PositionResolverFunc func(context.Context, int64, domain.PrincipalRef, time.Time) ([]string, error)

// Positions invokes the configured function.
func (function PositionResolverFunc) Positions(ctx context.Context, tenantID int64, person domain.PrincipalRef, at time.Time) ([]string, error) {
	return function(ctx, tenantID, person, at)
}

var (
	_ contract.PilotResolver    = PilotResolverFunc(nil)
	_ contract.PositionResolver = PositionResolverFunc(nil)
)
