package isolation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	keellimiter "github.com/nauticana/keel/limiter"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/saturate"
	"github.com/nauticana/scout/service/scope"
)

// AgentBudgetScope is the LimitError scope of an exhausted release budget.
const AgentBudgetScope = "agent_budget"

const (
	qBudgetTurnPin      = "scout_isolation_budget_turn_pin"
	qBudgetClock        = "scout_isolation_budget_clock"
	qPrincipalPeriodUse = "scout_isolation_principal_period_use"
)

// A settlement counts in the period holding its settled_at; a live hold counts in every period
// while it lives. The period has no upper bound: under the tenant lock a settlement committed
// by a later transaction may carry a later timestamp than this one's clock.
var agentBudgetQueries = map[string]string{
	qBudgetTurnPin: `
SELECT c.agent_id, c.agent_version
  FROM conversation_turn t
  JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
 WHERE t.partner_id = ? AND t.request_id = ?`,
	qBudgetClock: `SELECT CURRENT_TIMESTAMP`,
	qPrincipalPeriodUse: `
SELECT CAST(COALESCE(SUM(CASE WHEN status_code = ` + sqlReservationSettled + ` THEN settled_tokens ELSE 0 END), 0) AS BIGINT),
       CAST(COALESCE(SUM(CASE WHEN status_code = ` + sqlReservationSettled + ` AND currency_code = ? THEN settled_cost_minor_units ELSE 0 END), 0) AS BIGINT),
       CAST(COALESCE(SUM(CASE WHEN status_code = ` + sqlReservationHeld + ` THEN granted_tokens ELSE 0 END), 0) AS BIGINT),
       CAST(COALESCE(SUM(CASE WHEN status_code = ` + sqlReservationHeld + ` AND currency_code = ? THEN granted_cost_minor_units ELSE 0 END), 0) AS BIGINT)
  FROM budget_reservation
 WHERE partner_id = ? AND principal_kind = ? AND principal_id = ?
   AND (status_code = ` + sqlReservationHeld + ` AND expires_at > CURRENT_TIMESTAMP
     OR status_code = ` + sqlReservationSettled + ` AND settled_at >= ?)`,
}

// turnAgent is the conversation agent a request's hold is charged to, pinned to the
// conversation's release; a caller may only name that agent.
func (ledger *BudgetLedger) turnAgent(ctx context.Context, tenantID int64, requestID string, named domain.PrincipalRef) (domain.Principal, error) {
	pin, err := ledger.qs.Query(ctx, qBudgetTurnPin, tenantID, requestID)
	if err != nil {
		return domain.Principal{}, fmt.Errorf("reserve budget: find the turn's agent: %w", err)
	}
	if len(pin.Rows) == 0 {
		return domain.Principal{}, fmt.Errorf("%w: turn request %q does not exist", domain.ErrNotFound, requestID)
	}
	agentID, version := common.AsString(pin.Rows[0][0]), common.AsString(pin.Rows[0][1])
	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(version) == "" {
		return domain.Principal{}, fmt.Errorf("%w: turn request %q has no pinned agent release", domain.ErrNotReady, requestID)
	}
	if (named.Kind != "" || named.ID != "") && (named.Kind != domain.PrincipalAgent || named.ID != agentID) {
		return domain.Principal{}, fmt.Errorf("%w: budget principal %s %q is not the turn's agent", domain.ErrForbidden, named.Kind, named.ID)
	}
	return domain.Principal{Kind: domain.PrincipalAgent, ID: agentID, TenantID: tenantID, Release: version}, nil
}

// enforceable refuses a bounded budget the ledger cannot count: an invalid period, and a cost
// ceiling without a currency or in another currency than the hold's.
func enforceable(budget domain.ReleaseBudget, currency string) error {
	if !budget.Bounded() {
		return nil
	}
	if err := budget.Period.Validate(); err != nil {
		return &domain.BudgetUnenforceableError{Problem: domain.BudgetInvalidPeriod, Detail: err.Error()}
	}
	if budget.CostMinorUnits > 0 && budget.Currency == "" {
		return &domain.BudgetUnenforceableError{Problem: domain.BudgetNoCurrency, Detail: "the cost ceiling names no currency"}
	}
	if budget.CostMinorUnits > 0 && budget.Currency != currency {
		return &domain.BudgetUnenforceableError{Problem: domain.BudgetCurrencyMismatch,
			Detail: "the cost ceiling is in " + budget.Currency + ", the hold in " + currency}
	}
	return nil
}

// turnGrant applies a per-turn ceiling: a governed turn stops its work at the ceiling, so its
// hold is clamped to it; any other hold above the ceiling is refused.
func turnGrant(budget domain.ReleaseBudget, governed bool, tokens, cost int64) (int64, int64, error) {
	if budget.Period.Kind != domain.BudgetPerTurn || !budget.Bounded() {
		return tokens, cost, nil
	}
	clamp := func(requested, ceiling int64, what string) (int64, error) {
		if ceiling <= 0 || requested <= ceiling {
			return requested, nil
		}
		if governed {
			return ceiling, nil
		}
		return 0, fmt.Errorf("%w: %d %s exceeds the agent's per-turn budget of %d", domain.ErrBudgetExceeded, requested, what, ceiling)
	}
	tokens, err := clamp(tokens, budget.Tokens, "tokens")
	if err != nil {
		return 0, 0, err
	}
	cost, err = clamp(cost, budget.CostMinorUnits, "minor units")
	return tokens, cost, err
}

type periodUse struct {
	tokens, cost, heldTokens, heldCost int64
}

func (ledger *BudgetLedger) periodUse(ctx context.Context, qs keelport.QueryService, tenantID int64, agentID, currency string, start time.Time) (periodUse, error) {
	result, err := qs.Query(ctx, qPrincipalPeriodUse, currency, currency, tenantID, string(domain.PrincipalAgent), agentID, start.UTC())
	if err != nil {
		return periodUse{}, fmt.Errorf("read the budget period use of agent %q: %w", agentID, err)
	}
	if len(result.Rows) == 0 || len(result.Rows[0]) < 4 {
		return periodUse{}, fmt.Errorf("read the budget period use of agent %q: no aggregate row", agentID)
	}
	row := result.Rows[0]
	return periodUse{tokens: common.AsInt64(row[0]), cost: common.AsInt64(row[1]), heldTokens: common.AsInt64(row[2]), heldCost: common.AsInt64(row[3])}, nil
}

// checkPeriod admits a hold only while the agent's settled and held use of its current period
// leaves room for it. The tenant advisory lock serializes it with every other reserve and settlement.
// A hold against a cost ceiling must quote cost: a zero hold would let concurrent turns overrun it.
func (ledger *BudgetLedger) checkPeriod(ctx context.Context, tx keelport.QueryService, tenantID int64, agentID string, budget domain.ReleaseBudget, now time.Time, tokens, cost int64, currency string) error {
	if !budget.Bounded() || budget.Period.Kind == domain.BudgetPerTurn {
		return nil
	}
	if budget.CostMinorUnits > 0 && cost <= 0 {
		return &domain.BudgetUnenforceableError{Problem: domain.BudgetNoCostQuote,
			Detail: fmt.Sprintf("the hold quotes no cost against the agent's %s cost ceiling", budget.Period.Kind)}
	}
	if exceeds(tokens, budget.Tokens) || exceeds(cost, budget.CostMinorUnits) {
		return fmt.Errorf("%w: a hold of %d tokens and %d minor units exceeds agent %q's whole %s budget of %d tokens and %d minor units",
			domain.ErrBudgetExceeded, tokens, cost, agentID, budget.Period.Kind, budget.Tokens, budget.CostMinorUnits)
	}
	start, end, err := budget.Period.Bounds(now)
	if err != nil {
		return &domain.BudgetUnenforceableError{Problem: domain.BudgetInvalidPeriod, Detail: err.Error()}
	}
	use, err := ledger.periodUse(ctx, tx, tenantID, agentID, currency, start)
	if err != nil {
		return err
	}
	if fits(saturate.Add(use.tokens, use.heldTokens), tokens, budget.Tokens) &&
		fits(saturate.Add(use.cost, use.heldCost), cost, budget.CostMinorUnits) {
		return nil
	}
	exhausted := fmt.Errorf("%w: agent %q has spent its %s budget", domain.ErrBudgetExceeded, agentID, budget.Period.Kind)
	after := time.Duration(0)
	if budget.Period.Kind == domain.BudgetDaily || budget.Period.Kind == domain.BudgetMonthly {
		after = end.Sub(now)
	}
	return &keellimiter.LimitError{Err: exhausted, Scope: AgentBudgetScope, After: after}
}

// fits reports whether a grant fits beside what is used; a zero ceiling is not limited, and a
// spent axis admits nothing, not even an empty grant.
func fits(used, grant, ceiling int64) bool {
	return ceiling <= 0 || used < ceiling && grant <= ceiling-used
}

// exceeds reports a grant that no period of the ceiling could hold.
func exceeds(grant, ceiling int64) bool {
	return ceiling > 0 && grant > ceiling
}

// Spend reports the principal's use of the budget's current period by the database clock:
// usage settled in it and holds still live, apart. A rolling period ends at the database's now.
func (ledger *BudgetLedger) Spend(ctx context.Context, tenantID int64, principal domain.PrincipalRef, budget domain.ReleaseBudget) (domain.BudgetSpend, error) {
	if tenantID <= 0 || principal.Kind != domain.PrincipalAgent || strings.TrimSpace(principal.ID) == "" {
		return domain.BudgetSpend{}, fmt.Errorf("%w: tenant and agent are required", domain.ErrValidation)
	}
	if budget.Period.Kind == domain.BudgetPerTurn {
		return domain.BudgetSpend{}, fmt.Errorf("%w: a per-turn budget has no period spend", domain.ErrValidation)
	}
	if err := ledger.init(ctx); err != nil {
		return domain.BudgetSpend{}, err
	}
	exponent, err := scope.CheckBudgetUnit(ctx, ledger.qs, budget)
	if err != nil {
		return domain.BudgetSpend{}, err
	}
	clock, err := ledger.qs.Query(ctx, qBudgetClock)
	if err != nil {
		return domain.BudgetSpend{}, fmt.Errorf("read the database clock: %w", err)
	}
	if len(clock.Rows) == 0 {
		return domain.BudgetSpend{}, fmt.Errorf("read the database clock: no row")
	}
	now, ok := common.AsTimeOK(clock.Rows[0][0])
	if !ok {
		return domain.BudgetSpend{}, fmt.Errorf("read the database clock: invalid value %q", common.AsString(clock.Rows[0][0]))
	}
	start, end, err := budget.Period.Bounds(now)
	if err != nil {
		return domain.BudgetSpend{}, &domain.BudgetUnenforceableError{Problem: domain.BudgetInvalidPeriod, Detail: err.Error()}
	}
	use, err := ledger.periodUse(ctx, ledger.qs, tenantID, principal.ID, budget.Currency, start)
	if err != nil {
		return domain.BudgetSpend{}, err
	}
	return domain.BudgetSpend{
		PeriodStart: start, PeriodEnd: end, Tokens: use.tokens, CostMinorUnits: use.cost,
		HeldTokens: use.heldTokens, HeldCostMinorUnits: use.heldCost, Currency: budget.Currency, Exponent: exponent,
	}, nil
}

var _ contract.PrincipalBudgetSpend = (*BudgetLedger)(nil)
