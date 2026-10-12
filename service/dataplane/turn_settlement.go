package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/isolation"
)

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
	// UsageCategory labels the usage events of a settled turn.
	UsageCategory string
}

func (writer TurnSettlementWriter) validate() error {
	if writer.Budget == nil || strings.TrimSpace(writer.UsageCategory) == "" {
		return fmt.Errorf("%w: a turn settlement needs a budget settler and a usage category", domain.ErrValidation)
	}
	return nil
}

// write settles the reservation and writes the usage events of turn turnNo inside tx, whose first
// statement was the owner's guarded touch. The returned unheld error is a budget transition the
// reservation's state refused, for the caller to accept or refuse its end with; err is any other failure.
func (writer TurnSettlementWriter) write(ctx context.Context, tx port.TxQueryService, tenantID int64, conversationID string, turnNo int64, settlement domain.TurnSettlement) (unheld, err error) {
	if err = writer.validate(); err != nil {
		return nil, err
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
	if settlement.Refund {
		return unheld, nil
	}
	usage := settlement.Usage
	if strings.TrimSpace(conversationID) == "" || turnNo <= 0 || len(usage.Currency) != 3 ||
		usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.SearchQueries < 0 || usage.CostMinorUnits < 0 {
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
