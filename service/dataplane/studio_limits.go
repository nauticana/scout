package dataplane

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qStudioLimitsRelease = "scout_studio_limits_release"
	qStudioLimitsSpend   = "scout_studio_limits_spend"
)

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
	// Tokens in every currency, cost only in the budget's, and that currency's catalog exponent.
	qStudioLimitsSpend: `
SELECT CAST(COALESCE(SUM(u.input_tokens + u.output_tokens), 0) AS BIGINT),
       CAST(COALESCE(SUM(CASE WHEN u.currency_code = ? THEN u.cost_minor_units ELSE 0 END), 0) AS BIGINT),
       (SELECT c.exponent FROM currency c WHERE c.code = ?)
  FROM usage_event u
  JOIN agent_conversation conv ON conv.partner_id = u.partner_id AND conv.conversation_id = u.conversation_id
 WHERE u.partner_id = ? AND conv.agent_id = ? AND u.occurred_at >= ? AND u.occurred_at < ?`,
}

// StudioLimits reports the execution limits and budget frozen into an agent's stable release
// with the agent's current use: the turns it opened in the last minute and its settled usage
// in the budget's rolling window.
type StudioLimits struct {
	DB         port.DatabaseRepository
	Authorizer contract.StudioAuthorizer
	// ReleaseLimits reads the frozen release as the runtime does; pass the data plane's PrincipalLimits.
	ReleaseLimits contract.PrincipalLimits
	Now           func() time.Time

	once sync.Once
	qs   port.QueryService
}

var _ contract.StudioLimitsBackend = (*StudioLimits)(nil)

// Limits reports an agent's stable release; an agent with none is ErrNotFound.
func (s *StudioLimits) Limits(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentLimitsReport, error) {
	if s.DB == nil || s.Authorizer == nil || s.ReleaseLimits == nil {
		return domain.AgentLimitsReport{}, fmt.Errorf("studio limits: database, authorizer, and release limits are required")
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.AgentLimitsReport{}, err
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, studioLimitsQueries) })
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
	if budget.WindowTokens == 0 && budget.WindowCostMinorUnits == 0 {
		return report, nil
	}
	report.Budget = &budget
	if budget.Window > 0 {
		if report.Spend, err = s.spend(ctx, actor.TenantID, agentID, budget); err != nil {
			return domain.AgentLimitsReport{}, err
		}
	}
	return report, nil
}

// spend sums the agent's settled usage in the budget's trailing window; cost counts only
// usage in the budget's currency, reported with that currency's catalog exponent.
func (s *StudioLimits) spend(ctx context.Context, tenantID int64, agentID string, budget domain.BudgetLimits) (*domain.BudgetSpend, error) {
	end := s.now().UTC()
	start := end.Add(-budget.Window)
	result, err := s.qs.Query(ctx, qStudioLimitsSpend, budget.Currency, budget.Currency, tenantID, agentID, start, end)
	if err != nil {
		return nil, fmt.Errorf("read the budget spend of %q: %w", agentID, err)
	}
	if len(result.Rows) == 0 {
		return nil, fmt.Errorf("read the budget spend of %q: no aggregate row", agentID)
	}
	row := result.Rows[0]
	spend := &domain.BudgetSpend{
		PeriodStart: start, PeriodEnd: end, Tokens: common.AsInt64(row[0]),
		CostMinorUnits: common.AsInt64(row[1]), Currency: budget.Currency,
	}
	if budget.Currency == "" {
		return spend, nil
	}
	exponent, known := common.AsInt64OK(row[2])
	if !known {
		return nil, fmt.Errorf("%w: budget currency %q is not in the currency catalog", domain.ErrValidation, budget.Currency)
	}
	spend.Exponent = int(exponent)
	return spend, nil
}

func (s *StudioLimits) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
