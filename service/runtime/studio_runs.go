package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioRuns pages one agent's settled runs, and traces one of them, for a Studio actor
// who may view the agent. Traces is optional; without it a trace is ErrNotReady.
type StudioRuns struct {
	Query      contract.AgentRunQuery
	Traces     contract.RunTraceQuery
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

// Trace assembles one run of the agent in the actor's tenant; a run of an agent the actor
// cannot VIEW, of another agent, or of another tenant reads as not found.
func (s StudioRuns) Trace(ctx context.Context, actor domain.StudioActor, agentID string, runID int64) (domain.RunTrace, error) {
	if s.Authorizer == nil {
		return domain.RunTrace{}, fmt.Errorf("studio runs: authorizer is required")
	}
	if s.Traces == nil {
		return domain.RunTrace{}, fmt.Errorf("%w: run traces are not composed", domain.ErrNotReady)
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" || runID <= 0 {
		return domain.RunTrace{}, fmt.Errorf("%w: agent_name and a positive run_id are required", domain.ErrValidation)
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			return domain.RunTrace{}, fmt.Errorf("%w: run %d", domain.ErrNotFound, runID)
		}
		return domain.RunTrace{}, err
	}
	return s.Traces.Trace(ctx, actor.TenantID, agentID, runID)
}
