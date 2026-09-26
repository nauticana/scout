package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var loopPrincipal = domain.Principal{
	Kind: domain.PrincipalAgent, ID: "writer", TenantID: 7, Release: "v3", ScopeID: "team-a",
	Authority: domain.AuthorityChain{{GrantID: "grant-1"}},
}

type loopRequestBuilder struct{}

func (loopRequestBuilder) Build(_ context.Context, input domain.StepInput, _ domain.ToolLoopConfig) (domain.ModelRequest, error) {
	return domain.ModelRequest{
		TenantContext: domain.TenantContext{TenantID: input.Principal.TenantID}, RequestID: input.RequestID,
		ConversationID: "conversation", Prompt: []byte("do the work"), MaxOutputTokens: 100,
	}, nil
}

// loopHarness scripts the model and counts real tool side effects.
type loopHarness struct {
	t           *testing.T
	script      []domain.ModelResult
	modelCalls  int
	effects     map[string]int
	calls       []domain.ToolCall
	invoke      func(domain.ToolCall) (domain.ToolResult, error)
	journal     contract.LoopJournal
	limits      domain.ToolLoopLimits
	lastRequest domain.ModelRequest
}

func newLoopHarness(t *testing.T, script ...domain.ModelResult) *loopHarness {
	return &loopHarness{
		t: t, script: script, effects: map[string]int{}, journal: &MemoryLoopJournal{},
		limits: domain.ToolLoopLimits{MaxIterations: 6, MaxToolCalls: 8, MaxTokens: 10_000, MaxRepeatedCalls: 2, Deadline: time.Minute},
	}
}

func (harness *loopHarness) executor() *ToolLoopExecutor {
	executor, err := NewToolLoopExecutor(ToolLoopExecutor{
		Requests: loopRequestBuilder{},
		Router: fake.ModelRouterFunc(func(context.Context, domain.ModelRequest) (domain.ModelSelection, error) {
			return domain.ModelSelection{Provider: "p", Model: "m"}, nil
		}),
		Models: &fake.ModelGateway{GenerateFunc: func(_ context.Context, _ domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
			harness.lastRequest = request
			if harness.modelCalls >= len(harness.script) {
				harness.t.Fatalf("model asked %d times, script has %d decisions", harness.modelCalls+1, len(harness.script))
			}
			harness.modelCalls++
			return harness.script[harness.modelCalls-1], nil
		}},
		Registry: &fake.ToolRegistry{ListFunc: func(_ context.Context, tenantID int64, agentID, release string) ([]domain.ToolDefinition, error) {
			if tenantID != 7 || agentID != "writer" || release != "v3" {
				harness.t.Fatalf("tools listed for %d %s@%s", tenantID, agentID, release)
			}
			return []domain.ToolDefinition{
				{ToolID: "search", Version: "1", InputSchema: []byte(`{"type":"object"}`)},
				{ToolID: "shop.write", Version: "2", InputSchema: []byte(`{"type":"object"}`)},
			}, nil
		}},
		Tools: fake.GovernedToolGatewayFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			harness.calls = append(harness.calls, call)
			if harness.invoke != nil {
				if result, err := harness.invoke(call); err != nil || result.Output != nil {
					return result, err
				}
			}
			harness.effects[call.IdempotencyKey]++
			return domain.ToolResult{Output: []byte(`{"ok":true}`), Usage: domain.Usage{ToolCalls: 1}}, nil
		}),
		Journal: harness.journal, Limits: harness.limits,
		Pricer: fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
			return 0, "USD", nil
		}),
	})
	if err != nil {
		harness.t.Fatalf("NewToolLoopExecutor: %v", err)
	}
	return executor
}

func loopInput() domain.StepInput {
	return domain.StepInput{
		Step:      domain.ExecutionStep{ExecutionStepID: 11, StepID: "work", Kind: domain.StepKindToolLoop},
		Principal: loopPrincipal, RequestID: "request-1",
		OnBehalfOf: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"},
	}
}

func proposes(usage int64, calls ...domain.ModelToolCall) domain.ModelResult {
	return domain.ModelResult{ToolCalls: calls, FinishReason: domain.FinishReasonToolCalls, Usage: domain.Usage{InputTokens: usage, OutputTokens: usage}}
}

func answers(text string) domain.ModelResult {
	return domain.ModelResult{Output: []byte(text), FinishReason: "stop", Usage: domain.Usage{InputTokens: 5, OutputTokens: 5}}
}

func toolCall(id, name, arguments string) domain.ModelToolCall {
	return domain.ModelToolCall{CallID: id, Name: name, Arguments: []byte(arguments)}
}

func TestToolLoopRunsParallelCallsThroughTheGovernedGatewayToATerminalAnswer(t *testing.T) {
	harness := newLoopHarness(t,
		proposes(10, toolCall("a", "search", `{"q":"x"}`), toolCall("b", "shop_write", `{"sku":"1"}`)),
		answers("done"))
	result, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(result.State) != "done" || result.Usage.InputTokens != 15 || result.Usage.ToolCalls != 2 {
		t.Fatalf("result = %s, usage %+v", result.State, result.Usage)
	}
	if len(harness.calls) != 2 || harness.calls[1].ToolID != "shop.write" || harness.calls[1].ToolVersion != "2" {
		t.Fatalf("tool calls = %+v", harness.calls)
	}
	for _, call := range harness.calls {
		if !reflect.DeepEqual(call.Principal, loopPrincipal) || call.TenantContext.TenantID != 7 || call.RequestID != "request-1" || call.IdempotencyKey == "" || call.OnBehalfOf.ID != "42" {
			t.Fatalf("identity did not survive the iteration: %+v", call)
		}
	}
	followUp := harness.lastRequest.Messages
	if len(followUp) != 2 || len(followUp[0].ToolCalls) != 2 || len(followUp[1].Observations) != 2 || followUp[1].Observations[1].CallID != "b" {
		t.Fatalf("second model turn saw %+v", followUp)
	}
	var kinds []domain.TurnEventKind
	for _, event := range result.Events {
		if event.Version != domain.TurnEventVersion {
			t.Fatalf("unversioned event %+v", event)
		}
		kinds = append(kinds, event.Kind)
	}
	want := []domain.TurnEventKind{domain.TurnEventToolProposal, domain.TurnEventToolResult, domain.TurnEventToolProposal, domain.TurnEventToolResult, domain.TurnEventResult}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	if result.Events[len(result.Events)-1].Text != "done" {
		t.Fatalf("terminal event omitted its result: %+v", result.Events[len(result.Events)-1])
	}
}

func TestToolLoopMergesTheCitationsOfEveryGroundedCall(t *testing.T) {
	first := proposes(3, toolCall("c1", "search", `{"q":"a"}`))
	first.Citations = []domain.Citation{{URL: "https://a.example", Title: "A", Position: 1}, {URL: "https://b.example", Position: 2}}
	final := answers("done")
	final.Citations = []domain.Citation{{URL: "https://b.example", Position: 1}, {URL: "https://c.example", Position: 2}}
	harness := newLoopHarness(t, first, final)
	result, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Citation{{URL: "https://a.example", Title: "A", Position: 1}, {URL: "https://b.example", Position: 2}, {URL: "https://c.example", Position: 3}}
	if !reflect.DeepEqual(result.Citations, want) {
		t.Fatalf("citations = %+v", result.Citations)
	}
	last := result.Events[len(result.Events)-1]
	if last.Kind != domain.TurnEventCitations || !reflect.DeepEqual(last.Citations, want) || result.Events[len(result.Events)-2].Kind != domain.TurnEventResult {
		t.Fatalf("events = %+v", result.Events)
	}
	// A redelivery replays the journal and reports the same sources.
	replayed, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil || !reflect.DeepEqual(replayed.Citations, want) || harness.modelCalls != 2 {
		t.Fatalf("replay = %+v, %v, model calls %d", replayed.Citations, err, harness.modelCalls)
	}
	uncited := newLoopHarness(t, answers("plain"))
	if result, err := uncited.executor().Execute(context.Background(), loopInput()); err != nil || len(result.Citations) != 0 || result.Events[len(result.Events)-1].Kind != domain.TurnEventResult {
		t.Fatalf("an ungrounded answer emits no citations event: %+v, %v", result.Events, err)
	}
	negative := loopInput()
	negative.Step.Configuration = []byte(`{"search":{"max_searches":-1}}`)
	if _, err := newLoopHarness(t, answers("x")).executor().Execute(context.Background(), negative); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative search ceiling = %v", err)
	}
}

func TestToolLoopRejectsDifferentContentThatLostAJournalRace(t *testing.T) {
	journal := &MemoryLoopJournal{}
	key := domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11}
	winner := domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryModel, Iteration: 1, Model: &domain.ModelResult{Output: []byte("winner")}}
	if _, err := journal.Append(context.Background(), key, winner); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	harness := newLoopHarness(t, answers("loser"))
	harness.journal = journal
	// Loading replays the winner. Exercise append contention directly because a
	// valid loaded journal never asks the model for this already-written slot.
	run := &loopRun{key: key, journal: nil}
	_, err := harness.executor().append(context.Background(), run, domain.LoopEntry{
		Kind: domain.LoopEntryModel, Iteration: 1, Model: &domain.ModelResult{Output: []byte("loser")},
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for different first-writer content, got %v", err)
	}
}

func TestToolLoopIdempotencyKeyDoesNotCollideWhenLongCallIDsShareAPrefix(t *testing.T) {
	key := domain.LoopKey{TenantID: 7, RequestID: strings.Repeat("r", 120), ExecutionStepID: 11}
	left := callIdempotencyKey(key, toolCall(strings.Repeat("x", 300)+"a", "search", `{}`))
	right := callIdempotencyKey(key, toolCall(strings.Repeat("x", 300)+"b", "search", `{}`))
	if left == right || len(left) > 200 || !strings.HasPrefix(left, "loop:") {
		t.Fatalf("unsafe idempotency keys %q and %q", left, right)
	}
}

func TestToolLoopSuspendsOnPendingApprovalAndResumesWithoutRepeatingAnything(t *testing.T) {
	harness := newLoopHarness(t,
		proposes(10, toolCall("a", "search", `{"q":"x"}`), toolCall("b", "shop_write", `{"sku":"1"}`)),
		answers("published"))
	approved := false
	var approvedArguments []byte
	harness.invoke = func(call domain.ToolCall) (domain.ToolResult, error) {
		if call.ToolID == "shop.write" && !approved {
			approvedArguments = call.Arguments
			return domain.ToolResult{}, fmt.Errorf("guardrail: %w", domain.ErrApprovalPending)
		}
		return domain.ToolResult{}, nil
	}
	_, err := harness.executor().Execute(context.Background(), loopInput())
	if !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatalf("want ErrApprovalPending so the turn suspends, got %v", err)
	}
	if len(harness.effects) != 1 || harness.modelCalls != 1 {
		t.Fatalf("before the verdict: effects %v, model calls %d", harness.effects, harness.modelCalls)
	}

	approved = true
	result, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if harness.modelCalls != 2 {
		t.Fatalf("the journaled decision must not be re-asked; model calls = %d", harness.modelCalls)
	}
	resumed := harness.calls[len(harness.calls)-1]
	if resumed.ToolID != "shop.write" || string(resumed.Arguments) != string(approvedArguments) {
		t.Fatalf("resume must present the exact approved call, got %+v", resumed)
	}
	for key, count := range harness.effects {
		if count != 1 {
			t.Fatalf("effect %s ran %d times", key, count)
		}
	}
	resolved := false
	for _, event := range result.Events {
		resolved = resolved || event.Kind == domain.TurnEventApprovalResolved && event.Approval.Decision == string(domain.ApprovalApproved)
	}
	if !resolved || result.Usage.ToolCalls != 2 {
		t.Fatalf("resumed result lacks the resolved approval or full usage: %+v", result)
	}
}

// crashingJournal fails the append of one entry number once, after storing it or before.
type crashingJournal struct {
	*MemoryLoopJournal
	crashAt     int
	afterCommit bool
	crashed     bool
}

func (journal *crashingJournal) Append(ctx context.Context, key domain.LoopKey, entry domain.LoopEntry) (domain.LoopEntry, error) {
	if entry.EntryNo == journal.crashAt && !journal.crashed {
		journal.crashed = true
		if journal.afterCommit {
			if _, err := journal.MemoryLoopJournal.Append(ctx, key, entry); err != nil {
				return domain.LoopEntry{}, err
			}
		}
		return domain.LoopEntry{}, errors.New("worker crashed")
	}
	return journal.MemoryLoopJournal.Append(ctx, key, entry)
}

func TestToolLoopRedeliveryAfterEachDurableBoundaryNeverRepeatsACommittedEffect(t *testing.T) {
	for entryNo := 1; entryNo <= 4; entryNo++ {
		harness := newLoopHarness(t,
			proposes(10, toolCall("a", "search", `{"q":"x"}`), toolCall("b", "shop_write", `{"sku":"1"}`)),
			answers("done"))
		harness.journal = &crashingJournal{MemoryLoopJournal: &MemoryLoopJournal{}, crashAt: entryNo, afterCommit: true}
		if _, err := harness.executor().Execute(context.Background(), loopInput()); err == nil {
			t.Fatalf("entry %d: want the crash", entryNo)
		}
		result, err := harness.executor().Execute(context.Background(), loopInput())
		if err != nil || string(result.State) != "done" {
			t.Fatalf("entry %d: redelivery = %s, %v", entryNo, result.State, err)
		}
		for key, count := range harness.effects {
			if count != 1 {
				t.Fatalf("entry %d: effect %s ran %d times", entryNo, key, count)
			}
		}
		if harness.modelCalls != 2 {
			t.Fatalf("entry %d: model calls = %d, want each decision made once", entryNo, harness.modelCalls)
		}
	}
}

func TestToolLoopLimitsFailClosedWithTypedErrorsAndReportSpentUsage(t *testing.T) {
	again := func(id string) domain.ModelResult { return proposes(10, toolCall(id, "search", `{"q":"same"}`)) }
	distinct := func(id string) domain.ModelResult {
		return proposes(10, toolCall(id, "search", fmt.Sprintf(`{"q":%q}`, id)))
	}
	cases := map[string]struct {
		script []domain.ModelResult
		tune   func(*loopHarness, *domain.StepInput)
		want   error
	}{
		"iterations": {script: []domain.ModelResult{distinct("a"), distinct("b"), distinct("c")},
			tune: func(h *loopHarness, _ *domain.StepInput) { h.limits.MaxIterations = 2 }, want: domain.ErrExecutionLimit},
		"step config narrows iterations": {script: []domain.ModelResult{distinct("a"), distinct("b")},
			tune: func(_ *loopHarness, input *domain.StepInput) {
				input.Step.Configuration = []byte(`{"max_iterations":1}`)
			}, want: domain.ErrExecutionLimit},
		"step config narrows total tokens": {script: []domain.ModelResult{distinct("a"), distinct("b")},
			tune: func(_ *loopHarness, input *domain.StepInput) {
				input.Step.Configuration = []byte(`{"max_tokens":30}`)
			}, want: domain.ErrBudgetExceeded},
		"tool calls": {script: []domain.ModelResult{proposes(10, toolCall("a", "search", `{"q":"1"}`), toolCall("b", "search", `{"q":"2"}`))},
			tune: func(h *loopHarness, _ *domain.StepInput) { h.limits.MaxToolCalls = 1 }, want: domain.ErrExecutionLimit},
		"tokens": {script: []domain.ModelResult{distinct("a"), distinct("b")},
			tune: func(h *loopHarness, _ *domain.StepInput) { h.limits.MaxTokens = 30 }, want: domain.ErrBudgetExceeded},
		"repeated fingerprint": {script: []domain.ModelResult{again("a"), again("b"), again("c")}, want: domain.ErrLoopDetected},
		"delegated budget": {script: []domain.ModelResult{distinct("a")},
			tune: func(h *loopHarness, input *domain.StepInput) {
				input.Bounds = domain.DelegationBounds{BudgetMinorUnits: 5, Currency: "USD"}
				h.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
					return domain.ToolResult{Output: []byte(`{}`), Usage: domain.Usage{CostMinorUnits: 9, Currency: "USD"}}, nil
				}
			}, want: domain.ErrBudgetExceeded},
		"delegated budget currency": {script: []domain.ModelResult{distinct("a")},
			tune: func(h *loopHarness, input *domain.StepInput) {
				input.Bounds = domain.DelegationBounds{BudgetMinorUnits: 50, Currency: "USD"}
				h.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
					return domain.ToolResult{Output: []byte(`{}`), Usage: domain.Usage{CostMinorUnits: 9, Currency: "EUR"}}, nil
				}
			}, want: domain.ErrValidation},
		"wall clock": {script: []domain.ModelResult{distinct("a")},
			tune: func(h *loopHarness, _ *domain.StepInput) {
				h.limits.Deadline = time.Nanosecond
				h.invoke = func(domain.ToolCall) (domain.ToolResult, error) { return domain.ToolResult{}, context.DeadlineExceeded }
			}, want: domain.ErrExecutionLimit},
		"unbound tool": {script: []domain.ModelResult{proposes(10, toolCall("a", "delete_everything", `{}`))}, want: domain.ErrInvalidModelOutput},
	}
	for name, test := range cases {
		harness := newLoopHarness(t, test.script...)
		input := loopInput()
		if test.tune != nil {
			test.tune(harness, &input)
		}
		_, err := harness.executor().Execute(context.Background(), input)
		if !errors.Is(err, test.want) {
			t.Fatalf("%s: want %v, got %v", name, test.want, err)
		}
		if name != "wall clock" && spentUsage(err).InputTokens == 0 {
			t.Fatalf("%s: a failed loop must report the usage it spent, got %+v", name, spentUsage(err))
		}
	}
}

func TestToolLoopStepConfigurationOnlyNarrowsLimits(t *testing.T) {
	harness := newLoopHarness(t, answers("done"))
	input := loopInput()
	input.Step.Configuration = []byte(`{
		"max_iterations":99,"max_tool_calls":99,"max_tokens":99999,
		"max_cost_minor_units":25,"max_repeated_calls":99,
		"deadline_seconds":999,"max_output_tokens":1000
	}`)
	if _, err := harness.executor().Execute(context.Background(), input); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if harness.lastRequest.MaxOutputTokens != 100 {
		t.Fatalf("step widened max output tokens to %d", harness.lastRequest.MaxOutputTokens)
	}

	input.Step.Configuration = []byte(`{"max_iteration":1}`)
	if _, err := harness.executor().Execute(context.Background(), input); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown configuration field must fail closed, got %v", err)
	}
}

func TestToolLoopCostBoundFailsClosedWithoutPricing(t *testing.T) {
	harness := newLoopHarness(t, answers("done"))
	executor := *harness.executor()
	executor.Pricer = nil
	input := loopInput()
	input.Bounds = domain.DelegationBounds{BudgetMinorUnits: 10, Currency: "USD"}
	if _, err := executor.Execute(context.Background(), input); !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("want ErrDegraded without pricing, got %v", err)
	}
}

func TestToolLoopFeedsARecoverableToolFailureBackAsItsClassOnly(t *testing.T) {
	harness := newLoopHarness(t, proposes(10, toolCall("a", "search", `{"q":"x"}`)), answers("recovered"))
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, fmt.Errorf("%w: dial tcp 10.0.0.8:443", domain.ErrCircuitOpen)
	}
	result, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil || string(result.State) != "recovered" {
		t.Fatalf("Execute = %s, %v", result.State, err)
	}
	observation := harness.lastRequest.Messages[1].Observations[0]
	var body map[string]string
	if !observation.IsError || json.Unmarshal(observation.Output, &body) != nil || body["error"] == "" || len(body) != 1 {
		t.Fatalf("observation = %+v", observation)
	}
	if strings.Contains(string(observation.Output), "10.0.0.8") {
		t.Fatalf("internal error text leaked to the model: %s", observation.Output)
	}
}

func TestToolLoopRequiresAResolvedPinnedPrincipalAndBoundedLimits(t *testing.T) {
	harness := newLoopHarness(t)
	input := loopInput()
	input.Principal = domain.Principal{}
	if _, err := harness.executor().Execute(context.Background(), input); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("want ErrPrincipalUnknown, got %v", err)
	}
	input = loopInput()
	input.Principal.Release = ""
	if _, err := harness.executor().Execute(context.Background(), input); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden for an unpinned principal, got %v", err)
	}
	if _, err := NewToolLoopExecutor(ToolLoopExecutor{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation for an empty composition, got %v", err)
	}
	unbounded := *harness.executor()
	unbounded.Limits.MaxIterations = 0
	if _, err := NewToolLoopExecutor(unbounded); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation for an unbounded loop, got %v", err)
	}
}

func TestLoopTrajectoryProjectsTheJournalForEvaluation(t *testing.T) {
	harness := newLoopHarness(t, proposes(10, toolCall("a", "search", `{"q":"x"}`)), answers("done"))
	if _, err := harness.executor().Execute(context.Background(), loopInput()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	journal, _ := harness.journal.Load(context.Background(), domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11})
	var kinds []domain.TrajectoryEventKind
	for index, event := range LoopTrajectory(journal) {
		if event.Sequence != int64(index+1) {
			t.Fatalf("trajectory is not sequenced: %+v", event)
		}
		kinds = append(kinds, event.Kind)
	}
	want := []domain.TrajectoryEventKind{domain.TrajectoryToolCall, domain.TrajectoryObservation, domain.TrajectoryState}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("trajectory = %v, want %v", kinds, want)
	}
}
