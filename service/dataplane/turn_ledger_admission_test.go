package dataplane

import (
	"context"
	"errors"
	"reflect"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestTurnLedgerBeginAppliesAdmissionBeforePersistence(t *testing.T) {
	denied := domain.ErrRateLimited
	var tenantID int64
	ledger := &TurnLedger{Admission: &fake.TenantRateLimiter{AllowTurnFunc: func(_ context.Context, tenant domain.TenantContext) error {
		tenantID = tenant.TenantID
		return denied
	}}}
	_, _, err := ledger.Begin(context.Background(), 7, "user", "request", "task", "input",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "running",
		domain.AgentReleaseReference{AgentID: "agent", Version: "1", Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	if !errors.Is(err, denied) || tenantID != 7 {
		t.Fatalf("admission = tenant %d, error %v", tenantID, err)
	}
}

type ledgerQueryFake struct {
	queries []string
	args    map[string][]any
	commits int
}

func (query *ledgerQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	query.queries = append(query.queries, name)
	if query.args == nil {
		query.args = map[string][]any{}
	}
	query.args[name] = args
	return &keelmodel.QueryResult{Rows: [][]any{{int64(1)}}}, nil
}

func (*ledgerQueryFake) GenID() int64                       { return 0 }
func (query *ledgerQueryFake) Commit(context.Context) error { query.commits++; return nil }
func (*ledgerQueryFake) Rollback(context.Context) error     { return nil }

type ledgerDBFake struct {
	keelport.DatabaseRepository
	query *ledgerQueryFake
}

func (db ledgerDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func (db ledgerDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

// A model that answered with something unusable still spent the tokens: the turn
// fails, but the reservation settles at the real usage and the usage event is written.
func TestTurnLedgerFailWithUsageSettlesInsteadOfReleasing(t *testing.T) {
	query := &ledgerQueryFake{}
	var committed domain.Usage
	released, settledHook := 0, 0
	ledger := &TurnLedger{
		DB: ledgerDBFake{query: query}, UsageCategory: "agent_turn",
		Budget: &fake.TenantBudgetManager{
			CommitFunc: func(_ context.Context, _ domain.BudgetReservation, usage domain.Usage) error {
				committed = usage
				return nil
			},
			ReleaseFunc: func(context.Context, domain.BudgetReservation) error { released++; return nil },
		},
		OnSettled: func(context.Context, keelport.QueryService, TurnState, domain.Usage) error { settledHook++; return nil },
	}
	execution := TurnExecution{
		Turn:        TurnState{TenantID: 7, ConversationID: "c", TurnNo: 1, RequestID: "r", AgentID: "writer", AgentVersion: "v1"},
		Reservation: domain.BudgetReservation{TenantID: 7, ReservationID: "res-1"},
	}
	usage := domain.Usage{InputTokens: 40, OutputTokens: 9, CostMinorUnits: 3, Currency: "USD"}
	cause := errors.Join(domain.ErrInvalidModelOutput)
	if err := ledger.FailWithUsage(context.Background(), execution, cause, usage); !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("FailWithUsage must return the cause, got %v", err)
	}
	want := []string{qLedgerStageBilledFailure, qLedgerFinishBilledFailure, qLedgerInsertUsageEvent}
	if !reflect.DeepEqual(query.queries, want) {
		t.Fatalf("queries = %v, want %v", query.queries, want)
	}
	if committed != usage || released != 0 || settledHook != 1 || query.commits != 1 {
		t.Fatalf("committed %+v released %d hook %d commits %d", committed, released, settledHook, query.commits)
	}
	if event := query.args[qLedgerInsertUsageEvent]; event[8] != int64(40) || event[12] != int64(3) {
		t.Fatalf("usage event args = %v", event)
	}

	// Without usage there is nothing to bill: the ordinary release path runs.
	query.queries = nil
	if err := ledger.FailWithUsage(context.Background(), execution, cause, domain.Usage{}); !errors.Is(err, domain.ErrInvalidModelOutput) || released != 1 {
		t.Fatalf("zero-usage failure = %v, released %d", err, released)
	}
}
