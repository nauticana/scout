package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type budgetQueryFake struct {
	rows map[string][][]any
	args map[string][]any
}

func (query *budgetQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if query.args == nil {
		query.args = make(map[string][]any)
	}
	query.args[name] = append([]any(nil), args...)
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (*budgetQueryFake) GenID() int64                   { return 0 }
func (*budgetQueryFake) Commit(context.Context) error   { return nil }
func (*budgetQueryFake) Rollback(context.Context) error { return nil }

type budgetDBFake struct {
	keelport.DatabaseRepository
	query *budgetQueryFake
}

func (db budgetDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func (db budgetDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

func newBudgetLedger(query *budgetQueryFake) *BudgetLedger {
	return &BudgetLedger{DB: budgetDBFake{query: query}, Policy: usdBudget}
}

func storedBudget(id, request, status string, tokens, cost, attempt int64, expires any, settledTokens, settledCost any, expired bool) []any {
	return []any{id, request, status, tokens, cost, "USD", expires, settledTokens, settledCost, attempt, expired}
}

type budgetPolicyFunc func(context.Context, int64) (domain.BudgetLimits, error)

func (f budgetPolicyFunc) BudgetFor(ctx context.Context, tenantID int64) (domain.BudgetLimits, error) {
	return f(ctx, tenantID)
}

var usdBudget = budgetPolicyFunc(func(context.Context, int64) (domain.BudgetLimits, error) {
	return domain.BudgetLimits{WindowTokens: 10_000, WindowCostMinorUnits: 500, Currency: "USD", Window: time.Hour}, nil
})

func TestBudgetLedgerReserveGrantsAndDenies(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	query := &budgetQueryFake{rows: map[string][][]any{qGetTurnState: {{false}}, qReserveBudget: {{"any", expires}}}}
	ledger := newBudgetLedger(query)

	reservation, err := ledger.Reserve(context.Background(), 8, "req-1", 1_000, 50, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if reservation.TenantID != 8 || reservation.RequestID != "req-1" || reservation.Attempt != 1 || reservation.GrantedTokens != 1_000 || reservation.ReservationID != "any" || !reservation.ExpiresAt.Equal(expires) {
		t.Fatalf("reservation = %+v", reservation)
	}
	args := query.args[qReserveBudget]
	if len(args) != 17 || args[0] != int64(8) || args[3] != int64(1) || args[11] != int64(10_000) || args[16] != int64(500) {
		t.Fatalf("reserve args = %v", args)
	}

	// No returned row means the window budget denied the hold.
	denied := newBudgetLedger(&budgetQueryFake{rows: map[string][][]any{qGetTurnState: {{false}}}})
	_, err = denied.Reserve(context.Background(), 8, "req-2", 1_000, 50, "USD")
	if !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("denied = %v", err)
	}
}

func TestBudgetLedgerReserveValidation(t *testing.T) {
	ledger := newBudgetLedger(&budgetQueryFake{})
	if _, err := ledger.Reserve(context.Background(), 0, "", 0, -1, "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid = %v", err)
	}
	if _, err := ledger.Reserve(context.Background(), 8, "req", 10, 1, "EUR"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("currency mismatch = %v", err)
	}
}

func TestBudgetLedgerReserveIsIdempotentByRequest(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	existing := storedBudget("reservation", "req", "held", 10, 2, 1, expires.Format(time.RFC3339), nil, nil, false)
	query := &budgetQueryFake{rows: map[string][][]any{qGetTurnState: {{false}}, qFindBudget: {existing}}}
	reservation, err := newBudgetLedger(query).Reserve(context.Background(), 8, "req", 10, 2, "USD")
	if err != nil || reservation.ReservationID != "reservation" || reservation.Attempt != 1 || !reservation.ExpiresAt.Equal(expires.Truncate(time.Second)) {
		t.Fatalf("replay = %+v, %v", reservation, err)
	}
	if _, called := query.args[qReserveBudget]; called {
		t.Fatal("replay inserted a second reservation")
	}
}

func TestBudgetLedgerRenewsExpiredReservation(t *testing.T) {
	expires := time.Now().Add(-time.Minute)
	existing := storedBudget("old", "req", "held", 10, 2, 1, expires, nil, nil, true)
	newExpiry := time.Now().Add(time.Minute)
	query := &budgetQueryFake{rows: map[string][][]any{
		qGetTurnState:    {{false}},
		qFindBudget:      {existing},
		qExpireOneBudget: {{"old"}},
		qReserveBudget:   {{"new", newExpiry}},
	}}
	reservation, err := newBudgetLedger(query).Reserve(context.Background(), 8, "req", 10, 2, "USD")
	if err != nil || reservation.ReservationID != "new" || reservation.Attempt != 2 {
		t.Fatalf("renewed = %+v, %v", reservation, err)
	}
	if args := query.args[qReserveBudget]; len(args) != 17 || args[3] != int64(2) {
		t.Fatalf("reserve args = %v", args)
	}
}

func TestBudgetLedgerRenewsAttemptExpiredBySweeper(t *testing.T) {
	expires := time.Now().Add(-time.Minute)
	query := &budgetQueryFake{rows: map[string][][]any{
		qGetTurnState:  {{false}},
		qFindBudget:    {storedBudget("old", "req", "held", 10, 2, 1, expires, nil, nil, true)},
		qGetBudget:     {storedBudget("old", "req", "expired", 10, 2, 1, expires, int64(0), int64(0), false)},
		qReserveBudget: {{"new", time.Now().Add(time.Minute)}},
	}}
	reservation, err := newBudgetLedger(query).Reserve(context.Background(), 8, "req", 10, 2, "USD")
	if err != nil || reservation.Attempt != 2 {
		t.Fatalf("renewed after sweep = %+v, %v", reservation, err)
	}
}

func TestBudgetLedgerDoesNotRenewTerminalTurn(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{qGetTurnState: {{true}}}}
	if _, err := newBudgetLedger(query).Reserve(context.Background(), 8, "req", 10, 2, "USD"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("terminal turn = %v", err)
	}
}

func TestBudgetLedgerCommitAndRelease(t *testing.T) {
	current := storedBudget("r", "req", "held", 1_000, 50, 1, time.Now().Add(time.Minute), nil, nil, false)
	query := &budgetQueryFake{rows: map[string][][]any{qGetBudget: {current}, qSettleBudget: {{"r"}}, qReleaseBudget: {{"r"}}}}
	ledger := newBudgetLedger(query)
	reservation := domain.BudgetReservation{TenantID: 8, ReservationID: "r", Currency: "USD", GrantedTokens: 1_000, GrantedCostMinorUnits: 50}

	usage := domain.Usage{InputTokens: 100, OutputTokens: 50, CostMinorUnits: 30, Currency: "USD"}
	if err := ledger.Commit(context.Background(), reservation, usage); err != nil {
		t.Fatal(err)
	}
	args := query.args[qSettleBudget]
	if len(args) != 4 || args[0] != int64(150) || args[1] != int64(30) || args[2] != int64(8) || args[3] != "r" {
		t.Fatalf("settle args = %v", args)
	}
	if err := ledger.Release(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(context.Background(), reservation, domain.Usage{InputTokens: 2_000, CostMinorUnits: 100, Currency: "USD"}); err != nil {
		t.Fatalf("overage = %v", err)
	}
	if args := query.args[qSettleBudget]; len(args) != 4 || args[0] != int64(2_000) || args[1] != int64(100) {
		t.Fatalf("overage settle args = %v", args)
	}

	// Repeating the same terminal transition is idempotent.
	settled := storedBudget("r", "req", "settled", 1_000, 50, 1, time.Now(), int64(150), int64(30), false)
	replayed := newBudgetLedger(&budgetQueryFake{rows: map[string][][]any{qGetBudget: {settled}}})
	if err := replayed.Commit(context.Background(), reservation, usage); err != nil {
		t.Fatalf("double settle = %v", err)
	}
	if err := replayed.Commit(context.Background(), reservation, domain.Usage{Currency: "EUR"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("currency = %v", err)
	}
}

func TestBudgetLedgerRejectsInvalidExpiryEncoding(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{qGetTurnState: {{false}}, qReserveBudget: {{"r", "not-a-time"}}}}
	if _, err := newBudgetLedger(query).Reserve(context.Background(), 8, "req", 10, 2, "USD"); err == nil {
		t.Fatal("invalid expiry must fail")
	}
}

func TestBudgetLedgerFencesExpiredSettlement(t *testing.T) {
	current := storedBudget("r", "req", "held", 100, 10, 1, time.Now().Add(-time.Minute), nil, nil, true)
	query := &budgetQueryFake{rows: map[string][][]any{qGetBudget: {current}, qExpireOneBudget: {{"r"}}}}
	reservation := domain.BudgetReservation{TenantID: 8, ReservationID: "r", Currency: "USD"}
	err := newBudgetLedger(query).Commit(context.Background(), reservation, domain.Usage{CostMinorUnits: 1, Currency: "USD"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired commit = %v", err)
	}
	if _, called := query.args[qSettleBudget]; called {
		t.Fatal("expired reservation settled")
	}
}

func TestBudgetLedgerExpireIsBounded(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{qExpireBudget: {{"a"}, {"b"}}}}
	ledger := newBudgetLedger(query)
	expired, err := ledger.Expire(context.Background(), 100)
	if err != nil || expired != 2 {
		t.Fatalf("expired = %d, %v", expired, err)
	}
	if args := query.args[qExpireBudget]; len(args) != 1 || args[0] != 100 {
		t.Fatalf("expire args = %v", args)
	}
	if _, err := ledger.Expire(context.Background(), 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("limit = %v", err)
	}
}
