package evaluation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func releaseLimits(t *testing.T, limits domain.ExecutionLimits) *fake.PrincipalLimits {
	return &fake.PrincipalLimits{ExecutionLimitsFunc: func(_ context.Context, principal domain.Principal) (domain.ExecutionLimits, error) {
		if principal.Kind != domain.PrincipalAgent || principal.ID != "wingmate" || principal.TenantID != 7 || principal.Release != "v2" {
			t.Fatalf("limits read for %+v", principal)
		}
		return limits, nil
	}}
}

// chainGraph is six tool-loop steps run one after another.
func chainGraph() fake.DefinitionResolverFunc {
	return func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
		graph := domain.ExecutionGraph{AgentID: "wingmate", Version: "v2", EntryStepID: "s1"}
		for i := 1; i <= 6; i++ {
			step := domain.ExecutionStep{ExecutionStepID: int64(i), StepID: fmt.Sprintf("s%d", i), Kind: domain.StepKindToolLoop}
			if i < 6 {
				step.Configuration = []byte(fmt.Sprintf(`{"next_step_id":"s%d"}`, i+1))
			}
			graph.Steps = append(graph.Steps, step)
		}
		return graph, nil
	}
}

func lastReplayEvent(t *testing.T, evalCase domain.EvaluationCase) domain.TrajectoryEvent {
	t.Helper()
	if len(evalCase.Trajectory) == 0 {
		t.Fatal("empty trajectory")
	}
	return evalCase.Trajectory[len(evalCase.Trajectory)-1]
}

func TestReleaseCaseExecutorNarrowsToolCallsToTheReleaseLimits(t *testing.T) {
	harness := newReplayHarness(t, searchCall(), searchCall(), domain.ModelResult{Output: []byte("done")})
	harness.executor.ReleaseLimits = releaseLimits(t, domain.ExecutionLimits{MaxToolCalls: 1})
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatalf("a release limit reached is a scored outcome: %v", err)
	}
	if final := lastReplayEvent(t, evalCase); final.Name != "execution_limit" || !final.Failed {
		t.Fatalf("final state = %+v", final)
	}
	if calls := len(harness.sandbox.Recorded()); calls != 1 {
		t.Fatalf("sandbox calls = %d, want the release's one", calls)
	}
}

func TestReleaseCaseExecutorNeverWidensItsToolCallsByTheRelease(t *testing.T) {
	harness := newReplayHarness(t, searchCall(), searchCall(), domain.ModelResult{Output: []byte("done")})
	harness.executor.Limits.MaxToolCalls = 1
	harness.executor.ReleaseLimits = releaseLimits(t, domain.ExecutionLimits{MaxToolCalls: 50})
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if final := lastReplayEvent(t, evalCase); final.Name != "execution_limit" || len(harness.sandbox.Recorded()) != 1 {
		t.Fatalf("final state = %+v after %d calls; the executor's ceiling must hold", final, len(harness.sandbox.Recorded()))
	}
}

func TestReleaseCaseExecutorNarrowsGraphStepsToTheReleaseLimits(t *testing.T) {
	cases := []struct {
		name                   string
		executorSteps, release int
		wantModelCalls         int
	}{
		{"release narrows", 5, 2, 2},
		{"release cannot widen", 2, 10, 2},
		{"release binds none", 3, 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := make([]domain.ModelResult, 6)
			for i := range script {
				script[i] = domain.ModelResult{Output: []byte("again")}
			}
			harness := newReplayHarness(t, script...)
			harness.executor.Graphs = chainGraph()
			harness.executor.MaxSteps = tc.executorSteps
			harness.executor.ReleaseLimits = releaseLimits(t, domain.ExecutionLimits{MaxSteps: tc.release})
			evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if final := lastReplayEvent(t, evalCase); final.Name != "execution_limit" || len(harness.requests) != tc.wantModelCalls {
				t.Fatalf("final %+v after %d model calls, want %d", final, len(harness.requests), tc.wantModelCalls)
			}
		})
	}
}

func TestReleaseCaseExecutorNarrowsTheLoopDeadlineToTheReleaseDuration(t *testing.T) {
	cases := []struct {
		name    string
		release time.Duration
		want    time.Duration
	}{
		{"release narrows", 5 * time.Second, 5 * time.Second},
		{"release cannot widen", time.Hour, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newReplayHarness(t)
			var remaining time.Duration
			harness.executor.Models = &fake.ModelGateway{GenerateFunc: func(ctx context.Context, _ domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("the model call has no deadline")
				}
				remaining = time.Until(deadline)
				return domain.ModelResult{Output: []byte("done")}, nil
			}}
			harness.executor.ReleaseLimits = releaseLimits(t, domain.ExecutionLimits{MaxDuration: tc.release})
			if _, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil); err != nil {
				t.Fatal(err)
			}
			if remaining <= 0 || remaining > tc.want {
				t.Fatalf("model call deadline in %s, want at most %s", remaining, tc.want)
			}
		})
	}
}

func TestReleaseCaseExecutorScoresARunOutReleaseDuration(t *testing.T) {
	harness := newReplayHarness(t)
	harness.executor.Models = &fake.ModelGateway{GenerateFunc: func(ctx context.Context, _ domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
		<-ctx.Done()
		return domain.ModelResult{}, ctx.Err()
	}}
	harness.executor.ReleaseLimits = releaseLimits(t, domain.ExecutionLimits{MaxDuration: 20 * time.Millisecond})
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatalf("running out of the release's duration is a scored outcome: %v", err)
	}
	if final := lastReplayEvent(t, evalCase); final.Name != "deadline" || !final.Failed {
		t.Fatalf("final state = %+v", final)
	}
}

func TestReleaseCaseExecutorStopsAtTheReleaseDurationBetweenSteps(t *testing.T) {
	script := make([]domain.ModelResult, 5)
	for i := range script {
		script[i] = domain.ModelResult{Output: []byte("again")}
	}
	harness := newReplayHarness(t, script...)
	harness.executor.Graphs = chainGraph()
	harness.executor.MaxSteps = 5
	clock := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	harness.executor.Now = func() time.Time {
		clock = clock.Add(10 * time.Second)
		return clock
	}
	harness.executor.ReleaseLimits = releaseLimits(t, domain.ExecutionLimits{MaxDuration: 25 * time.Second})
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if final := lastReplayEvent(t, evalCase); final.Name != "execution_limit" || len(harness.requests) >= 5 {
		t.Fatalf("final %+v after %d model calls; the replay must stop at its duration", final, len(harness.requests))
	}
}

type ctxGuardrail struct {
	contract.GuardrailEnforcer
	after func(context.Context) (domain.ModelChunk, error)
}

func (guardrail ctxGuardrail) AfterModelChunk(ctx context.Context, _ domain.GuardrailConfig, _ domain.GuardrailSubject, _ domain.ModelChunk) (domain.ModelChunk, error) {
	return guardrail.after(ctx)
}

func TestReleaseCaseExecutorScoresGuardrailsRunningPastTheReleaseDuration(t *testing.T) {
	harness := newReplayHarness(t, domain.ModelResult{Output: []byte("done")})
	harness.executor.GuardrailConfigs = guardrailConfigs{}
	harness.executor.Guardrails = ctxGuardrail{after: func(ctx context.Context) (domain.ModelChunk, error) {
		<-ctx.Done()
		return domain.ModelChunk{}, ctx.Err()
	}}
	harness.executor.ReleaseLimits = releaseLimits(t, domain.ExecutionLimits{MaxDuration: 20 * time.Millisecond})
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatalf("guardrails running out the release's duration are a scored outcome: %v", err)
	}
	if final := lastReplayEvent(t, evalCase); final.Name != "deadline" || !final.Failed {
		t.Fatalf("final state = %+v", final)
	}
}

func TestReleaseCaseExecutorReturnsTheCallersCancellation(t *testing.T) {
	harness := newReplayHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	harness.executor.Models = &fake.ModelGateway{GenerateFunc: func(ctx context.Context, _ domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
		cancel()
		return domain.ModelResult{}, ctx.Err()
	}}
	harness.executor.ReleaseLimits = releaseLimits(t, domain.ExecutionLimits{MaxDuration: time.Minute})
	if _, err := harness.executor.Execute(ctx, replaySubject(), replayExample(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's cancellation, not a scored outcome", err)
	}
}

func TestReleaseCaseExecutorFailsTheCaseClosedWithoutReleaseLimits(t *testing.T) {
	cases := map[string]contract.PrincipalLimits{
		"unreadable": &fake.PrincipalLimits{ExecutionLimitsFunc: func(context.Context, domain.Principal) (domain.ExecutionLimits, error) {
			return domain.ExecutionLimits{}, domain.ErrNotFound
		}},
		"malformed": &fake.PrincipalLimits{ExecutionLimitsFunc: func(context.Context, domain.Principal) (domain.ExecutionLimits, error) {
			return domain.ExecutionLimits{}, domain.ErrValidation
		}},
	}
	for name, limits := range cases {
		t.Run(name, func(t *testing.T) {
			harness := newReplayHarness(t)
			sandboxes := 0
			harness.executor.Sandboxes = func(context.Context, domain.GoldenExample) (contract.ToolSandbox, error) {
				sandboxes++
				return harness.sandbox, nil
			}
			harness.executor.ReleaseLimits = limits
			if _, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil); err == nil {
				t.Fatal("a release whose limits cannot be read must fail the case")
			}
			if sandboxes != 0 || len(harness.requests) != 0 {
				t.Fatalf("the replay ran: sandboxes %d model calls %d", sandboxes, len(harness.requests))
			}
		})
	}
	t.Run("not composed", func(t *testing.T) {
		harness := newReplayHarness(t)
		harness.executor.ReleaseLimits = nil
		if _, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil); err == nil {
			t.Fatal("an executor without release limits must refuse")
		}
	})
}

// A release's per-turn budget stops a replay as it stops a live turn; the stop is a scored outcome.
func TestReleaseCaseExecutorStopsAtTheReleasesTurnBudget(t *testing.T) {
	answer := domain.ModelResult{Output: []byte("done"), Usage: domain.Usage{InputTokens: 10, OutputTokens: 4}}
	harness := newReplayHarness(t, answer, answer, answer, answer, answer, answer)
	harness.executor.Graphs = chainGraph()
	limits := releaseLimits(t, domain.ExecutionLimits{})
	limits.BudgetFunc = func(context.Context, domain.Principal) (domain.ReleaseBudget, error) {
		return domain.ReleaseBudget{Tokens: 20, Period: domain.BudgetPeriod{Kind: domain.BudgetPerTurn}}, nil
	}
	harness.executor.ReleaseLimits = limits
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatalf("a spent turn budget is a scored outcome: %v", err)
	}
	final := lastReplayEvent(t, evalCase)
	if !final.Failed || len(harness.requests) >= 6 {
		t.Fatalf("final state = %+v after %d model calls; the 20-token turn budget must stop the replay", final, len(harness.requests))
	}
}
