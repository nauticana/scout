package runtime

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioRuns pages one agent's settled runs for a Studio actor who may view the agent.
type StudioRuns struct {
	Query      contract.AgentRunQuery
	Authorizer contract.StudioAuthorizer
}

var _ contract.StudioRunsBackend = StudioRuns{}

func (s StudioRuns) Runs(ctx context.Context, actor domain.StudioActor, filter domain.AgentRunFilter) ([]domain.AgentRun, error) {
	if s.Query == nil || s.Authorizer == nil {
		return nil, fmt.Errorf("studio runs: run query and authorizer are required")
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, filter.AgentID); err != nil {
		return nil, err
	}
	return s.Query.Runs(ctx, actor.TenantID, filter)
}
