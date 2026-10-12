package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// limitedRuntime records what the governor was started with, the principal whose limits
// were read, and the limits each step received.
func limitedRuntime(t *testing.T, recorder *runtimeRecorder, limits domain.ExecutionLimits, readErr error) (*TurnRuntime, *domain.TenantRuntimePolicy, *domain.Principal, *[]domain.ExecutionLimits) {
	t.Helper()
	runtime := newTestRuntime(t, recorder)
	started, read, stepLimits := &domain.TenantRuntimePolicy{}, &domain.Principal{}, &[]domain.ExecutionLimits{}
	runtime.Limits = &fake.PrincipalLimits{ExecutionLimitsFunc: func(_ context.Context, principal domain.Principal) (domain.ExecutionLimits, error) {
		*read = principal
		return limits, readErr
	}}
	runtime.Governor = &fake.ExecutionGovernor{StartFunc: func(_ context.Context, _ domain.TurnRequest, policy domain.TenantRuntimePolicy) (contract.ExecutionPermit, error) {
		*started = policy
		return &fake.ExecutionPermit{}, nil
	}}
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
			*stepLimits = append(*stepLimits, input.Limits)
			return stepExecutor(recorder).Execute(ctx, input)
		}), nil
	})
	return runtime, started, read, stepLimits
}

func TestTurnRuntimeNarrowsTheTenantPolicyByTheReleaseLimits(t *testing.T) {
	limits := domain.ExecutionLimits{TurnsPerMinute: 30, MaxSteps: 3, MaxToolCalls: 2, MaxDuration: 10 * time.Second}
	runtime, started, read, stepLimits := limitedRuntime(t, &runtimeRecorder{}, limits, nil)
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("HandleTurn: %v", err)
	}
	if started.MaxSteps != 3 || started.TurnTimeout != 10*time.Second || started.MaxTokens != 1000 || started.CostCurrency != "USD" {
		t.Fatalf("governor started with %+v", *started)
	}
	// The limits are the conversation's release's, not whatever is deployed now.
	if read.Kind != domain.PrincipalAgent || read.ID != "agent" || read.TenantID != 7 || read.Release != "v1" {
		t.Fatalf("limits read for %+v", *read)
	}
	if len(*stepLimits) != 2 || (*stepLimits)[0] != limits || (*stepLimits)[1] != limits {
		t.Fatalf("steps ran under %+v", *stepLimits)
	}
}

func TestTurnRuntimeNeverWidensTheTenantPolicy(t *testing.T) {
	runtime, started, _, _ := limitedRuntime(t, &runtimeRecorder{}, domain.ExecutionLimits{MaxSteps: 50, MaxDuration: time.Hour}, nil)
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("HandleTurn: %v", err)
	}
	if started.MaxSteps != 10 || started.TurnTimeout != time.Minute {
		t.Fatalf("governor started with %+v, want the tenant ceilings", *started)
	}
}

func TestTurnRuntimeFailsClosedWhenTheReleaseLimitsCannotBeRead(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime, started, _, stepLimits := limitedRuntime(t, recorder, domain.ExecutionLimits{}, domain.ErrNotFound)
	_, err := runtime.HandleTurn(context.Background(), runtimeClaim())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want the unread limits to fail the turn", err)
	}
	if started.MaxSteps != 0 || len(*stepLimits) != 0 {
		t.Fatalf("a turn ran without its limits: policy %+v, steps %+v", *started, *stepLimits)
	}
	if len(recorder.failures) != 1 || recorder.failures[0] != string(domain.TurnFailed)+":not_found" {
		t.Fatalf("failures = %v", recorder.failures)
	}
	// The hold the turn was admitted on goes back rather than waiting out its expiry.
	if recorder.released != 1 || recorder.committed != 0 {
		t.Fatalf("released %d, settled %d, want the unused hold released once", recorder.released, recorder.committed)
	}
}

func TestTurnRuntimeRequiresPrincipalLimits(t *testing.T) {
	runtime := newTestRuntime(t, &runtimeRecorder{})
	runtime.Limits = nil
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}
