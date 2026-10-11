package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func modelLedger(query *ledgerQueryFake, hooked *domain.Usage) *TurnLedger {
	return &TurnLedger{
		DB: ledgerDBFake{query: query}, UsageCategory: "agent_turn",
		Budget: &fake.TenantBudgetManager{
			CommitFunc:  func(context.Context, domain.BudgetReservation, domain.Usage) error { return nil },
			ReleaseFunc: func(context.Context, domain.BudgetReservation) error { return nil },
		},
		OnSettled: func(_ context.Context, _ keelport.QueryService, _ TurnState, usage domain.Usage) error {
			*hooked = usage
			return nil
		},
	}
}

func insertedModels(query *ledgerQueryFake) []domain.ModelReference {
	var models []domain.ModelReference
	for _, args := range query.calls[qLedgerInsertUsageEvent] {
		model := domain.ModelReference{}
		if args[17] != nil {
			model = domain.ModelReference{ProviderID: args[17].(string), ModelID: args[18].(string)}
		}
		models = append(models, model)
	}
	return models
}

func TestTurnLedgerCompleteSettlesUsagePerModel(t *testing.T) {
	query := &ledgerQueryFake{}
	var hooked domain.Usage
	ledger := modelLedger(query, &hooked)
	usage := domain.Usage{InputTokens: 30, OutputTokens: 5, SearchQueries: 1, CostMinorUnits: 7, Currency: "USD",
		Models: []domain.ModelSpend{
			{Model: gpt, InputTokens: 10, CostMinorUnits: 2},
			{Model: claude, InputTokens: 20, OutputTokens: 5, SearchQueries: 1, CostMinorUnits: 5},
		}}
	execution := TurnExecution{
		Turn:        TurnState{TenantID: 7, ConversationID: "c", TurnNo: 1, RequestID: "r", AgentID: "writer", AgentVersion: "v1"},
		Reservation: domain.BudgetReservation{TenantID: 7, ReservationID: "res-1"},
	}
	if err := ledger.Complete(context.Background(), execution, "text", "done", usage, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var staged []domain.ModelSpend
	if err := json.Unmarshal([]byte(query.args[qLedgerStageSuccess][8].(string)), &staged); err != nil || !reflect.DeepEqual(staged, usage.Models) {
		t.Fatalf("staged breakdown = %+v, %v", staged, err)
	}
	if got := insertedModels(query); !reflect.DeepEqual(got, []domain.ModelReference{claude, gpt}) {
		t.Fatalf("usage events per model = %+v", got)
	}
	if claudeLine := query.calls[qLedgerInsertUsageEvent][0]; claudeLine[11] != int64(20) || claudeLine[14] != int64(1) || claudeLine[15] != int64(5) {
		t.Fatalf("claude usage event = %v", claudeLine)
	}
	if !reflect.DeepEqual(hooked.Models, usage.Models) || hooked.SearchQueries != 1 {
		t.Fatalf("settle hook usage = %+v", hooked)
	}
}

func recoveredTurnRow(staged any) []any {
	row := make([]any, 29)
	copy(row, []any{int64(7), "c", int64(1), "r", "running", "digest", nil, "draft", "summary", "text", `"done"`, nil,
		nil, nil, "release", "res-1", int64(30), int64(5), int64(2), int64(3), int64(7), "USD", "settled", int64(1), false, "writer", "v1", nil, staged})
	return row
}

// A crash between settling the budget and finishing the turn must settle the same
// per-model rows the live path would have.
func TestTurnLedgerRecoveryAttributesTheStagedModels(t *testing.T) {
	staged := `[{"model":{"provider_id":"anthropic","model_id":"claude"},"input_tokens":30,"output_tokens":5,"search_queries":2,"cost_minor_units":7}]`
	query := &ledgerQueryFake{rows: map[string][][]any{qLedgerFindTurn: {recoveredTurnRow(staged)}}}
	var hooked domain.Usage
	ledger := modelLedger(query, &hooked)
	if _, _, replay, err := ledger.Inspect(context.Background(), 7, "r", "draft", "digest"); err != nil || !replay {
		t.Fatalf("Inspect = %v, replay %v", err, replay)
	}
	if got := insertedModels(query); !reflect.DeepEqual(got, []domain.ModelReference{claude, {}}) {
		t.Fatalf("recovered usage events = %+v", got)
	}
	if event := query.calls[qLedgerInsertUsageEvent][0]; event[11] != int64(30) || event[14] != int64(2) || event[15] != int64(7) {
		t.Fatalf("recovered usage event = %v", event)
	}
	if hooked.ToolCalls != 2 || hooked.SearchQueries != 3 || len(hooked.Models) != 1 {
		t.Fatalf("recovered settle hook usage = %+v", hooked)
	}
	remainder := query.calls[qLedgerInsertUsageEvent][1]
	if remainder[13] != 2 || remainder[14] != int64(1) {
		t.Fatalf("recovered unattributed usage = %v", remainder)
	}

	unattributed := &ledgerQueryFake{rows: map[string][][]any{qLedgerFindTurn: {recoveredTurnRow(nil)}}}
	if _, _, _, err := modelLedger(unattributed, &hooked).Inspect(context.Background(), 7, "r", "draft", "digest"); err != nil {
		t.Fatalf("Inspect without a breakdown: %v", err)
	}
	if got := insertedModels(unattributed); !reflect.DeepEqual(got, []domain.ModelReference{{}}) {
		t.Fatalf("a turn staged without models writes one unattributed event: %+v", got)
	}
}

func TestTurnLedgerRefusesAMalformedOrOversizedStagedBreakdown(t *testing.T) {
	var hooked domain.Usage
	malformed := &ledgerQueryFake{rows: map[string][][]any{qLedgerFindTurn: {recoveredTurnRow("{not json")}}}
	if _, _, _, err := modelLedger(malformed, &hooked).Inspect(context.Background(), 7, "r", "draft", "digest"); err == nil {
		t.Fatal("a malformed staged breakdown must not settle")
	}
	oversized := `[{"model":{"provider_id":"anthropic","model_id":"claude"},"input_tokens":31}]`
	exceeding := &ledgerQueryFake{rows: map[string][][]any{qLedgerFindTurn: {recoveredTurnRow(oversized)}}}
	if _, _, _, err := modelLedger(exceeding, &hooked).Inspect(context.Background(), 7, "r", "draft", "digest"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a breakdown over the staged totals = %v, want validation", err)
	}
	if len(malformed.calls[qLedgerInsertUsageEvent])+len(exceeding.calls[qLedgerInsertUsageEvent]) != 0 {
		t.Fatal("a refused breakdown wrote usage events")
	}
}

func TestTurnLedgerStagingRefusesABreakdownOverTheTotals(t *testing.T) {
	query := &ledgerQueryFake{}
	var hooked domain.Usage
	ledger := modelLedger(query, &hooked)
	execution := TurnExecution{
		Turn:        TurnState{TenantID: 7, ConversationID: "c", TurnNo: 1, RequestID: "r", AgentID: "writer", AgentVersion: "v1"},
		Reservation: domain.BudgetReservation{TenantID: 7, ReservationID: "res-1"},
	}
	bad := domain.Usage{InputTokens: 1, Currency: "USD", Models: []domain.ModelSpend{{Model: claude, InputTokens: 2}}}
	cause := errors.New("unusable answer")
	if err := ledger.FailWithUsage(context.Background(), execution, cause, bad); !errors.Is(err, domain.ErrValidation) || !errors.Is(err, cause) {
		t.Fatalf("FailWithUsage = %v", err)
	}
	if _, err := StageJobUsage(context.Background(), query, 7, 9, "draft", "res-1", bad); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("StageJobUsage = %v", err)
	}
	if len(query.queries) != 0 {
		t.Fatalf("a refused breakdown reached the database: %v", query.queries)
	}

	good := domain.Usage{InputTokens: 2, Currency: "USD"}.AttributedTo(claude)
	if staged, err := StageJobUsage(context.Background(), query, 7, 9, "draft", "res-1", good); err != nil || !staged {
		t.Fatalf("StageJobUsage = %v, %v", staged, err)
	}
	if models, err := decodeStagedModels(query.args[qLedgerStageJobUsage][6]); err != nil || !reflect.DeepEqual(models, good.Models) {
		t.Fatalf("staged job breakdown = %+v, %v", models, err)
	}
	if _, err := StageJobUsage(context.Background(), query, 7, 9, "draft", "res-1", domain.Usage{InputTokens: 2, Currency: "USD"}); err != nil || query.args[qLedgerStageJobUsage][6] != nil {
		t.Fatalf("usage without a breakdown stages NULL: %v, %v", query.args[qLedgerStageJobUsage][6], err)
	}
}
