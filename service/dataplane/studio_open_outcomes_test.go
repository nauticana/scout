package dataplane

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

// everyAgentRoles holds AGENT_OPERATION VIEW through a grant that bypasses the agent scope.
type everyAgentRoles struct{ values *[]string }

func (roles everyAgentRoles) Authorize(_ context.Context, _ domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
	*roles.values = append(*roles.values, value)
	allowed := object == domain.OperationAuthorizationObject && action == string(domain.OperationView)
	return domain.AuthorizationGrant{Allowed: allowed, LowLimit: "*", BypassScope: allowed}, nil
}

func openOutcomeRow(requestID string, step int64, callID string, status domain.CallOutcomeStatus, deadline time.Time) []any {
	return []any{requestID, step, callID, "shop.write", "2", string(status), deadline.Add(-time.Minute), deadline, true, "writer-a", "conversation-1", int64(4)}
}

func TestOpenOutcomesPagesOldestDeadlineFirstWithinTheActorsTenant(t *testing.T) {
	ctx := context.Background()
	deadline := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fixture := newTurnControlFixture(domain.OperationView)
	fixture.query.sequence = map[string][][][]any{qOperateOpenCalls: {
		{openOutcomeRow("request:1", 11, "c:1", domain.CallUnknown, deadline), openOutcomeRow("request-2", 12, "c2", domain.CallInFlight, deadline.Add(time.Second))},
		{openOutcomeRow("request-2", 12, "c2", domain.CallInFlight, deadline.Add(time.Second))},
	}}
	page, err := fixture.service.OpenOutcomes(ctx, usageActor, domain.OpenOutcomeFilter{AgentID: "writer-a", Limit: 1})
	if err != nil || len(page.Outcomes) != 1 || page.NextBefore == "" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	first := page.Outcomes[0]
	if first.Key != (domain.CallOutcomeKey{TenantID: 7, RequestID: "request:1", ExecutionStepID: 11, CallID: "c:1"}) || first.AgentID != "writer-a" ||
		first.ConversationID != "conversation-1" || first.TurnNo != 4 || first.Status != domain.CallUnknown || !first.DeadlinePassed ||
		!first.DeadlineAt.Equal(deadline) || first.Tool.ToolID != "shop.write" {
		t.Fatalf("first outcome = %+v", first)
	}
	args := fixture.query.firstArgs(qOperateOpenCalls)
	if len(args) != 9 || args[0] != int64(7) || args[1] != "writer-a" || args[2] != "writer-a" || args[3] != "" || args[8] != 2 {
		t.Fatalf("first page args = %v", args)
	}

	next, err := fixture.service.OpenOutcomes(ctx, usageActor, domain.OpenOutcomeFilter{AgentID: "writer-a", Before: page.NextBefore, Limit: 1})
	if err != nil || len(next.Outcomes) != 1 || next.NextBefore != "" {
		t.Fatalf("last page = %+v, %v", next, err)
	}
	// The cursor is the last call returned, colons in its request and call ids included; the next page starts after it.
	args = fixture.query.args[qOperateOpenCalls][1]
	if args[3] != page.NextBefore || !args[4].(time.Time).Equal(deadline) || args[5] != "request:1" || args[6] != int64(11) || args[7] != "c:1" {
		t.Fatalf("next page args = %v", args)
	}

	sql := operateQueries[qOperateOpenCalls]
	if !strings.Contains(sql, "o.partner_id = ?") || !strings.Contains(sql, "o.status_code IN ("+sqlCallOpen+")") ||
		!strings.Contains(sql, "ORDER BY o.deadline_at, o.request_id, o.execution_step_id, o.call_id") ||
		!strings.Contains(sql, "(o.deadline_at, o.request_id, o.execution_step_id, o.call_id) > (?, ?, ?, ?)") {
		t.Fatalf("open outcome query = %s", sql)
	}
}

func TestOpenOutcomesRefusesAMalformedCursorAndAnOversizedPageBeforeReading(t *testing.T) {
	ctx := context.Background()
	fixture := newTurnControlFixture(domain.OperationView)
	for _, before := range []string{"x", "1:2:cmVx", "1:0:cmVx:c", "1:2:!!:c", "1:2::c", "1:2:cmVx:", "x:2:cmVx:c"} {
		if _, err := fixture.service.OpenOutcomes(ctx, usageActor, domain.OpenOutcomeFilter{AgentID: "writer-a", Before: before}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("cursor %q = %v, want validation", before, err)
		}
	}
	if _, err := fixture.service.OpenOutcomes(ctx, usageActor, domain.OpenOutcomeFilter{AgentID: "writer-a", Limit: DefaultLiveTurnMaxPageSize + 1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversized page = %v, want validation", err)
	}
	if slices.Contains(fixture.query.calls, qOperateOpenCalls) {
		t.Fatal("a refused page must read nothing")
	}
}

func TestOpenOutcomesAuthorizesTheAgentOrEveryAgent(t *testing.T) {
	ctx := context.Background()
	none := newTurnControlFixture()
	if _, err := none.service.OpenOutcomes(ctx, usageActor, domain.OpenOutcomeFilter{AgentID: "writer-a"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("no VIEW = %v, want forbidden", err)
	}
	// A grant on one agent does not list every agent's calls.
	scoped := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	if _, err := scoped.service.OpenOutcomes(ctx, usageActor, domain.OpenOutcomeFilter{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an agent-scoped grant across agents = %v, want forbidden", err)
	}
	hidden := newTurnControlFixture(domain.OperationView)
	hidden.service.Authorizer = allowStudio{domain.StudioEdit}
	if _, err := hidden.service.OpenOutcomes(ctx, usageActor, domain.OpenOutcomeFilter{AgentID: "writer-a"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unviewable agent = %v, want not found", err)
	}
	for _, fixture := range []*turnControlFixture{none, scoped, hidden} {
		if slices.Contains(fixture.query.calls, qOperateOpenCalls) {
			t.Fatal("a refused listing must read nothing")
		}
	}

	var values []string
	every := newTurnControlFixture()
	every.service.Roles = everyAgentRoles{values: &values}
	if _, err := every.service.OpenOutcomes(ctx, usageActor, domain.OpenOutcomeFilter{}); err != nil {
		t.Fatalf("a wildcard VIEW grant = %v", err)
	}
	if args := every.query.firstArgs(qOperateOpenCalls); args[0] != int64(7) || args[1] != "" || !slices.Equal(values, []string{"agent:*"}) {
		t.Fatalf("every agent's calls are listed within the actor's tenant: args %v, checked %v", args, values)
	}
}
