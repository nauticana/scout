package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/isolation"
)

const (
	qSettleOperatorUsage = "scout_turn_settle_operator_usage"
	qSettleOperatorActor = "scout_turn_settle_operator_actor"
)

// turnSettlementQueries read the operator spend a turn's end aggregates; every transaction that
// ends a turn carries them beside TurnLedgerQueries.
var turnSettlementQueries = map[string]string{
	qSettleOperatorUsage: `
SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(tool_calls), 0),
       COALESCE(SUM(search_queries), 0), COALESCE(SUM(cost_minor_units), 0), COUNT(DISTINCT currency_code), COALESCE(MAX(currency_code), '')
  FROM turn_operator_entry
 WHERE partner_id = ? AND request_id = ? AND kind_code = ` + sqlOperatorTool,
	// The person whose action came last is the one the operator line is attributed to.
	qSettleOperatorActor: `
SELECT actor_kind, actor_id
  FROM turn_operator_entry
 WHERE partner_id = ? AND request_id = ? AND kind_code = ` + sqlOperatorTool + `
 ORDER BY entry_no DESC
 LIMIT 1`,
}

// TurnBudgetSettler settles a turn's reservation inside the transaction that ends the turn, after
// that transaction's guarded touch of the owner's turn row; a reservation no longer held is
// domain.ErrConflict and leaves the transaction usable.
type TurnBudgetSettler interface {
	SettleTurnTx(ctx context.Context, tx port.TxQueryService, reservation domain.BudgetReservation, usage domain.Usage, refund bool) error
}

var _ TurnBudgetSettler = (*isolation.BudgetLedger)(nil)

// TurnSettlementWriter writes a turn's settlement in the transaction that records the turn's end,
// so the reservation is never settled while that end can still be refused: a fence that moved
// after the guarded touch is impossible, and one that moved before refuses the whole unit.
type TurnSettlementWriter struct {
	Budget TurnBudgetSettler
	// UsageCategory labels the usage events of a settled turn's agent, and OperatorUsageCategory,
	// which must differ, the one line of what people spent operating it.
	UsageCategory         string
	OperatorUsageCategory string
}

func (writer TurnSettlementWriter) validate() error {
	usage, operator := strings.TrimSpace(writer.UsageCategory), strings.TrimSpace(writer.OperatorUsageCategory)
	if writer.Budget == nil || usage == "" || operator == "" || usage == operator {
		return fmt.Errorf("%w: a turn settlement needs a budget settler and distinct agent and operator usage categories", domain.ErrValidation)
	}
	return nil
}

// write settles the reservation and writes the usage events of turn turnNo inside tx, whose first
// statement was the guarded touch of the turn's owner or holder: the agent's lines, and one
// operator line aggregating the turn's operator tool calls, which a refund does not skip because
// each call was settled against the tenant when it was made. The returned unheld error is a budget
// transition the reservation's state refused, for the caller to accept or refuse its end with; err
// is any other failure.
func (writer TurnSettlementWriter) write(ctx context.Context, tx port.TxQueryService, tenantID int64, requestID, conversationID string, turnNo int64, settlement domain.TurnSettlement) (unheld, err error) {
	if err = writer.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(conversationID) == "" || strings.TrimSpace(requestID) == "" || turnNo <= 0 {
		return nil, fmt.Errorf("%w: a settlement names its turn", domain.ErrValidation)
	}
	if settlement.Reservation.ReservationID != "" {
		if settlement.Reservation.TenantID != tenantID {
			return nil, fmt.Errorf("%w: the reservation of tenant %d cannot settle a turn of tenant %d", domain.ErrForbidden, settlement.Reservation.TenantID, tenantID)
		}
		budgetErr := writer.Budget.SettleTurnTx(ctx, tx, settlement.Reservation, settlement.Usage, settlement.Refund)
		switch {
		case errors.Is(budgetErr, domain.ErrConflict):
			unheld = fmt.Errorf("settle turn budget: %w", budgetErr)
		case budgetErr != nil:
			return nil, fmt.Errorf("settle turn budget: %w", budgetErr)
		}
	}
	if err = writer.writeOperatorLine(ctx, tx, tenantID, requestID, conversationID, turnNo, settlement); err != nil {
		return nil, err
	}
	if settlement.Refund {
		return unheld, nil
	}
	usage := settlement.Usage
	if len(usage.Currency) != 3 || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.SearchQueries < 0 || usage.CostMinorUnits < 0 {
		return nil, fmt.Errorf("%w: turn identity and non-negative priced usage are required", domain.ErrValidation)
	}
	lines, err := usageLines(usage)
	if err != nil {
		return nil, err
	}
	// The guarded touch already serializes this turn's settlement, so the usage lock is not taken.
	ledger := TurnLedger{UsageCategory: writer.UsageCategory}
	if _, err = ledger.insertUsageLines(ctx, tx, false, tenantID, conversationID, turnNo, settlement.SubjectRef, settlement.Attribution, lines); err != nil {
		return nil, err
	}
	return unheld, nil
}

// writeOperatorLine writes the turn's one operator usage line, attributed to the person who acted
// last and kept on behalf of the turn's end user; a turn no person called a tool in writes none.
func (writer TurnSettlementWriter) writeOperatorLine(ctx context.Context, tx port.TxQueryService, tenantID int64, requestID, conversationID string, turnNo int64, settlement domain.TurnSettlement) error {
	totals, err := tx.Query(ctx, qSettleOperatorUsage, tenantID, requestID)
	if err != nil {
		return fmt.Errorf("read operator usage: %w", err)
	}
	if len(totals.Rows) == 0 || len(totals.Rows[0]) < 8 || common.AsInt64(totals.Rows[0][0]) == 0 {
		return nil
	}
	row := totals.Rows[0]
	if common.AsInt64(row[6]) != 1 {
		return fmt.Errorf("%w: turn %q has operator spend in more than one currency", domain.ErrValidation, requestID)
	}
	usage := domain.Usage{
		InputTokens: common.AsInt64(row[1]), OutputTokens: common.AsInt64(row[2]), ToolCalls: int(common.AsInt64(row[3])),
		SearchQueries: common.AsInt64(row[4]), CostMinorUnits: common.AsInt64(row[5]), Currency: common.AsString(row[7]),
	}
	actor, err := tx.Query(ctx, qSettleOperatorActor, tenantID, requestID)
	if err != nil {
		return fmt.Errorf("read operator usage actor: %w", err)
	}
	if len(actor.Rows) == 0 || len(actor.Rows[0]) < 2 {
		return fmt.Errorf("%w: turn %q has operator spend without its person", domain.ErrConflict, requestID)
	}
	attribution := domain.UsageAttribution{
		Principal:  domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(actor.Rows[0][0])), ID: common.AsString(actor.Rows[0][1])},
		ScopeID:    settlement.Attribution.ScopeID,
		OnBehalfOf: settlement.Attribution.OnBehalfOf, WorkRef: settlement.Attribution.WorkRef,
	}
	ledger := TurnLedger{UsageCategory: writer.OperatorUsageCategory}
	_, err = ledger.insertUsageLines(ctx, tx, false, tenantID, conversationID, turnNo, settlement.SubjectRef, attribution, []usageLine{{usage: usage}})
	return err
}
