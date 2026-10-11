package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

type runsQueryFunc func(context.Context, int64, domain.AgentRunFilter) ([]domain.AgentRun, error)

func (function runsQueryFunc) Runs(ctx context.Context, tenantID int64, filter domain.AgentRunFilter) ([]domain.AgentRun, error) {
	return function(ctx, tenantID, filter)
}

type viewOnly string

func (agent viewOnly) Authorize(_ context.Context, _ domain.StudioActor, action domain.StudioAction, agentID string) error {
	if action != domain.StudioView || agentID != string(agent) {
		return domain.ErrForbidden
	}
	return nil
}

func TestStudioRunsPagesOnlyAViewableAgentOfTheActorsTenant(t *testing.T) {
	actor := domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}
	runs := StudioRuns{Authorizer: viewOnly("writer-a"), Query: runsQueryFunc(func(_ context.Context, tenantID int64, filter domain.AgentRunFilter) ([]domain.AgentRun, error) {
		if tenantID != 7 || filter.AgentID != "writer-a" {
			t.Fatalf("tenant = %d, filter = %+v", tenantID, filter)
		}
		return []domain.AgentRun{{ID: 3}}, nil
	})}
	if page, err := runs.Runs(context.Background(), actor, domain.AgentRunFilter{AgentID: "writer-a", Limit: 10}); err != nil || len(page) != 1 {
		t.Fatalf("page = %+v, error = %v", page, err)
	}
	if _, err := runs.Runs(context.Background(), actor, domain.AgentRunFilter{AgentID: "writer-b", Limit: 10}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("other agent = %v, want forbidden", err)
	}
}

type traceQueryFunc func(context.Context, int64, string, int64) (domain.RunTrace, error)

func (function traceQueryFunc) Trace(ctx context.Context, tenantID int64, agentID string, runID int64) (domain.RunTrace, error) {
	return function(ctx, tenantID, agentID, runID)
}

func TestStudioRunsTracesARunOfAViewableAgentInTheActorsTenantOnly(t *testing.T) {
	actor := domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}
	asked := 0
	runs := StudioRuns{Authorizer: viewOnly("writer-a"), Traces: traceQueryFunc(func(_ context.Context, tenantID int64, agentID string, runID int64) (domain.RunTrace, error) {
		asked++
		if tenantID != 7 || agentID != "writer-a" || runID != 31 {
			t.Fatalf("trace of %d/%s/%d", tenantID, agentID, runID)
		}
		return domain.RunTrace{Run: domain.AgentRun{ID: runID}}, nil
	})}
	if trace, err := runs.Trace(context.Background(), actor, " writer-a ", 31); err != nil || trace.Run.ID != 31 {
		t.Fatalf("trace = %+v, %v", trace, err)
	}
	if _, err := runs.Trace(context.Background(), actor, "writer-b", 31); !errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an agent the actor cannot view = %v, want not found", err)
	}
	for _, refused := range []struct {
		agent string
		run   int64
	}{{"", 31}, {"writer-a", 0}, {"writer-a", -4}} {
		if _, err := runs.Trace(context.Background(), actor, refused.agent, refused.run); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%+v = %v, want validation", refused, err)
		}
	}
	if asked != 1 {
		t.Fatalf("only the authorized, valid trace reaches the query: %d", asked)
	}
	if _, err := (StudioRuns{Authorizer: viewOnly("writer-a")}).Trace(context.Background(), actor, "writer-a", 31); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("without a trace query = %v, want not ready", err)
	}
}
