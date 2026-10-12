package isolation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

var operatorPerson = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}

// Decision 6: an operator action is held against the tenant's window as the person's: never
// charged to the turn's agent or its release budget, numbered among the person's own attempts,
// and refused when the window is spent.
func TestBudgetLedgerHoldsAnOperatorActionAgainstTheTenantOnly(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	query := &budgetQueryFake{rows: map[string][][]any{qReserveBudget: {{"op-res", expires}}, qOperatorAttempt: {{int64(3)}}}}
	ledger := newBudgetLedger(query)
	ledger.Releases = nil
	reservation, err := ledger.ReserveOperatorAction(context.Background(), domain.BudgetRequest{
		TenantID: 8, RequestID: "req-1", Principal: operatorPerson, Tokens: 1, Currency: "USD"})
	if err != nil || reservation.ReservationID != "op-res" || reservation.Attempt != 3 || reservation.Replayed {
		t.Fatalf("operator hold = %+v, %v", reservation, err)
	}
	args := query.args[qReserveBudget]
	if args[3] != "human" || args[4] != "9" || args[5] != int64(3) || args[13] != int64(10_000) || args[18] != int64(500) {
		t.Fatalf("operator hold args = %v", args)
	}
	if query.ran(qBudgetTurnPin) || query.ran(qPrincipalPeriodUse) || query.ran(qFindBudget) {
		t.Fatalf("an operator hold read the agent's budget: %v", query.names)
	}
	if attempt := query.args[qOperatorAttempt]; attempt[0] != int64(8) || attempt[1] != "req-1" {
		t.Fatalf("attempt args = %v", attempt)
	}
	denied := newBudgetLedger(&budgetQueryFake{rows: map[string][][]any{qOperatorAttempt: {{int64(1)}}}})
	if _, err = denied.ReserveOperatorAction(context.Background(), domain.BudgetRequest{TenantID: 8, RequestID: "req-1", Principal: operatorPerson, Tokens: 1, Currency: "USD"}); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("a spent window = %v", err)
	}
	if _, err = ledger.ReserveOperatorAction(context.Background(), domain.BudgetRequest{TenantID: 8, RequestID: "req-1", Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"}, Tokens: 1, Currency: "USD"}); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("an agent's operator hold = %v", err)
	}
	ended := newBudgetLedger(&budgetQueryFake{rows: map[string][][]any{qGetTurnState: {{true, dbNow}}}})
	if _, err = ended.ReserveOperatorAction(context.Background(), domain.BudgetRequest{TenantID: 8, RequestID: "req-1", Principal: operatorPerson, Tokens: 1, Currency: "USD"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an ended turn's operator hold = %v", err)
	}
}

// The turn's own hold is found among the agent's attempts only, so a person's holds of the same
// request never replay as, or renumber, the agent's.
func TestBudgetLedgerFindsTheTurnsHoldAmongTheAgentsAttempts(t *testing.T) {
	if sql := budgetQueries[qFindBudget]; !strings.Contains(sql, "principal_kind = "+sqlPrincipalAgent) {
		t.Fatalf("the turn's hold lookup = %s", sql)
	}
	if sql := budgetQueries[qOperatorAttempt]; !strings.Contains(sql, "principal_kind = "+sqlPrincipalHuman) || !strings.Contains(sql, "partner_id = ? AND request_id = ?") {
		t.Fatalf("the operator attempt numbering = %s", sql)
	}
}
