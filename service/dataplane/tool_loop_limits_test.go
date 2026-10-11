package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestToolLoopReleaseLimitsOnlyNarrowItsToolCalls(t *testing.T) {
	twoCalls := proposes(10, toolCall("a", "search", `{"q":"1"}`), toolCall("b", "search", `{"q":"2"}`))

	harness := newLoopHarness(t, twoCalls, answers("done"))
	input := loopInput()
	input.Limits = domain.ExecutionLimits{MaxToolCalls: 1}
	if _, err := harness.executor().Execute(context.Background(), input); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("error = %v, want the release's tool-call limit", err)
	}
	if len(harness.calls) != 1 {
		t.Fatalf("calls = %d, want the second refused before it left", len(harness.calls))
	}

	harness = newLoopHarness(t, twoCalls, answers("done"))
	harness.limits.MaxToolCalls = 1
	input.Limits = domain.ExecutionLimits{MaxToolCalls: 99}
	if _, err := harness.executor().Execute(context.Background(), input); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("error = %v, want a release unable to raise the executor ceiling", err)
	}
}

func TestToolLoopReleaseMaxDurationOnlyShortensTheStepDeadline(t *testing.T) {
	for name, test := range map[string]struct {
		duration, want time.Duration
	}{
		"shorter": {duration: 2 * time.Second, want: 2 * time.Second},
		"longer":  {duration: time.Hour, want: time.Minute},
	} {
		executor := *newLoopHarness(t).executor()
		var remaining time.Duration
		executor.Models = &fake.ModelGateway{GenerateFunc: func(ctx context.Context, _ domain.ModelSelection, _ domain.ModelRequest) (domain.ModelResult, error) {
			deadline, _ := ctx.Deadline()
			remaining = time.Until(deadline)
			return answers("done"), nil
		}}
		input := loopInput()
		input.Limits = domain.ExecutionLimits{MaxDuration: test.duration}
		if _, err := executor.Execute(context.Background(), input); err != nil {
			t.Fatalf("%s: Execute: %v", name, err)
		}
		if remaining <= test.want-time.Second || remaining > test.want {
			t.Fatalf("%s: step deadline in %s, want about %s", name, remaining, test.want)
		}
	}
}
