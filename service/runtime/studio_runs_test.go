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
