package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var traceBase = time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)

func traceAt(seconds int) time.Time { return traceBase.Add(time.Duration(seconds) * time.Second) }

// traceTable answers the trace queries from rows keyed by tenant and request id, and
// fails the test when a source query names anything else.
type traceTable struct {
	t       *testing.T
	runs    map[string][]any
	turns   map[string][]any
	sources map[string]map[string][][]any
	calls   []string
}

func (table *traceTable) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	table.calls = append(table.calls, name)
	switch name {
	case qRunTraceRun:
		if row, ok := table.runs[strings.TrimSpace(fmt.Sprintln(args...))]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{row}}, nil
		}
		return &keelmodel.QueryResult{}, nil
	case qRunTraceTurn:
		if row, ok := table.turns[strings.TrimSpace(fmt.Sprintln(args...))]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{row}}, nil
		}
		return &keelmodel.QueryResult{}, nil
	}
	if len(args) != 3 {
		table.t.Fatalf("%s takes tenant, request id, and a limit, got %v", name, args)
	}
	rows := table.sources[name][fmt.Sprintf("%v %v", args[0], args[1])]
	return &keelmodel.QueryResult{Rows: rows[:min(len(rows), args[2].(int))]}, nil
}

func (*traceTable) GenID() int64 { return 0 }

type traceDB struct {
	keelport.DatabaseRepository
	table *traceTable
}

func (db traceDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.table
}

type charterActionsFunc func(context.Context, int64, string, int) ([]domain.CharterAction, error)

func (function charterActionsFunc) RunActions(ctx context.Context, tenantID int64, requestID string, limit int) ([]domain.CharterAction, error) {
	return function(ctx, tenantID, requestID, limit)
}

type traceFixture struct {
	table   *traceTable
	storage *fake.ObjectStorage
	objects *ObjectStateStore
	sizes   []int
	store   *TableRunTrace
}

const traceRequest = "turn-req-1"

// newTraceFixture holds run 31 of writer-a in tenant 7: a loop that calls a tool behind an
// approval, sees a guardrail and a Charter action, fails a second call, and answers.
func newTraceFixture(t *testing.T) *traceFixture {
	t.Helper()
	storage := &fake.ObjectStorage{}
	fixture := &traceFixture{
		table: &traceTable{t: t, runs: map[string][]any{
			"7 31 writer-a": {int64(31), "writer-a", "4", "chat", traceRequest, "C", traceAt(9)},
			"7 32 writer-a": {int64(32), "writer-a", "4", "batch", nil, "C", traceAt(9)},
		}, turns: map[string][]any{
			"7 " + traceRequest: {"conv-1", int64(3), "C", traceAt(0), traceAt(1), traceAt(5), traceAt(9)},
		}, sources: map[string]map[string][][]any{}},
		storage: storage, objects: newObjectStateStore(storage),
	}
	model := domain.Usage{InputTokens: 10, OutputTokens: 5, CostMinorUnits: 3, Currency: "USD",
		Models: []domain.ModelSpend{{Model: domain.ModelReference{ProviderID: "anthropic", ModelID: "claude"}, InputTokens: 10, OutputTokens: 5, CostMinorUnits: 3}}}
	lookup := domain.ToolReference{ToolID: "lookup", Version: "v1"}
	fixture.journal(t, 1, domain.LoopEntry{Kind: domain.LoopEntryModel, Iteration: 1, Usage: model, Model: &domain.ModelResult{
		Output: []byte("SECRET-THOUGHT"), ToolCalls: []domain.ModelToolCall{{CallID: "c1", Name: "lookup", Arguments: []byte(`{"q":"SECRET-ARGS"}`)}}}})
	fixture.journal(t, 2, domain.LoopEntry{Kind: domain.LoopEntryApprovalPending, Iteration: 1, Tool: lookup,
		Observation: &domain.ModelToolObservation{CallID: "c1", Name: "lookup"}})
	fixture.journal(t, 3, domain.LoopEntry{Kind: domain.LoopEntryObservation, Iteration: 1, Tool: lookup, Usage: domain.Usage{ToolCalls: 1},
		Observation: &domain.ModelToolObservation{CallID: "c1", Name: "lookup", Output: []byte(`{"rows":"SECRET-RESULT"}`)},
		Effect:      &domain.EffectObservation{Status: domain.EffectSatisfied}})
	fixture.journal(t, 4, domain.LoopEntry{Kind: domain.LoopEntryObservation, Iteration: 2, Tool: lookup, Usage: domain.Usage{ToolCalls: 1},
		Observation: &domain.ModelToolObservation{CallID: "c2", Name: "lookup", IsError: true, Output: []byte(`{"error":"rate_limited"}`)}})
	fixture.journal(t, 5, domain.LoopEntry{Kind: domain.LoopEntryModel, Iteration: 3, Usage: model, Model: &domain.ModelResult{Output: []byte("SECRET-ANSWER")}})
	fixture.source(qRunTraceSafety, []any{int64(5), "tool_output", "T", "redact", "soft", `["pii.email"]`, "4", "g7", int64(12), traceAt(3)})
	fixture.source(qRunTraceApprovals, []any{int64(9), int64(11), "invoke", "lookup", "policy", "rule-1", "high", "A",
		strings.Repeat("d", 64), nil, traceAt(2), traceAt(2).Add(time.Second), "human", traceAt(2).Add(time.Second)})
	fixture.source(qRunTraceDecisions, []any{int64(21), "tool_invoke", "invoke", "lookup", "A", "policy-1", "3", "4",
		"agent", "writer-a", nil, nil, nil, "record_evidence,notify", nil, traceAt(1)})
	fixture.store = &TableRunTrace{DB: traceDB{table: fixture.table}, Objects: fixture.objects,
		Charter: charterActionsFunc(func(_ context.Context, tenantID int64, requestID string, limit int) ([]domain.CharterAction, error) {
			if tenantID != 7 || requestID != traceRequest || limit <= 0 {
				t.Fatalf("charter actions of %d/%q limit %d", tenantID, requestID, limit)
			}
			return []domain.CharterAction{{ActionID: domain.CharterRef{Namespace: "acme", ID: "ACT-1"}, Capability: domain.CharterRef{ID: "CAP-LOOKUP"},
				OperationClass: "read", Disposition: "executed", Outcome: "found", At: traceAt(3),
				Authority: []domain.CharterEvaluation{{Ref: &domain.CharterRef{ID: "GRANT-1"}, Result: "allowed"}}}}, nil
		})}
	return fixture
}

func (fixture *traceFixture) journal(t *testing.T, entryNo int, entry domain.LoopEntry) {
	t.Helper()
	entry.EntryNo = entryNo
	payload, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.LoopKey{TenantID: 7, RequestID: traceRequest, ExecutionStepID: 11}
	ref, err := fixture.objects.Dehydrate(context.Background(), loopEntryName(key, entryNo), payload)
	if err != nil {
		t.Fatal(err)
	}
	fixture.sizes = append(fixture.sizes, len(payload))
	fixture.source(qRunTraceJournal, []any{int64(11), int64(entryNo), ref.URI, ref.Digest, traceAt(entryNo)})
}

func (fixture *traceFixture) source(name string, row []any) {
	if fixture.table.sources[name] == nil {
		fixture.table.sources[name] = map[string][][]any{}
	}
	key := "7 " + traceRequest
	fixture.table.sources[name][key] = append(fixture.table.sources[name][key], row)
}

func traceKinds(events []domain.RunTraceEvent) string {
	kinds := make([]string, len(events))
	for index, event := range events {
		kinds[index] = string(event.Kind)
		if event.Decision != nil {
			kinds[index] += ":" + string(event.Decision.Source)
		}
	}
	return strings.Join(kinds, ",")
}

func TestRunTraceOrdersEverySourceByTimeWithAStableTieBreak(t *testing.T) {
	fixture := newTraceFixture(t)
	trace, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31)
	if err != nil {
		t.Fatal(err)
	}
	want := "model_step,decision:audit,approval,tool_call,guardrail,decision:charter,tool_call,model_step"
	if got := traceKinds(trace.Events); got != want || trace.Truncated {
		t.Fatalf("events = %s (truncated %v), want %s", got, trace.Truncated, want)
	}
	for index, event := range trace.Events {
		if event.Sequence != index+1 {
			t.Fatalf("event %d has sequence %d", index, event.Sequence)
		}
	}
	if trace.Run.ID != 31 || trace.Run.RequestID != traceRequest || trace.Turn == nil || trace.Turn.TurnNo != 3 ||
		trace.Turn.Status != domain.TurnCompleted || !trace.Turn.FirstResponseAt.Equal(traceAt(5)) {
		t.Fatalf("run %+v turn %+v", trace.Run, trace.Turn)
	}
	first, call, failed, answer := trace.Events[0].Model, trace.Events[3].Tool, trace.Events[6].Tool, trace.Events[7].Model
	if first.Finish != domain.RunTraceFinishToolCalls || first.ToolCalls != 1 || first.Usage.InputTokens != 10 ||
		len(first.Models) != 1 || first.Models[0].ModelID != "claude" || first.Journal.EntryNo != 1 || len(first.Journal.Digest) != 64 {
		t.Fatalf("first model step = %+v", first)
	}
	if call.Outcome != domain.RunTraceToolOK || call.Tool.ToolID != "lookup" || call.Effect != domain.EffectSatisfied || call.Journal.EntryNo != 3 {
		t.Fatalf("tool call = %+v", call)
	}
	if failed.Outcome != domain.RunTraceToolError || failed.ErrorClass != "rate_limited" || failed.Journal.Iteration != 2 {
		t.Fatalf("failed tool call = %+v", failed)
	}
	if answer.Finish != domain.RunTraceFinishAnswer || answer.ToolCalls != 0 {
		t.Fatalf("answer = %+v", answer)
	}
	approval, guardrail := trace.Events[2].Approval, trace.Events[4].Guardrail
	if approval.Status != domain.ApprovalStatusApproved || approval.DeciderKind != domain.PrincipalHuman || approval.RuleSource != "policy" || len(approval.ProposedDigest) != 64 {
		t.Fatalf("approval = %+v", approval)
	}
	if guardrail.Stage != domain.GuardrailStageToolOutput || len(guardrail.RuleIDs) != 1 || guardrail.Duration != 12*time.Millisecond {
		t.Fatalf("guardrail = %+v", guardrail)
	}
	audit, charter := trace.Events[1].Decision, trace.Events[5].Decision
	if audit.Outcome != domain.DecisionAllow || audit.PolicyVersion != "3" || len(audit.Obligations) != 2 || audit.Authority.Subject.ID != "writer-a" {
		t.Fatalf("audit decision = %+v", audit)
	}
	if charter.Charter == nil || charter.Charter.Disposition != "executed" || charter.Action != "CAP-LOOKUP" || charter.Charter.Authority[0].Result != "allowed" {
		t.Fatalf("charter decision = %+v", charter)
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET") {
		t.Fatalf("a trace must not carry model output, tool arguments, or tool results: %s", encoded)
	}
}

func TestRunTraceOfAnotherTenantOrAgentIsNotFoundAndReadsNoSource(t *testing.T) {
	for _, probe := range []struct {
		tenant int64
		agent  string
	}{{8, "writer-a"}, {7, "writer-b"}} {
		fixture := newTraceFixture(t)
		if _, err := fixture.store.Trace(context.Background(), probe.tenant, probe.agent, 31); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("run 31 as %d/%s = %v, want not found", probe.tenant, probe.agent, err)
		}
		if len(fixture.table.calls) != 1 || fixture.table.calls[0] != qRunTraceRun {
			t.Fatalf("a run outside the scope must read nothing else: %v", fixture.table.calls)
		}
	}
	fixture := newTraceFixture(t)
	for _, refused := range []struct {
		tenant int64
		agent  string
		run    int64
	}{{0, "writer-a", 31}, {7, " ", 31}, {7, "writer-a", 0}} {
		if _, err := fixture.store.Trace(context.Background(), refused.tenant, refused.agent, refused.run); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%+v = %v, want validation", refused, err)
		}
	}
}

func TestRunTraceOfARunOutsideATurnHasNoEvents(t *testing.T) {
	fixture := newTraceFixture(t)
	trace, err := fixture.store.Trace(context.Background(), 7, "writer-a", 32)
	if err != nil || trace.Run.ID != 32 || trace.Turn != nil || trace.Events == nil || len(trace.Events) != 0 || trace.Truncated {
		t.Fatalf("trace = %+v, %v", trace, err)
	}
	if len(fixture.table.calls) != 1 {
		t.Fatalf("a run without a request id reads no source: %v", fixture.table.calls)
	}
}

func TestRunTraceFailsOnATamperedJournalObject(t *testing.T) {
	fixture := newTraceFixture(t)
	for key := range fixture.storage.Objects {
		if strings.Contains(key, "/11/3/") {
			fixture.storage.Objects[key] = []byte(`{"entry_no":3,"kind":"observation"}`)
		}
	}
	_, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("a tampered entry = %v, want a digest mismatch", err)
	}

	unreadable := newTraceFixture(t)
	unreadable.table.sources[qRunTraceJournal]["7 "+traceRequest][0][2] = "scout://elsewhere/scout/loop/7/x"
	_, err = unreadable.store.Trace(context.Background(), 7, "writer-a", 31)
	if err == nil || errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unreadable stored entry must be a server failure, got %v", err)
	}

	charterDown := newTraceFixture(t)
	charterDown.store.Charter = charterActionsFunc(func(context.Context, int64, string, int) ([]domain.CharterAction, error) {
		return nil, fmt.Errorf("%w: no namespace", domain.ErrNotFound)
	})
	if _, err := charterDown.store.Trace(context.Background(), 7, "writer-a", 31); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a failed Charter read must fail the trace as a server error, got %v", err)
	}

	cancelled := newTraceFixture(t)
	cancelled.store.Charter = charterActionsFunc(func(context.Context, int64, string, int) ([]domain.CharterAction, error) {
		return nil, context.Canceled
	})
	if _, err := cancelled.store.Trace(context.Background(), 7, "writer-a", 31); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled Charter read = %v, want cancellation", err)
	}

	unavailable := newTraceFixture(t)
	unavailable.store.Charter = charterActionsFunc(func(context.Context, int64, string, int) ([]domain.CharterAction, error) {
		return nil, fmt.Errorf("provider detail: %w", domain.ErrNotReady)
	})
	if _, err := unavailable.store.Trace(context.Background(), 7, "writer-a", 31); !errors.Is(err, domain.ErrNotReady) || strings.Contains(err.Error(), "provider detail") {
		t.Fatalf("an unavailable Charter source = %v, want sanitized not ready", err)
	}
}

func TestRunTraceFailsOnAMalformedJournalEntry(t *testing.T) {
	fixture := newTraceFixture(t)
	fixture.table.sources[qRunTraceJournal]["7 "+traceRequest] = nil
	fixture.journal(t, 1, domain.LoopEntry{Kind: domain.LoopEntryModel})
	if _, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31); err == nil || !strings.Contains(err.Error(), "malformed loop journal entry") {
		t.Fatalf("malformed journal entry = %v", err)
	}
}

func TestRunTraceStopsAtItsBoundsAndCutsEverySourceAtTheHorizon(t *testing.T) {
	fixture := newTraceFixture(t)
	fixture.store.Limits = domain.RunTraceLimits{MaxEvents: 3}
	trace, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31)
	if err != nil || !trace.Truncated || traceKinds(trace.Events) != "model_step,decision:audit,approval" {
		t.Fatalf("event cap: %s truncated %v, %v", traceKinds(trace.Events), trace.Truncated, err)
	}

	fixture = newTraceFixture(t)
	fixture.store.Limits = domain.RunTraceLimits{MaxJournalEntries: 2}
	trace, err = fixture.store.Trace(context.Background(), 7, "writer-a", 31)
	if err != nil || !trace.Truncated || traceKinds(trace.Events) != "model_step,decision:audit,approval" {
		t.Fatalf("entry cap: %s truncated %v, %v", traceKinds(trace.Events), trace.Truncated, err)
	}

	fixture = newTraceFixture(t)
	fixture.store.Limits = domain.RunTraceLimits{MaxJournalBytes: int64(fixture.sizes[0])}
	trace, err = fixture.store.Trace(context.Background(), 7, "writer-a", 31)
	if err != nil || !trace.Truncated || traceKinds(trace.Events) != "model_step,decision:audit" {
		t.Fatalf("byte cap: %s truncated %v, %v", traceKinds(trace.Events), trace.Truncated, err)
	}
	reads := 0
	for _, call := range fixture.table.calls {
		if call == qRunTraceJournal {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("the journal is listed once: %v", fixture.table.calls)
	}

	for name, limits := range map[string]domain.RunTraceLimits{
		"negative":         {MaxEvents: -1},
		"events too high":  {MaxEvents: MaxRunTraceEvents + 1},
		"entries too high": {MaxJournalEntries: MaxRunTraceJournalEntries + 1},
		"bytes too high":   {MaxJournalBytes: MaxRunTraceJournalBytes + 1},
	} {
		fixture.store = &TableRunTrace{DB: traceDB{table: fixture.table}, Objects: fixture.objects, Limits: limits}
		if _, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestObservedErrorClassNeverEchoesUnknownText(t *testing.T) {
	for output, want := range map[string]string{
		`{"error":"rate_limited"}`: "rate_limited", `{"error":"internal"}`: "internal",
		`{"error":"SECRET detail"}`: "", `not json`: "", `{"rows":1}`: "",
	} {
		if got := observedErrorClass([]byte(output)); got != want {
			t.Fatalf("observedErrorClass(%s) = %q, want %q", output, got, want)
		}
	}
}

func TestJournalEventRejectsInvalidStoredValues(t *testing.T) {
	for name, entry := range map[string]domain.LoopEntry{
		"negative usage":        {Kind: domain.LoopEntryModel, Model: &domain.ModelResult{}, Usage: domain.Usage{InputTokens: -1}},
		"cost without currency": {Kind: domain.LoopEntryModel, Model: &domain.ModelResult{}, Usage: domain.Usage{CostMinorUnits: 1}},
		"blank tool":            {Kind: domain.LoopEntryObservation, Observation: &domain.ModelToolObservation{}},
		"invalid effect": {Kind: domain.LoopEntryObservation, Tool: domain.ToolReference{ToolID: "lookup"},
			Observation: &domain.ModelToolObservation{}, Effect: &domain.EffectObservation{Status: "maybe"}},
		"unknown kind": {Kind: "future"},
	} {
		if _, err := journalEvent(entry, domain.RunTraceJournalRef{}); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestRunTraceRefusesAnOverflowingGuardrailDuration(t *testing.T) {
	fixture := newTraceFixture(t)
	fixture.table.sources[qRunTraceSafety]["7 "+traceRequest][0][8] = int64(math.MaxInt64)
	if _, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31); err == nil || !strings.Contains(err.Error(), "malformed safety event") {
		t.Fatalf("overflowing guardrail duration = %v", err)
	}
}

// Charter returns a run's latest actions, so a window it filled may omit the run's first ones; a
// trace refuses rather than show a beginning without its Charter decisions.
func TestRunTraceRefusesACharterWindowThatMayOmitTheRunsStart(t *testing.T) {
	fixture := newTraceFixture(t)
	fixture.store.Limits = domain.RunTraceLimits{MaxEvents: 2}
	fixture.store.Charter = charterActionsFunc(func(_ context.Context, _ int64, _ string, limit int) ([]domain.CharterAction, error) {
		actions := make([]domain.CharterAction, limit)
		for index := range actions {
			actions[index] = domain.CharterAction{ActionID: domain.CharterRef{ID: fmt.Sprintf("ACT-%d", index)}, At: traceAt(3)}
		}
		return actions, nil
	})
	if _, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("a full Charter window = %v, want ErrExecutionLimit", err)
	}
}

// A journaled call carries its outcome record matched by step and call id; a call without one, such as a read, carries none.
func TestRunTraceAttachesEachCallsSettlementByStepAndCallID(t *testing.T) {
	fixture := newTraceFixture(t)
	evidence := strings.Repeat("e", 64)
	fixture.source(qRunTraceOutcomes, []any{int64(11), "c2", "C", "T", "human", "9", traceAt(8), evidence})
	fixture.source(qRunTraceOutcomes, []any{int64(12), "c1", "U", "", "", "", nil, ""})
	trace, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31)
	if err != nil {
		t.Fatal(err)
	}
	read, settled := trace.Events[3].Tool, trace.Events[6].Tool
	if read.CallID != "c1" || read.Settlement != nil {
		t.Fatalf("a call without an outcome record of its step = %+v", read)
	}
	want := domain.RunTraceCallSettlement{Status: domain.CallCompleted, Resolution: domain.ResolvedByAttestation,
		Resolver: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}, ResolvedAt: traceAt(8), EvidenceDigest: evidence}
	if settled.CallID != "c2" || settled.Settlement == nil || *settled.Settlement != want {
		t.Fatalf("settled call = %+v", settled)
	}

	fixture.table.sources[qRunTraceOutcomes]["7 "+traceRequest][0][2] = "X"
	if _, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31); err == nil || !strings.Contains(err.Error(), "malformed call outcome") {
		t.Fatalf("an unknown outcome status = %v", err)
	}
	many := make([][]any, maxTurnOutcomes+1)
	for index := range many {
		many[index] = []any{int64(11), fmt.Sprintf("c%d", index), "C", "G", "", "", traceAt(8), ""}
	}
	fixture.table.sources[qRunTraceOutcomes]["7 "+traceRequest] = many
	if _, err := fixture.store.Trace(context.Background(), 7, "writer-a", 31); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("an unbounded outcome read = %v, want ErrExecutionLimit", err)
	}
}
