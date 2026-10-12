package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var (
	claude = domain.ModelReference{ProviderID: "anthropic", ModelID: "claude"}
	gpt    = domain.ModelReference{ProviderID: "openai", ModelID: "gpt"}
)

func TestUsageLinesSplitATurnPerModelAndKeepWhatNoModelPerformed(t *testing.T) {
	usage := domain.Usage{InputTokens: 30, OutputTokens: 12, ToolCalls: 2, SearchQueries: 1, CostMinorUnits: 9, Currency: "USD",
		Models: []domain.ModelSpend{
			{Model: gpt, InputTokens: 10, OutputTokens: 2, CostMinorUnits: 4},
			{Model: claude, InputTokens: 20, OutputTokens: 10, SearchQueries: 1, CostMinorUnits: 5},
		}}
	lines, err := usageLines(usage)
	if err != nil {
		t.Fatal(err)
	}
	want := []usageLine{
		{model: claude, usage: domain.Usage{InputTokens: 20, OutputTokens: 10, SearchQueries: 1, CostMinorUnits: 5, Currency: "USD"}},
		{model: gpt, usage: domain.Usage{InputTokens: 10, OutputTokens: 2, CostMinorUnits: 4, Currency: "USD"}},
		{usage: domain.Usage{ToolCalls: 2, Currency: "USD"}},
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %+v, want %+v", lines, want)
	}
	if usage.Models[0].Model != gpt {
		t.Fatal("splitting reordered the caller's breakdown")
	}
}

func TestUsageLinesWriteOneUnattributedLineWithoutABreakdown(t *testing.T) {
	for name, usage := range map[string]domain.Usage{
		"priced":  {InputTokens: 3, OutputTokens: 4, CostMinorUnits: 1, Currency: "USD"},
		"nothing": {Currency: "USD"},
	} {
		lines, err := usageLines(usage)
		if err != nil || len(lines) != 1 || lines[0].model != (domain.ModelReference{}) || !reflect.DeepEqual(lines[0].usage, usage) {
			t.Fatalf("%s: lines = %+v, error = %v", name, lines, err)
		}
	}
	exact := domain.Usage{InputTokens: 3, Currency: "USD"}.AttributedTo(claude)
	if lines, err := usageLines(exact); err != nil || len(lines) != 1 || lines[0].model != claude {
		t.Fatalf("a fully attributed turn writes no empty remainder: %+v, %v", lines, err)
	}
}

func TestUsageLinesRefuseABreakdownThatDoesNotFitTheTotals(t *testing.T) {
	cases := map[string]domain.Usage{
		"exceeds tokens": {InputTokens: 1, Currency: "USD", Models: []domain.ModelSpend{{Model: claude, InputTokens: 2}}},
		"exceeds cost": {InputTokens: 2, CostMinorUnits: 1, Currency: "USD",
			Models: []domain.ModelSpend{{Model: claude, InputTokens: 1, CostMinorUnits: 1}, {Model: gpt, InputTokens: 1, CostMinorUnits: 1}}},
		"duplicate model": {InputTokens: 2, Currency: "USD", Models: []domain.ModelSpend{{Model: claude, InputTokens: 1}, {Model: claude, InputTokens: 1}}},
		"blank model":     {InputTokens: 1, Currency: "USD", Models: []domain.ModelSpend{{Model: domain.ModelReference{ProviderID: "anthropic"}, InputTokens: 1}}},
		"negative":        {InputTokens: 1, Currency: "USD", Models: []domain.ModelSpend{{Model: claude, OutputTokens: -1}}},
	}
	for name, usage := range cases {
		if _, err := usageLines(usage); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want validation", name, err)
		}
	}
}

func TestMergeModelSpendAddsPerModelWithoutAliasing(t *testing.T) {
	total := []domain.ModelSpend{{Model: gpt, InputTokens: 1, CostMinorUnits: 1}}
	merged, err := mergeModelSpend(total, []domain.ModelSpend{{Model: gpt, InputTokens: 2, SearchQueries: 1}, {Model: claude, OutputTokens: 3}})
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ModelSpend{{Model: claude, OutputTokens: 3}, {Model: gpt, InputTokens: 3, SearchQueries: 1, CostMinorUnits: 1}}
	if !reflect.DeepEqual(merged, want) || total[0].InputTokens != 1 {
		t.Fatalf("merged = %+v, total = %+v", merged, total)
	}
	if _, err := mergeModelSpend(merged, []domain.ModelSpend{{Model: gpt, CostMinorUnits: math.MaxInt64}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("overflow = %v, want validation", err)
	}
	if _, err := mergeModelSpend(nil, []domain.ModelSpend{{InputTokens: 1}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unnamed model = %v, want validation", err)
	}
}

func TestAddChunkUsageCarriesSearchesAndTheModelBreakdown(t *testing.T) {
	var total domain.Usage
	for _, chunk := range []domain.Usage{
		domain.Usage{InputTokens: 4, SearchQueries: 2, CostMinorUnits: 3, Currency: "USD"}.AttributedTo(claude),
		domain.Usage{OutputTokens: 6, CostMinorUnits: 2, Currency: "USD"}.AttributedTo(gpt),
		{ToolCalls: 1},
	} {
		if err := addChunkUsage(&total, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if total.SearchQueries != 2 || total.CostMinorUnits != 5 || total.ToolCalls != 1 || len(total.Models) != 2 ||
		total.Models[0].Model != claude || total.Models[0].SearchQueries != 2 || total.Models[1].OutputTokens != 6 {
		t.Fatalf("total = %+v", total)
	}
	if err := addChunkUsage(&total, domain.Usage{SearchQueries: -1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative searches = %v, want validation", err)
	}
}

func TestDurableStepAndLoopRecordsKeepTheModelBreakdown(t *testing.T) {
	usage := domain.Usage{InputTokens: 4, ToolCalls: 1, CostMinorUnits: 2, Currency: "USD"}.AttributedTo(claude)
	step, err := json.Marshal(domain.StepResult{State: []byte("s"), Usage: usage})
	if err != nil {
		t.Fatal(err)
	}
	var result domain.StepResult
	if err = json.Unmarshal(step, &result); err != nil || !reflect.DeepEqual(result.Usage, usage) {
		t.Fatalf("step result replayed %+v, error = %v", result.Usage, err)
	}
	journaled, err := json.Marshal(domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryModel, Usage: usage})
	if err != nil {
		t.Fatal(err)
	}
	var entry domain.LoopEntry
	if err = json.Unmarshal(journaled, &entry); err != nil || !reflect.DeepEqual(entry.Usage, usage) {
		t.Fatalf("loop entry replayed %+v, error = %v", entry.Usage, err)
	}
}

func TestRecordUsageWritesEveryModelOfATurnOnceUnderTheTurnLock(t *testing.T) {
	query := &queueQueryFake{
		rows:     map[string][][]any{qLedgerInsertUsageEvent: {{int64(1)}}},
		sequence: map[string][][][]any{qLedgerUsageSettled: {nil, {{int64(1)}}}},
	}
	store := newTestRecordStore(query)
	usage := domain.Usage{InputTokens: 30, OutputTokens: 12, ToolCalls: 2, CostMinorUnits: 9, Currency: "USD",
		Models: []domain.ModelSpend{
			{Model: gpt, InputTokens: 10, OutputTokens: 2, CostMinorUnits: 4},
			{Model: claude, InputTokens: 20, OutputTokens: 10, CostMinorUnits: 5},
		}}
	attribution := domain.UsageAttribution{Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "agent"}, WorkRef: "task-9"}
	if err := store.RecordUsage(context.Background(), recordOwner, "conversation-1", 4, "agent@v1", attribution, usage); err != nil {
		t.Fatal(err)
	}
	if lock := query.firstArgs(qLedgerLock); len(lock) != 1 || lock[0] != "usage:7:conversation-1:4:model_output" {
		t.Fatalf("lock = %v", lock)
	}
	inserts := query.args[qLedgerInsertUsageEvent]
	want := [][]any{
		{int64(20), int64(10), 0, int64(0), int64(5), "USD", "anthropic", "claude"},
		{int64(10), int64(2), 0, int64(0), int64(4), "USD", "openai", "gpt"},
		{int64(0), int64(0), 2, int64(0), int64(0), "USD", nil, nil},
	}
	if len(inserts) != len(want) {
		t.Fatalf("inserts = %v", inserts)
	}
	for index, args := range inserts {
		if args[3] != "model_output" || args[10] != "task-9" || !reflect.DeepEqual(args[11:], want[index]) {
			t.Fatalf("insert %d = %v, want tail %v", index, args, want[index])
		}
	}

	// A replay, even one whose models differ, finds the turn settled and writes nothing.
	replay := domain.Usage{InputTokens: 30, OutputTokens: 12, ToolCalls: 2, CostMinorUnits: 9, Currency: "USD"}.AttributedTo(gpt)
	if err := store.RecordUsage(context.Background(), recordOwner, "conversation-1", 4, "agent@v1", attribution, replay); err != nil {
		t.Fatal(err)
	}
	if len(query.args[qLedgerInsertUsageEvent]) != len(want) {
		t.Fatalf("a replay wrote usage again: %v", query.args[qLedgerInsertUsageEvent])
	}
	query.args = nil
	usage.Models[0].InputTokens = 31
	if err := store.RecordUsage(context.Background(), recordOwner, "conversation-1", 4, "agent@v1", attribution, usage); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("overattributed usage = %v, want validation", err)
	}
	if len(query.args[qLedgerInsertUsageEvent]) != 0 {
		t.Fatal("an inconsistent breakdown wrote usage rows")
	}
}

func TestTurnLedgerReportsInsertedWhenAnyModelRowIsNew(t *testing.T) {
	query := &queueQueryFake{sequence: map[string][][][]any{qLedgerInsertUsageEvent: {nil, {{int64(9)}}}}}
	ledger := TurnLedger{UsageCategory: "agent_turn"}
	usage := domain.Usage{InputTokens: 2, ToolCalls: 1, Currency: "USD"}.AttributedTo(claude)
	inserted, err := ledger.InsertUsageEvent(context.Background(), query, 7, "c", 1, "agent@v1", domain.UsageAttribution{}, usage)
	if err != nil || !inserted || len(query.args[qLedgerInsertUsageEvent]) != 2 {
		t.Fatalf("inserted = %v, error = %v, args = %v", inserted, err, query.args)
	}
}

func TestTurnRuntimeSettlesTheModelsEveryStepUsed(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(_ context.Context, input domain.StepInput) (domain.StepResult, error) {
			if input.Step.StepID == "step-1" {
				usage := domain.Usage{InputTokens: 8, SearchQueries: 2, CostMinorUnits: 3, Currency: "USD"}.AttributedTo(claude)
				return domain.StepResult{State: []byte("plan"), NextStepID: "step-2", Usage: usage}, nil
			}
			usage := domain.Usage{OutputTokens: 5, CostMinorUnits: 2, Currency: "USD"}.AttributedTo(gpt)
			usage.ToolCalls = 1
			return domain.StepResult{State: []byte("answer"), Usage: usage}, nil
		}), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.usage) != 1 {
		t.Fatalf("usage events = %+v", recorder.usage)
	}
	settled := recorder.usage[0]
	want := []domain.ModelSpend{
		{Model: claude, InputTokens: 8, SearchQueries: 2, CostMinorUnits: 3},
		{Model: gpt, OutputTokens: 5, CostMinorUnits: 2},
	}
	if settled.SearchQueries != 2 || settled.ToolCalls != 1 || settled.CostMinorUnits != 5 || !reflect.DeepEqual(settled.Models, want) {
		t.Fatalf("settled = %+v", settled)
	}
	if _, err := usageLines(settled); err != nil {
		t.Fatalf("the settled usage must split into rows: %v", err)
	}
}

func TestToolLoopAttributesEachDecisionToItsModelAndPricesEachShare(t *testing.T) {
	attributed := proposes(10, toolCall("a", "search", `{"q":"x"}`))
	attributed.Usage = attributed.Usage.AttributedTo(claude)
	harness := newLoopHarness(t, attributed, answers("done"))
	executor := harness.executor()
	var priced []domain.ModelReference
	executor.Pricer = fake.ModelPricerFunc(func(_ context.Context, model domain.ModelReference, usage domain.ModelUsage) (int64, string, error) {
		priced = append(priced, model)
		return usage.InputTokens + usage.OutputTokens, "USD", nil
	})
	result, err := executor.Execute(context.Background(), loopInput())
	if err != nil {
		t.Fatal(err)
	}
	selected := domain.ModelReference{ProviderID: "p", ModelID: "m"}
	want := []domain.ModelSpend{
		{Model: claude, InputTokens: 10, OutputTokens: 10, CostMinorUnits: 20},
		{Model: selected, InputTokens: 5, OutputTokens: 5, CostMinorUnits: 10},
	}
	if !reflect.DeepEqual(result.Usage.Models, want) || result.Usage.CostMinorUnits != 30 || result.Usage.ToolCalls != 1 ||
		!reflect.DeepEqual(priced, []domain.ModelReference{claude, selected}) {
		t.Fatalf("usage = %+v, priced = %v", result.Usage, priced)
	}
}
