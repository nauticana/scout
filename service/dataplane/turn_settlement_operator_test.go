package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

var operatorSettlementWriter = TurnSettlementWriter{Budget: &settlerFake{}, UsageCategory: "model_output", OperatorUsageCategory: "operator"}

// Decision 6 and §8.17: a turn's end writes one operator line, the sum of every operator tool
// call, attributed to the person who acted last on behalf of the turn's end user, beside the
// agent's line; a refunded turn still writes it, since each call was settled when it was made.
func TestTurnSettlementWritesOneOperatorLine(t *testing.T) {
	ctx := context.Background()
	settlement := domain.TurnSettlement{
		Reservation: domain.BudgetReservation{TenantID: 7, ReservationID: "res-1"}, Refund: true, SubjectRef: "writer-a@v3",
		Attribution: domain.UsageAttribution{Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"}, ScopeID: "root",
			OnBehalfOf: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "end-user-5"}, WorkRef: "case-1"},
	}
	query := &queueQueryFake{rows: map[string][][]any{
		qSettleOperatorUsage: {{int64(3), int64(0), int64(0), int64(3), int64(2), int64(160), int64(1), "USD"}},
		qSettleOperatorActor: {{"human", "9"}},
	}}
	if _, err := operatorSettlementWriter.write(ctx, query, 7, "request-1", "conversation-1", 4, settlement); err != nil {
		t.Fatal(err)
	}
	lines := query.args[qLedgerInsertUsageEvent]
	if len(lines) != 1 {
		t.Fatalf("usage lines = %v", lines)
	}
	line := lines[0]
	if line[3] != "operator" || line[4] != "writer-a@v3" || line[5] != "human" || line[6] != "9" || line[7] != "root" ||
		line[8] != "human" || line[9] != "end-user-5" || line[10] != "case-1" || line[13] != 3 || line[14] != int64(2) ||
		line[15] != int64(160) || line[16] != "USD" || line[17] != nil {
		t.Fatalf("operator line = %v", line)
	}
	if args := query.firstArgs(qSettleOperatorUsage); args[0] != int64(7) || args[1] != "request-1" {
		t.Fatalf("operator usage read for %v", args)
	}

	none := &queueQueryFake{rows: map[string][][]any{qSettleOperatorUsage: {{int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), ""}}}}
	if _, err := operatorSettlementWriter.write(ctx, none, 7, "request-1", "conversation-1", 4, settlement); err != nil || len(none.args[qLedgerInsertUsageEvent]) != 0 {
		t.Fatalf("a turn without operator calls wrote %v, %v", none.args[qLedgerInsertUsageEvent], err)
	}
	mixed := &queueQueryFake{rows: map[string][][]any{qSettleOperatorUsage: {{int64(2), int64(0), int64(0), int64(2), int64(0), int64(10), int64(2), "USD"}}}}
	if _, err := operatorSettlementWriter.write(ctx, mixed, 7, "request-1", "conversation-1", 4, settlement); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("operator spend in two currencies = %v", err)
	}
	for _, writer := range []TurnSettlementWriter{
		{Budget: &settlerFake{}, UsageCategory: "model_output"},
		{Budget: &settlerFake{}, UsageCategory: "model_output", OperatorUsageCategory: "model_output"},
	} {
		if _, err := writer.write(ctx, query, 7, "request-1", "conversation-1", 4, settlement); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("a writer without a distinct operator category = %v", err)
		}
	}
}
