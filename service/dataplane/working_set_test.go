package dataplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type workingSetStub struct {
	set     domain.WorkingSet
	err     error
	request domain.WorkingSetRequest
}

func (stub *workingSetStub) Compile(_ context.Context, request domain.WorkingSetRequest) (domain.WorkingSet, error) {
	stub.request = request
	return stub.set, stub.err
}

type workingSetRecords struct {
	owners []domain.TurnOwner
	sets   []domain.WorkingSet
	err    error
}

func (records *workingSetRecords) RecordWorkingSet(_ context.Context, owner domain.TurnOwner, set domain.WorkingSet) error {
	records.owners, records.sets = append(records.owners, owner), append(records.sets, set)
	return records.err
}

func workingSetBuilder() *ReleaseLoopRequestBuilder {
	return &ReleaseLoopRequestBuilder{Definitions: &definitionReaderStub{definition: domain.AgentDefinition{
		AgentID: "writer", Version: "v3",
		Languages: []domain.CompiledPrompt{{LanguageCode: "en-US", Sections: []domain.CompiledPromptSection{{Instruction: "be brief"}}}},
	}}, Renderer: taskRenderer{}, MaxOutputTokens: 500}
}

func workingSetInput() domain.StepInput {
	input := loopInput()
	input.Input = []byte("renew the policy")
	input.Snapshot.ConversationID = "conversation-9"
	return input
}

// Without a compiler the request is exactly what it was: no working set, no assurance.
func TestReleaseLoopRequestBuilderWithoutAWorkingSetIsUnchanged(t *testing.T) {
	request, err := workingSetBuilder().Build(context.Background(), workingSetInput(), domain.ToolLoopConfig{})
	if err != nil || string(request.Prompt) != "writer|be brief|renew the policy|" || request.Assurance != "" {
		t.Fatalf("prompt %q, assurance %q, err %v", request.Prompt, request.Assurance, err)
	}
}

// A settled working set is recorded as the turn's owner, appended as fenced data, and carried on the request.
func TestReleaseLoopRequestBuilderAppendsAndRecordsTheWorkingSet(t *testing.T) {
	compiler := &workingSetStub{set: domain.WorkingSet{
		Assurance: domain.AssuranceBounded, Excluded: []string{"crm:old"},
		Selected: []domain.WorkingSetCandidate{{ID: "crm:limit", Provenance: "crm", Standing: domain.StandingCurrent, Content: "limit 5000"}},
	}}
	records := &workingSetRecords{}
	builder := workingSetBuilder()
	builder.WorkingSet, builder.WorkingSetRecords = compiler, records
	input := workingSetInput()
	request, err := builder.Build(context.Background(), input, domain.ToolLoopConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if request.Assurance != domain.AssuranceBounded || !strings.Contains(string(request.Prompt), `<item id="crm:limit"`) || !strings.Contains(string(request.Prompt), "limit 5000") {
		t.Fatalf("prompt %q, assurance %q", request.Prompt, request.Assurance)
	}
	if compiler.request.RequestID != "request-1" || compiler.request.ConversationID != "conversation-9" || compiler.request.Task != "renew the policy" ||
		compiler.request.Principal.ID != input.Principal.ID || compiler.request.TenantContext.TenantID != input.Principal.TenantID {
		t.Fatalf("compile request = %+v", compiler.request)
	}
	if len(records.sets) != 1 || records.owners[0] != input.Owner || !slices.Equal(records.sets[0].Excluded, []string{"crm:old"}) {
		t.Fatalf("records = %+v", records)
	}
}

// A conflicted or insufficient set is recorded, then stops the turn before the model.
func TestReleaseLoopRequestBuilderStopsOnAnUnsettledWorkingSet(t *testing.T) {
	for _, assurance := range []domain.WorkingSetAssurance{domain.AssuranceConflicted, domain.AssuranceInsufficient} {
		records := &workingSetRecords{}
		builder := workingSetBuilder()
		builder.WorkingSet, builder.WorkingSetRecords = &workingSetStub{set: domain.WorkingSet{Assurance: assurance, Excluded: []string{"a"}}}, records
		if _, err := builder.Build(context.Background(), workingSetInput(), domain.ToolLoopConfig{}); !errors.Is(err, domain.ErrWorkingSetUnassured) {
			t.Fatalf("%s: error = %v, want unassured", assurance, err)
		}
		if len(records.sets) != 1 || records.sets[0].Assurance != assurance {
			t.Fatalf("%s: an unsettled set is recorded before the turn stops: %+v", assurance, records.sets)
		}
	}
}

// A compiler without a recorder, an invalid assurance, and a failed record or compile all fail closed.
func TestReleaseLoopRequestBuilderWorkingSetFailsClosed(t *testing.T) {
	builder := workingSetBuilder()
	builder.WorkingSet = &workingSetStub{set: domain.WorkingSet{Assurance: domain.AssuranceVerified}}
	if _, err := builder.Build(context.Background(), workingSetInput(), domain.ToolLoopConfig{}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no recorder: %v", err)
	}
	builder.WorkingSetRecords = &workingSetRecords{err: domain.ErrFenceLost}
	if _, err := builder.Build(context.Background(), workingSetInput(), domain.ToolLoopConfig{}); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("record failure: %v", err)
	}
	builder.WorkingSet, builder.WorkingSetRecords = &workingSetStub{set: domain.WorkingSet{Assurance: "X"}}, &workingSetRecords{}
	if _, err := builder.Build(context.Background(), workingSetInput(), domain.ToolLoopConfig{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid assurance: %v", err)
	}
	builder.WorkingSet = &workingSetStub{err: domain.ErrExecutionLimit}
	if _, err := builder.Build(context.Background(), workingSetInput(), domain.ToolLoopConfig{}); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("compile failure: %v", err)
	}
}

// The recorder writes the assurance through the owner's guard, then every selected and excluded id.
func TestTableWorkingSetRecorderPersistsSelectedAndExcludedIDs(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qWorkingSetMark: {{int64(4), "conversation-1"}}}}
	recorder := &TableWorkingSetRecorder{DB: queueDBFake{query: query}}
	set := domain.WorkingSet{
		Assurance: domain.AssuranceBounded, Excluded: []string{"crm:old", "erp:big"},
		Selected: []domain.WorkingSetCandidate{{ID: "crm:limit"}},
	}
	if err := recorder.RecordWorkingSet(context.Background(), recordOwner, set); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(query.calls, []string{qWorkingSetMark, qWorkingSetClear, qWorkingSetItem, qWorkingSetItem, qWorkingSetItem}) {
		t.Fatalf("calls = %v", query.calls)
	}
	if mark := query.firstArgs(qWorkingSetMark); fmt.Sprint(mark) != fmt.Sprint([]any{"B", int64(7), "request-1", int64(3)}) {
		t.Fatalf("mark args = %v", mark)
	}
	if clear := query.firstArgs(qWorkingSetClear); fmt.Sprint(clear) != fmt.Sprint([]any{int64(7), "conversation-1", int64(4)}) {
		t.Fatalf("clear args = %v", clear)
	}
	want := [][]any{
		{int64(7), "conversation-1", int64(4), "crm:limit", true},
		{int64(7), "conversation-1", int64(4), "crm:old", false},
		{int64(7), "conversation-1", int64(4), "erp:big", false},
	}
	if fmt.Sprint(query.args[qWorkingSetItem]) != fmt.Sprint(want) {
		t.Fatalf("item args = %v", query.args[qWorkingSetItem])
	}
}

// A moved fence records nothing, and a malformed set is refused before the database.
func TestTableWorkingSetRecorderRefusesALostFenceAndAMalformedSet(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{}}
	recorder := &TableWorkingSetRecorder{DB: queueDBFake{query: query}}
	if err := recorder.RecordWorkingSet(context.Background(), recordOwner, domain.WorkingSet{Assurance: domain.AssuranceVerified}); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("error = %v, want fence lost", err)
	}
	if slices.Contains(query.calls, qWorkingSetItem) || slices.Contains(query.calls, qWorkingSetClear) {
		t.Fatalf("calls = %v", query.calls)
	}
	for name, set := range map[string]domain.WorkingSet{
		"assurance": {Assurance: "X"},
		"duplicate": {Assurance: domain.AssuranceVerified, Selected: []domain.WorkingSetCandidate{{ID: "a"}}, Excluded: []string{"a"}},
		"blank":     {Assurance: domain.AssuranceVerified, Excluded: []string{" "}},
		"long":      {Assurance: domain.AssuranceVerified, Excluded: []string{strings.Repeat("x", domain.MaxWorkingSetCandidateIDBytes+1)}},
	} {
		query.calls = nil
		if err := recorder.RecordWorkingSet(context.Background(), recordOwner, set); !errors.Is(err, domain.ErrValidation) || len(query.calls) != 0 {
			t.Fatalf("%s: error = %v, calls = %v", name, err, query.calls)
		}
	}
}

// An unsettled working set holds the turn under code E for a person, keeping its reservation.
func TestTurnRuntimeHoldsAnUnassuredTurnForAPerson(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
			return domain.StepResult{}, &LoopError{Err: fmt.Errorf("%w: the working set is C", domain.ErrWorkingSetUnassured)}
		}), nil
	})
	var codes []domain.TurnHoldCode
	runtime.Records.(*fake.TurnRecordStore).HoldFunc = func(_ context.Context, owner domain.TurnOwner, code domain.TurnHoldCode) (int64, error) {
		codes = append(codes, code)
		return owner.Fence + 1, nil
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("a hold ends the delivery cleanly: %v", err)
	}
	if !slices.Equal(codes, []domain.TurnHoldCode{domain.HoldEvidence}) || len(recorder.failures) != 0 || recorder.committed != 0 {
		t.Fatalf("hold codes %v, failures %v, committed %d", codes, recorder.failures, recorder.committed)
	}
}

// The table store holds a turn on evidence, which the worker may take, and still refuses other codes.
func TestTableTurnRecordStoreHoldsOnEvidence(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qRecordHold: {{int64(4)}}}}
	store := newTestRecordStore(query)
	if _, err := store.Hold(context.Background(), recordOwner, domain.HoldEvidence); err != nil {
		t.Fatal(err)
	}
	if args := query.firstArgs(qRecordHold); len(args) == 0 || args[0] != string(domain.HoldEvidence) {
		t.Fatalf("hold args = %v", args)
	}
	if _, err := store.Hold(context.Background(), recordOwner, domain.HoldPreempted); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}

type assuredRequestBuilder struct{ assurance domain.WorkingSetAssurance }

func (builder assuredRequestBuilder) Build(ctx context.Context, input domain.StepInput, config domain.ToolLoopConfig) (domain.ModelRequest, error) {
	request, err := loopRequestBuilder{}.Build(ctx, input, config)
	request.Assurance = builder.assurance
	return request, err
}

// The loop gives the request's assurance to every call, so the gateway sees the turn's working set.
func TestToolLoopCarriesTheWorkingSetAssuranceToEveryCall(t *testing.T) {
	harness := newLoopHarness(t,
		proposes(10, toolCall("a", "search", `{"q":"x"}`), toolCall("b", "shop_write", `{"sku":"1"}`)),
		answers("done"))
	executor := harness.executor()
	executor.Requests = assuredRequestBuilder{assurance: domain.AssuranceBounded}
	if _, err := executor.Execute(context.Background(), loopInput()); err != nil {
		t.Fatal(err)
	}
	if len(harness.calls) != 2 || harness.calls[0].Assurance != domain.AssuranceBounded || harness.calls[1].Assurance != domain.AssuranceBounded {
		t.Fatalf("calls = %+v", harness.calls)
	}
}

var _ contract.WorkingSetCompiler = (*workingSetStub)(nil)
