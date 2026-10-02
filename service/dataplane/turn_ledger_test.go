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

type ledgerQueryFake struct {
	queries []string
	args    map[string][]any
	commits int
}

type ledgerDBFake struct {
	keelport.DatabaseRepository
	query *ledgerQueryFake
}

// A model that answered with something unusable still spent the tokens: the turn
// fails, but the reservation settles at the real usage and the usage event is written.
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
func (db ledgerDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}
func (db ledgerDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

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

func TestTurnLedgerRecordsEverySettledTurnAsARun(t *testing.T) {
	runs := &runLog{}
	ledger := &TurnLedger{
		DB: ledgerDBFake{query: &ledgerQueryFake{}}, UsageCategory: "agent_turn", Activity: runs,
		Budget: &fake.TenantBudgetManager{
			CommitFunc:  func(context.Context, domain.BudgetReservation, domain.Usage) error { return nil },
			ReleaseFunc: func(context.Context, domain.BudgetReservation) error { return nil },
		},
	}
	state := TurnState{TenantID: 7, ConversationID: "c", TurnNo: 1, RequestID: "r", TaskKind: "draft", AgentID: "writer", AgentVersion: "v1"}
	execution := TurnExecution{Turn: state, Reservation: domain.BudgetReservation{TenantID: 7, ReservationID: "res-1"}}
	cause := errors.New("model unavailable")
	_ = ledger.Fail(context.Background(), execution, cause)
	_ = ledger.FailWithUsage(context.Background(), execution, cause, domain.Usage{InputTokens: 1, Currency: "USD"})
	_ = ledger.FailUnreserved(context.Background(), state, cause)
	if len(*runs) != 3 {
		t.Fatalf("runs = %+v", *runs)
	}
	completed := TurnExecution{Turn: state, Reservation: execution.Reservation}
	if err := ledger.Complete(context.Background(), completed, "text", "done", domain.Usage{InputTokens: 2, Currency: "USD"}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(*runs) != 4 || (*runs)[3].Status != domain.RunCompleted {
		t.Fatalf("a completed turn must be recorded as a completed run: %+v", *runs)
	}
	for _, run := range (*runs)[:3] {
		if run.Status != domain.RunFailed || run.RequestID != "r" || run.TaskKind != "draft" || run.Release.Version != "v1" {
			t.Fatalf("run = %+v", run)
		}
	}
}
