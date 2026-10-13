package dataplane

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

const qStudioLimitsRelease = "scout_studio_limits_release"

var studioLimitsQueries = map[string]string{
	// The stable release, and the agent's turns opened in the last minute by the database clock.
	qStudioLimitsRelease: `
SELECT d.stable_version,
       (SELECT COUNT(*)
          FROM conversation_turn t
          JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
         WHERE t.partner_id = d.partner_id AND c.agent_id = d.agent_id
           AND t.queued_at > CURRENT_TIMESTAMP - INTERVAL '1 minute')
  FROM agent_deployment d
 WHERE d.partner_id = ? AND d.agent_id = ?`,
}

// StudioLimits reports the execution limits and budget frozen into an agent's stable release
// with the agent's current use: the turns it opened in the last minute and its spend in the
// budget's current period, read from the ledger that enforces it.
type StudioLimits struct {
	DB         port.DatabaseRepository
	Authorizer contract.StudioAuthorizer
	// ReleaseLimits reads the frozen release as the runtime does; pass the data plane's PrincipalLimits.
	ReleaseLimits contract.PrincipalLimits
	// Spend reads a period budget's use; pass the data plane's BudgetLedger.
	Spend contract.PrincipalBudgetSpend
	// Currencies checks a per-turn budget's unit; defaults to keel's catalog over DB.
	Currencies scope.CurrencyCatalog

	once sync.Once
	qs   port.QueryService
}

var _ contract.StudioLimitsBackend = (*StudioLimits)(nil)

// Limits reports an agent's stable release; an agent with none is ErrNotFound. A budget the
// ledger cannot enforce is reported with its problem instead of failing the report.
func (s *StudioLimits) Limits(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentLimitsReport, error) {
	if s.DB == nil || s.Authorizer == nil || s.ReleaseLimits == nil || s.Spend == nil {
		return domain.AgentLimitsReport{}, fmt.Errorf("studio limits: database, authorizer, release limits, and spend are required")
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.AgentLimitsReport{}, err
	}
	s.once.Do(func() {
		s.qs = s.DB.GetQueryService(ctx, studioLimitsQueries)
		s.Currencies = scope.Currencies(s.Currencies, s.DB)
	})
	stable, err := s.qs.Query(ctx, qStudioLimitsRelease, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentLimitsReport{}, fmt.Errorf("read the stable release of %q: %w", agentID, err)
	}
	if len(stable.Rows) == 0 {
		return domain.AgentLimitsReport{}, fmt.Errorf("%w: agent %q has no published release", domain.ErrNotFound, agentID)
	}
	report := domain.AgentLimitsReport{
		AgentID: agentID, Release: common.AsString(stable.Rows[0][0]), TurnsLastMinute: common.AsInt64(stable.Rows[0][1]),
	}
	principal := domain.Principal{Kind: domain.PrincipalAgent, ID: agentID, TenantID: actor.TenantID, Release: report.Release}
	if report.Limits, err = s.ReleaseLimits.ExecutionLimits(ctx, principal); err != nil {
		return domain.AgentLimitsReport{}, err
	}
	budget, err := s.ReleaseLimits.Budget(ctx, principal)
	if err != nil {
		return domain.AgentLimitsReport{}, err
	}
	if !budget.Bounded() {
		return report, nil
	}
	report.Budget = &budget
	if budget.Period.Kind == domain.BudgetPerTurn {
		_, err = scope.CheckBudgetUnit(ctx, s.Currencies, budget)
	} else {
		var spend domain.BudgetSpend
		if spend, err = s.Spend.Spend(ctx, actor.TenantID, domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: agentID}, budget); err == nil {
			report.Spend = &spend
		}
	}
	var unenforceable *domain.BudgetUnenforceableError
	if errors.As(err, &unenforceable) {
		report.BudgetProblem = unenforceable.Problem
		return report, nil
	}
	if err != nil {
		return domain.AgentLimitsReport{}, err
	}
	return report, nil
}
